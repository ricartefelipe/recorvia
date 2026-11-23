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
// fim do WAL arquivado (Latest), a partir do backup base adequado ou de um
// backup indicado pelo nome.
type RestoreOptions struct {
	Target     time.Time
	Latest     bool
	BackupName string
}

// recoveryMarker delimita, no postgresql.auto.conf, os parâmetros escritos
// pela restauração (removidos depois da promoção com ALTER SYSTEM RESET).
const recoveryMarker = "# recorvia-pgbackup restore"

// PrepareRestore prepara um servidor novo para recuperar até o ponto pedido:
// confere que o diretório de dados está vazio, escolhe o backup base, confere
// a impressão digital da chave de cifragem, extrai o backup e grava
// recovery.signal e o alvo da recuperação. Ao iniciar, o PostgreSQL busca o
// WAL com o restore_command e é promovido ao alcançar o alvo. Se a extração
// falhar, o diretório de dados é esvaziado.
func PrepareRestore(ctx context.Context, r Runner, cfg Config, opts RestoreOptions, log *slog.Logger) (Backup, error) {
	if opts.Latest == !opts.Target.IsZero() {
		return Backup{}, errors.New("informe o instante da restauração (--target-time) ou --latest")
	}
	if cfg.PGData == "" {
		return Backup{}, errors.New("PGDATA não definido")
	}
	if err := requireEmptyDir(cfg.PGData); err != nil {
		return Backup{}, err
	}
	list, err := ListBackups(ctx, r)
	if err != nil {
		return Backup{}, fmt.Errorf("listagem dos backups: %w", err)
	}
	var chosen Backup
	if opts.BackupName != "" {
		found := false
		for _, b := range list {
			if b.Name == opts.BackupName {
				chosen, found = b, true
			}
		}
		if !found {
			return Backup{}, fmt.Errorf("backup base %s não encontrado no armazenamento", opts.BackupName)
		}
		if !opts.Target.IsZero() && !chosen.FinishTime.Before(opts.Target) {
			return Backup{}, fmt.Errorf("o backup base %s terminou em %s, depois do instante pedido", chosen.Name, chosen.FinishTime.UTC().Format(time.RFC3339))
		}
	} else if chosen, err = ForTarget(list, opts.Target); err != nil {
		return Backup{}, err
	}
	switch fp := chosen.Fingerprint(); {
	case fp == "":
		log.Warn("backup base sem a impressão digital da chave; a chave só é conferida na extração", slog.String("backup", chosen.Name))
	case fp != cfg.KeyFingerprint():
		return Backup{}, fmt.Errorf("chave de cifragem do backup diferente da usada no backup base %s (impressão digital do backup %s, da chave informada %s): nada foi baixado",
			chosen.Name, fp, cfg.KeyFingerprint())
	}
	log.Info("extraindo o backup base", slog.String("backup", chosen.Name), slog.Time("finish_time", chosen.FinishTime))
	if err := FetchBackup(ctx, r, cfg.PGData, chosen.Name); err != nil {
		if cerr := emptyDir(cfg.PGData); cerr != nil {
			log.Error("não foi possível esvaziar o diretório de dados", slog.String("error", cerr.Error()))
		}
		return Backup{}, fmt.Errorf("extração do backup base %s falhou (chave de cifragem errada, armazenamento inacessível ou arquivo corrompido); o diretório de dados foi esvaziado: %w", chosen.Name, err)
	}
	if err := writeRecoveryConfig(cfg.PGData, opts.Target); err != nil {
		return Backup{}, err
	}
	return chosen, nil
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

// writeRecoveryConfig grava recovery.signal e, com instante, o alvo e a
// promoção ao alcançá-lo. Sem instante, a recuperação vai até o fim do WAL
// arquivado e o servidor é promovido.
func writeRecoveryConfig(pgdata string, target time.Time) error {
	if err := os.WriteFile(filepath.Join(pgdata, "recovery.signal"), nil, 0o600); err != nil {
		return err
	}
	if target.IsZero() {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(pgdata, "postgresql.auto.conf"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // PGDATA da configuração
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "\n%s (removido depois da promoção)\nrecovery_target_time = '%s'\nrecovery_target_action = 'promote'\n",
		recoveryMarker, target.UTC().Format("2006-01-02 15:04:05.999999-07"))
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
