package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

// KeyringCheckCommand é o subcomando da API e do worker que confere o
// keyring contra o banco e termina (por exemplo, antes de religar a
// aplicação sobre um banco restaurado).
const KeyringCheckCommand = "keyring-check"

// keyringCheckTimeout limita a conferência do keyring (a leitura percorre as
// organizações uma a uma).
const keyringCheckTimeout = 5 * time.Minute

// KeyringAuditor confere o keyring contra os dados cifrados gravados.
type KeyringAuditor interface {
	AuditKeyring(ctx context.Context) (pgstore.KeyringAudit, error)
}

// VerifyKeyring confere, na inicialização, se o keyring configurado decifra
// os dados cifrados em uso no banco (ADR-063). No modo enforce, um keyring
// incompatível impede a inicialização: com a chave errada ou ausente, o
// worker encerraria notificações pendentes e entregas de webhook sem volta e
// as chamadas ao provedor falhariam. No modo warn, o problema é registrado e
// a aplicação inicia, inclusive quando a própria conferência falha (leitura
// do banco ou prazo esgotado). No modo off, nada é conferido.
//
// Não impedem a inicialização em nenhum modo, e ficam registrados: dados de
// chaves declaradas perdidas (RECORVIA_ENCRYPTION_LOST_KEYS), registros que
// não decifram com uma chave comprovada por outros registros (dado
// corrompido, que nenhuma correção do keyring resolve) e dados fora de uso.
func VerifyKeyring(ctx context.Context, a KeyringAuditor, mode config.KeyringCheckMode, log *slog.Logger) error {
	if mode == config.KeyringCheckOff {
		log.WarnContext(ctx, "conferência do keyring desligada (RECORVIA_ENCRYPTION_STARTUP_CHECK=off)")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, keyringCheckTimeout)
	defer cancel()
	audit, err := a.AuditKeyring(ctx)
	if err != nil {
		if mode == config.KeyringCheckWarn {
			log.ErrorContext(ctx, "conferência do keyring não concluída; iniciando assim mesmo (RECORVIA_ENCRYPTION_STARTUP_CHECK=warn): "+
				"o keyring não foi conferido contra o banco", slog.String("error", err.Error()))
			return nil
		}
		return fmt.Errorf("conferência do keyring: %w", err)
	}
	for _, u := range audit.Usages {
		p := u.Problem()
		switch {
		case p == "" || u.Blocking():
			// Sem problema, ou problema do keyring (tratado abaixo, com o modo).
		case u.Corrupted():
			log.ErrorContext(ctx, "registros em uso que não decifram com uma chave comprovada por outros registros (dado corrompido ou "+
				"copiado de outro contexto): não é problema do keyring; a aplicação inicia e esses registros falham item a item",
				slog.String("problem", p))
		case u.LostInUse():
			log.WarnContext(ctx, "dados em uso cifrados com chave declarada perdida (RECORVIA_ENCRYPTION_LOST_KEYS): ficam ilegíveis "+
				"até serem substituídos (conta de provedor configurada de novo, segredo de webhook rotacionado)", slog.String("problem", p))
		default:
			log.WarnContext(ctx, "dado cifrado fora de uso que o keyring não decifra", slog.String("problem", p))
		}
	}
	mismatch := audit.Err()
	switch {
	case mismatch == nil:
		log.InfoContext(ctx, "keyring conferido com o banco", slog.Int("tenants", audit.Tenants),
			slog.String("configured_keys", strings.Join(audit.ConfiguredKeys, ",")), slog.String("active_key", audit.ActiveKey),
			slog.String("lost_keys", strings.Join(audit.LostKeys, ",")))
		return nil
	case mode == config.KeyringCheckWarn:
		log.ErrorContext(ctx, "keyring incompatível com o banco; iniciando assim mesmo (RECORVIA_ENCRYPTION_STARTUP_CHECK=warn): "+
			"notificações pendentes e entregas de webhook que dependem das chaves ausentes serão encerradas, e as contas de provedor "+
			"afetadas precisam ser configuradas de novo", slog.String("error", mismatch.Error()))
		return nil
	default:
		return fmt.Errorf("%w. A aplicação não foi iniciada: confira RECORVIA_ENCRYPTION_KEYS (todas as chaves usadas na cifragem dos "+
			"dados, com os mesmos identificadores e valores). Se a chave foi perdida, declare-a em RECORVIA_ENCRYPTION_LOST_KEYS: "+
			"veja docs/operacao/backup-e-restauracao.md", mismatch)
	}
}

// WriteKeyringReport escreve o resultado da conferência em formato de tabela,
// sem nenhum conteúdo decifrado.
func WriteKeyringReport(w io.Writer, audit pgstore.KeyringAudit) error {
	lost := ""
	if len(audit.LostKeys) > 0 {
		lost = "; declaradas perdidas: " + strings.Join(audit.LostKeys, ", ")
	}
	if _, err := fmt.Fprintf(w, "chaves configuradas: %s (ativa: %s)%s; organizações conferidas: %d\n",
		strings.Join(audit.ConfiguredKeys, ", "), audit.ActiveKey, lost, audit.Tenants); err != nil {
		return err
	}
	if len(audit.Usages) == 0 {
		_, err := fmt.Fprintln(w, "nenhum dado cifrado em uso encontrado no banco")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "FINALIDADE\tCHAVE\tREGISTROS\tEM USO\tNÃO DECIFRAM\tRESULTADO"); err != nil {
		return err
	}
	for _, u := range audit.Usages {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", u.Purpose, u.KeyID, u.Total, u.InUse, u.Failed, u.Result()); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// RunKeyringCheck executa o subcomando keyring-check: abre o banco com a
// configuração do processo, confere o keyring, escreve o relatório em out e
// devolve erro quando algum dado em uso não decifra por engano do keyring
// (chave ausente ou com outro valor). Não altera dados.
func RunKeyringCheck(ctx context.Context, getenv config.Getenv, out io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateStore(); err != nil {
		return err
	}
	// O subcomando não emite logs estruturados: a saída fica reservada ao
	// relatório e o erro, a quem o chamou.
	log := logging.New(io.Discard, cfg.LogLevel, "keyring-check")
	core, err := Open(ctx, cfg, log, "recorvia-keyring-check")
	if err != nil {
		return err
	}
	defer core.Close()
	return checkKeyring(ctx, core.Store, out)
}

func checkKeyring(ctx context.Context, a KeyringAuditor, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, keyringCheckTimeout)
	defer cancel()
	audit, err := a.AuditKeyring(ctx)
	if err != nil {
		return fmt.Errorf("conferência do keyring: %w", err)
	}
	if err := WriteKeyringReport(out, audit); err != nil {
		return err
	}
	if err := audit.Err(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "keyring compatível com o banco"); err != nil {
		return err
	}
	// O que não é engano do keyring não muda o código de saída, mas fica à
	// vista de quem opera.
	for _, p := range audit.LostProblems() {
		if _, err := fmt.Fprintln(out, "atenção:", p+"; os dados ficam ilegíveis até serem substituídos"); err != nil {
			return err
		}
	}
	for _, p := range audit.CorruptedProblems() {
		if _, err := fmt.Fprintln(out, "atenção:", p+"; não é problema do keyring: trate como incidente de integridade"); err != nil {
			return err
		}
	}
	return nil
}
