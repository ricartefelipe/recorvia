package pgbackup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RestoreOptions escolhe o ponto da restauração: um instante (Target) ou o
// fim do WAL arquivado (Latest), numa linha do tempo (Timeline; zero: a mais
// recente do arquivo), a partir do backup base adequado ou de um backup
// indicado pelo nome.
type RestoreOptions struct {
	Target     time.Time
	Latest     bool
	BackupName string
	Timeline   uint32
}

// RestorePlan é o que a restauração preparou: o backup base extraído, a linha
// do tempo seguida na recuperação e a mais recente do arquivo.
type RestorePlan struct {
	Backup         Backup
	Timeline       uint32
	LatestTimeline uint32
}

// recoveryMarker delimita, no postgresql.auto.conf, os parâmetros escritos
// pela restauração (removidos depois da promoção com ALTER SYSTEM RESET).
const recoveryMarker = "# recorvia-pgbackup restore"

// PrepareRestore prepara um servidor novo para recuperar até o ponto pedido:
// confere que o diretório de dados está vazio, escolhe a linha do tempo e o
// backup base do histórico dela, confere a impressão digital da chave de
// cifragem, extrai o backup e grava recovery.signal, a linha do tempo e o
// alvo da recuperação. Ao iniciar, o PostgreSQL busca o WAL com o
// restore_command e é promovido ao alcançar o alvo. Se a extração falhar, o
// diretório de dados é esvaziado.
func PrepareRestore(ctx context.Context, r Runner, cfg Config, opts RestoreOptions, log *slog.Logger) (RestorePlan, error) {
	if opts.Latest == !opts.Target.IsZero() {
		return RestorePlan{}, errors.New("informe o instante da restauração (--target-time) ou --latest")
	}
	if cfg.PGData == "" {
		return RestorePlan{}, errors.New("PGDATA não definido")
	}
	if err := requireEmptyDir(cfg.PGData); err != nil {
		return RestorePlan{}, err
	}
	list, err := ListBackups(ctx, r)
	if err != nil {
		return RestorePlan{}, fmt.Errorf("listagem dos backups: %w", err)
	}
	// A chave é conferida antes de ler qualquer arquivo cifrado (o histórico
	// das linhas do tempo): sem nenhum backup com a impressão digital dela, a
	// chave é de outro destino ou foi trocada.
	if err := requireKnownKey(list, cfg.KeyFingerprint()); err != nil {
		return RestorePlan{}, err
	}
	plan := RestorePlan{Timeline: opts.Timeline}
	if plan.LatestTimeline, err = LatestTimeline(ctx, r); err != nil {
		return RestorePlan{}, err
	}
	switch {
	case plan.Timeline == 0:
		plan.Timeline = plan.LatestTimeline
		if plan.LatestTimeline > 1 {
			log.Warn("o arquivo tem mais de uma linha do tempo; a recuperação segue a mais recente (para outra, use --timeline)",
				slog.Uint64("timeline", uint64(plan.Timeline)))
		}
	case plan.Timeline > plan.LatestTimeline:
		return RestorePlan{}, fmt.Errorf("linha do tempo %d não existe no arquivo (a mais recente é a %d)", plan.Timeline, plan.LatestTimeline)
	case plan.Timeline < plan.LatestTimeline:
		log.Warn("há linhas do tempo mais novas no arquivo; a recuperação segue a pedida",
			slog.Uint64("timeline", uint64(plan.Timeline)), slog.Uint64("latest_timeline", uint64(plan.LatestTimeline)))
	}
	history, err := ReadAncestry(ctx, r, plan.Timeline)
	if err != nil {
		return RestorePlan{}, err
	}
	chosen, err := chooseBackup(list, opts, history)
	if err != nil {
		return RestorePlan{}, err
	}
	plan.Backup = chosen
	switch fp := chosen.Fingerprint(); {
	case fp == "":
		log.Warn("backup base sem a impressão digital da chave; a chave só é conferida na extração", slog.String("backup", chosen.Name))
	case fp != cfg.KeyFingerprint():
		return RestorePlan{}, fmt.Errorf("chave de cifragem do backup diferente da usada no backup base %s (impressão digital do backup %s, da chave informada %s): nada foi baixado",
			chosen.Name, fp, cfg.KeyFingerprint())
	}
	log.Info("extraindo o backup base", slog.String("backup", chosen.Name), slog.Time("finish_time", chosen.FinishTime),
		slog.Uint64("timeline", uint64(plan.Timeline)))
	if err := FetchBackup(ctx, r, cfg.PGData, chosen.Name); err != nil {
		if cerr := emptyDir(cfg.PGData); cerr != nil {
			log.Error("não foi possível esvaziar o diretório de dados", slog.String("error", cerr.Error()))
		}
		return RestorePlan{}, fmt.Errorf("extração do backup base %s falhou (chave de cifragem errada, armazenamento inacessível ou arquivo corrompido); o diretório de dados foi esvaziado: %w", chosen.Name, err)
	}
	if err := writeRecoveryConfig(cfg.PGData, opts.Target, plan.Timeline); err != nil {
		return RestorePlan{}, err
	}
	return plan, nil
}

// requireKnownKey recusa a chave quando há backups com impressão digital e
// nenhum deles foi feito com ela.
func requireKnownKey(list []Backup, fingerprint string) error {
	var latest *Backup
	for i := range list {
		switch fp := list[i].Fingerprint(); {
		case fp == fingerprint:
			return nil
		case fp != "":
			latest = &list[i]
		}
	}
	if latest == nil {
		return nil
	}
	return fmt.Errorf("chave de cifragem do backup diferente da usada no backup base %s (impressão digital do backup %s, da chave informada %s): nada foi baixado",
		latest.Name, latest.Fingerprint(), fingerprint)
}

// chooseBackup escolhe o backup base: o indicado pelo nome (que precisa estar
// no histórico da linha alvo e ter terminado antes do instante) ou o mais
// recente do histórico terminado antes do instante.
func chooseBackup(list []Backup, opts RestoreOptions, history Ancestry) (Backup, error) {
	if opts.BackupName == "" {
		b, err := ForTarget(list, opts.Target, history)
		if errors.Is(err, ErrNoBackupForTarget) {
			return Backup{}, fmt.Errorf("%w no histórico da linha do tempo %d; os backups de linhas abandonadas depois de uma restauração não servem a ela: informe --timeline ou um backup com --backup (recorvia-pgbackup list)",
				err, history.Target)
		}
		return b, err
	}
	for _, b := range list {
		if b.Name != opts.BackupName {
			continue
		}
		if !history.Contains(b) {
			return Backup{}, fmt.Errorf("o backup base %s (linha do tempo %d) não está no histórico da linha do tempo %d: informe outra linha com --timeline ou outro backup",
				b.Name, b.Timeline(), history.Target)
		}
		if !opts.Target.IsZero() && !b.FinishTime.Before(opts.Target) {
			return Backup{}, fmt.Errorf("o backup base %s terminou em %s, depois do instante pedido", b.Name, b.FinishTime.UTC().Format(time.RFC3339))
		}
		return b, nil
	}
	return Backup{}, fmt.Errorf("backup base %s não encontrado no armazenamento", opts.BackupName)
}

// requireEmptyDir cria o diretório de dados (0700) se não existe e recusa um
// diretório com conteúdo: a restauração nunca sobrescreve um servidor.
func requireEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("o diretório de dados %s não está vazio: a restauração só é feita num servidor novo (volume vazio); nada foi alterado", dir)
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // o PostgreSQL exige 0700 no diretório de dados
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		errs = append(errs, os.RemoveAll(filepath.Join(dir, e.Name())))
	}
	return errors.Join(errs...)
}

// writeRecoveryConfig grava recovery.signal, a linha do tempo seguida e, com
// instante, o alvo e a promoção ao alcançá-lo. Sem instante, a recuperação
// vai até o fim do WAL arquivado da linha e o servidor é promovido. A linha
// do tempo é sempre explícita: o padrão do PostgreSQL ("latest") seguiria a
// linha mais recente do arquivo, que depois de uma restauração anterior pode
// não ser a pedida.
func writeRecoveryConfig(pgdata string, target time.Time, timeline uint32) error {
	if err := os.WriteFile(filepath.Join(pgdata, "recovery.signal"), nil, 0o600); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(pgdata, "postgresql.auto.conf"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // PGDATA da configuração
	if err != nil {
		return err
	}
	conf := fmt.Sprintf("\n%s (removido depois da promoção)\nrecovery_target_timeline = '%d'\n", recoveryMarker, timeline)
	if !target.IsZero() {
		conf += fmt.Sprintf("recovery_target_time = '%s'\nrecovery_target_action = 'promote'\n", target.UTC().Format("2006-01-02 15:04:05.999999-07"))
	}
	_, werr := io.WriteString(f, conf)
	return errors.Join(werr, f.Close())
}

// Códigos de saída do restore_command: o PostgreSQL trata qualquer código
// diferente de zero como "arquivo ausente" (fim do WAL disponível, e a
// recuperação termina) e códigos acima de 125 como erro fatal.
const (
	FetchOK       = 0
	FetchNotFound = 1
	FetchFatal    = 126
	// walgNotFound é o código do wal-g wal-fetch para arquivo inexistente
	// no armazenamento (EX_IOERR).
	walgNotFound = 74
)

// FetchExitCode traduz o resultado do wal-g wal-fetch para o restore_command.
// Só a ausência do arquivo encerra a recuperação normalmente; erros de
// leitura, de rede ou de decifragem a interrompem, para que o servidor não
// seja promovido antes do ponto pedido por um WAL que existe mas não pôde
// ser lido.
func FetchExitCode(err error) int {
	if err == nil {
		return FetchOK
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == walgNotFound {
		return FetchNotFound
	}
	return FetchFatal
}

// WALFetch busca um arquivo do WAL (restore_command) e devolve o código de
// saída para o PostgreSQL.
func WALFetch(ctx context.Context, r Runner, name, path string, stderr io.Writer) int {
	err := r.Run(ctx, io.Discard, "wal-fetch", name, path)
	code := FetchExitCode(err)
	if code == FetchFatal {
		_, _ = fmt.Fprintf(stderr, "recorvia-pgbackup: erro ao buscar %s no armazenamento (a recuperação é interrompida): %v\n", name, err)
	}
	return code
}

// ParseTarget lê o instante da restauração (RFC 3339, por exemplo
// 2026-11-22T10:15:00Z ou 2026-11-22T07:15:00-03:00).
func ParseTarget(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, fmt.Errorf("instante inválido %q: use RFC 3339 com fuso, por exemplo 2026-11-22T10:15:00Z", s)
	}
	return t.UTC(), nil
}
