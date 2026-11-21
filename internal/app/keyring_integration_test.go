//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// O subcomando keyring-check e a conferência da inicialização, sobre um banco
// real com dados cifrados pelos serviços (conta de provedor e endpoint de
// webhook com uma entrega pendente), como depois de uma restauração: com as
// chaves certas o keyring é aceito; com a chave ausente ou com outro valor, o
// subcomando e a inicialização falham com a chave e a finalidade no erro, sem
// alterar o banco.
func TestKeyringCheckAgainstRestoredDatabase(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	iso := pgtest.Isolated(t)
	env := fixture.NewOn(t, pgtest.AppDBOf(t, iso, 8), start)
	owner, err := pgxpool.New(ctx, iso.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)

	cfg, err := config.Load(func(k string) string {
		return map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_WEBHOOK_ALLOWED_PRIVATE_CIDRS": "127.0.0.1/32"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := Assemble(cfg, log, env.DB, env.Keyring, env.Clock)
	if err != nil {
		t.Fatal(err)
	}
	tn := env.NewTenant(t, "restaurado")
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	err = env.Store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
		return tx.SaveAccount(ctx, payments.ProviderAccount{ID: ids.New(), TenantID: tn.ID, Provider: "simulator", Environment: "local",
			Status: "active", IsDefault: true, APIKey: "sim_restaurado_0001", APIKeyHint: "0001", Secret: strings.Repeat("s", 40),
			CreatedAt: start, UpdatedAt: start, Version: 1}, []byte("hash-do-token"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, _, err := core.Webhooks.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: "http://127.0.0.1:9/hooks",
		EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := core.Webhooks.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, start, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	deliveryState := func() string {
		t.Helper()
		var status string
		var attempts int
		if err := owner.QueryRow(ctx, `SELECT status, attempts FROM webhook_deliveries WHERE tenant_id = $1 AND endpoint_id = $2`,
			tn.ID, endpoint.ID).Scan(&status, &attempts); err != nil {
			t.Fatal(err)
		}
		return status + "/" + string(rune('0'+attempts))
	}
	if got := deliveryState(); got != "pending/0" {
		t.Fatalf("entrega antes da conferência: %s", got)
	}

	b64 := func(b byte) string { return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32)) }
	run := func(keys, active string) (string, error) {
		t.Helper()
		vars := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": iso.AppURL,
			"RECORVIA_ENCRYPTION_KEYS": keys, "RECORVIA_ENCRYPTION_ACTIVE_KEY": active}
		var out bytes.Buffer
		err := RunKeyringCheck(ctx, func(k string) string { return vars[k] }, &out)
		return out.String(), err
	}

	t.Run("subcomando com as chaves guardadas", func(t *testing.T) {
		// A chave t1 (bytes 7) é a do ambiente de testes; t2 é a chave ativa
		// de uma rotação posterior, ainda sem dados.
		out, err := run("t1:"+b64(7)+",t2:"+b64(8), "t2")
		if err != nil {
			t.Fatalf("keyring-check com as chaves certas: %v\n%s", err, out)
		}
		for _, frag := range []string{"chaves configuradas: t1, t2 (ativa: t2); organizações conferidas: 1",
			pgstore.KeyringPurposeProviderAccount, pgstore.KeyringPurposeWebhookSigning, "keyring compatível com o banco"} {
			if !strings.Contains(out, frag) {
				t.Errorf("relatório sem %q:\n%s", frag, out)
			}
		}
		if strings.Contains(out, "sim_restaurado_0001") || strings.Contains(out, "whsec_") {
			t.Fatalf("o relatório não pode trazer segredos:\n%s", out)
		}
	})
	t.Run("subcomando com a chave ausente", func(t *testing.T) {
		out, err := run("t2:"+b64(8), "t2")
		if !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), `chave "t1" ausente do keyring`) {
			t.Fatalf("erro = %v", err)
		}
		if !strings.Contains(out, "chave ausente") || strings.Contains(out, "keyring compatível") {
			t.Fatalf("relatório inesperado:\n%s", out)
		}
	})
	t.Run("subcomando com outro valor para a chave", func(t *testing.T) {
		out, err := run("t1:"+b64(99), "t1")
		if !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), `chave "t1" com valor diferente do usado na cifragem`) {
			t.Fatalf("erro = %v", err)
		}
		if !strings.Contains(out, "chave com outro valor") {
			t.Fatalf("relatório inesperado:\n%s", out)
		}
	})
	t.Run("subcomando sem acesso ao banco", func(t *testing.T) {
		u, err := url.Parse(iso.AppURL)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.UserPassword(u.User.Username(), "senha-incorreta")
		vars := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": u.String(),
			"RECORVIA_ENCRYPTION_KEYS": "t1:" + b64(7), "RECORVIA_ENCRYPTION_ACTIVE_KEY": "t1"}
		if err := RunKeyringCheck(ctx, func(k string) string { return vars[k] }, io.Discard); err == nil || errors.Is(err, secrets.ErrKeyringMismatch) {
			t.Fatalf("erro = %v, esperado falha de conexão (não de keyring)", err)
		}
	})

	wrong, err := secrets.NewKeyring(map[string][]byte{"t1": bytes.Repeat([]byte{99}, 32)}, "t1")
	if err != nil {
		t.Fatal(err)
	}
	wrongStore := pgstore.New(env.DB, wrong, env.Clock, nil)
	t.Run("inicialização recusada com a chave errada", func(t *testing.T) {
		err := VerifyKeyring(ctx, wrongStore, config.KeyringCheckEnforce, log)
		if !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), "A aplicação não foi iniciada") {
			t.Fatalf("erro = %v", err)
		}
	})
	t.Run("inicialização aceita com as chaves certas", func(t *testing.T) {
		if err := VerifyKeyring(ctx, env.Store, config.KeyringCheckEnforce, log); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("modo warn inicia e registra", func(t *testing.T) {
		var logs bytes.Buffer
		if err := VerifyKeyring(ctx, wrongStore, config.KeyringCheckWarn, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), "keyring incompatível com o banco") {
			t.Fatalf("log sem o aviso:\n%s", logs.String())
		}
	})
	// A própria conferência pode falhar (banco indisponível, prazo esgotado):
	// no modo enforce a aplicação não inicia; no modo warn, registra e inicia.
	t.Run("falha da conferência conforme o modo", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		err := VerifyKeyring(canceled, env.Store, config.KeyringCheckEnforce, log)
		if err == nil || errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), "conferência do keyring") {
			t.Fatalf("erro = %v, esperado falha da conferência (não de keyring)", err)
		}
		var logs bytes.Buffer
		if err := VerifyKeyring(canceled, env.Store, config.KeyringCheckWarn, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatalf("o modo warn deveria iniciar mesmo sem concluir a conferência: %v", err)
		}
		if !strings.Contains(logs.String(), "conferência do keyring não concluída; iniciando assim mesmo") ||
			!strings.Contains(logs.String(), "level=ERROR") {
			t.Fatalf("log sem o registro da falha:\n%s", logs.String())
		}
	})
	// Nenhuma das conferências acima tocou na entrega pendente (com a chave
	// errada, o worker a encerraria como secret_unavailable).
	if got := deliveryState(); got != "pending/0" {
		t.Fatalf("entrega depois das conferências: %s", got)
	}

	// Chave perdida: a chave t1 não existe mais e a instalação segue com t2.
	// Declarada em RECORVIA_ENCRYPTION_LOST_KEYS, deixa de impedir a
	// inicialização no modo enforce, sem desligar a conferência das demais
	// chaves; o relatório mostra o que ainda depende dela até a conta ser
	// configurada de novo e o segredo do endpoint ser rotacionado.
	t.Run("chave perdida declarada e integrações refeitas", func(t *testing.T) {
		runLost := func(lost string) (string, error) {
			t.Helper()
			vars := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": iso.AppURL,
				"RECORVIA_ENCRYPTION_KEYS": "t2:" + b64(8), "RECORVIA_ENCRYPTION_ACTIVE_KEY": "t2", "RECORVIA_ENCRYPTION_LOST_KEYS": lost}
			var out bytes.Buffer
			err := RunKeyringCheck(ctx, func(k string) string { return vars[k] }, &out)
			return out.String(), err
		}
		if _, err := runLost(""); !errors.Is(err, secrets.ErrKeyringMismatch) {
			t.Fatalf("sem a declaração, a chave ausente é recusada: %v", err)
		}
		if _, err := runLost("t2"); err == nil || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_LOST_KEYS") {
			t.Fatalf("erro = %v, esperado recusar chave configurada declarada como perdida", err)
		}
		// A chave perdida (t1) era a ativa: a fase 1 da rotação, que
		// acrescenta a chave nova e mantém a ativa, é recusada como erro de
		// configuração, com ou sem a declaração, sem conferir nem alterar o
		// banco. A saída é a troca num único passo, conferida logo abaixo.
		for _, lost := range []string{"", "t1"} {
			vars := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": iso.AppURL,
				"RECORVIA_ENCRYPTION_KEYS": "t2:" + b64(8), "RECORVIA_ENCRYPTION_ACTIVE_KEY": "t1", "RECORVIA_ENCRYPTION_LOST_KEYS": lost}
			var out bytes.Buffer
			err := RunKeyringCheck(ctx, func(k string) string { return vars[k] }, &out)
			if err == nil || errors.Is(err, secrets.ErrKeyringMismatch) ||
				!strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada") || out.Len() != 0 {
				t.Fatalf("fase 1 com a chave ativa perdida (declarada: %q): erro = %v, saída = %q", lost, err, out.String())
			}
		}
		out, err := runLost("t1")
		if err != nil {
			t.Fatalf("keyring-check com a chave declarada perdida: %v\n%s", err, out)
		}
		for _, frag := range []string{"chaves configuradas: t2 (ativa: t2); declaradas perdidas: t1; organizações conferidas: 1",
			"chave declarada perdida", "keyring compatível com o banco",
			`atenção: chave "t1" declarada perdida: 2 registro(s) de ` + pgstore.KeyringPurposeProviderAccount,
			`atenção: chave "t1" declarada perdida: 1 registro(s) de ` + pgstore.KeyringPurposeWebhookSigning} {
			if !strings.Contains(out, frag) {
				t.Errorf("relatório sem %q:\n%s", frag, out)
			}
		}

		newKeys, err := secrets.NewKeyring(map[string][]byte{"t2": bytes.Repeat([]byte{8}, 32)}, "t2")
		if err != nil {
			t.Fatal(err)
		}
		// Sem a declaração, o modo enforce recusa; com ela, inicia e registra
		// o que continua ilegível.
		if err := VerifyKeyring(ctx, pgstore.New(env.DB, newKeys, env.Clock, nil), config.KeyringCheckEnforce, log); !errors.Is(err, secrets.ErrKeyringMismatch) {
			t.Fatalf("erro = %v, esperado recusa sem a declaração", err)
		}
		declared, err := newKeys.WithLostKeys([]string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		if err := VerifyKeyring(ctx, pgstore.New(env.DB, declared, env.Clock, nil), config.KeyringCheckEnforce,
			slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatalf("inicialização no modo enforce com a chave declarada perdida: %v", err)
		}
		if !strings.Contains(logs.String(), "dados em uso cifrados com chave declarada perdida") ||
			!strings.Contains(logs.String(), "keyring conferido com o banco") {
			t.Fatalf("log inesperado:\n%s", logs.String())
		}

		// A aplicação religada com a chave nova refaz as integrações: a conta
		// de provedor é gravada de novo e o segredo do endpoint, rotacionado.
		recovered, err := Assemble(cfg, log, env.DB, declared, env.Clock)
		if err != nil {
			t.Fatal(err)
		}
		err = recovered.Store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
			acc, err := tx.AccountByProvider(ctx, "simulator")
			if err != nil {
				return err
			}
			acc.APIKey, acc.APIKeyHint, acc.Secret = "sim_refeita_0002", "0002", strings.Repeat("n", 40)
			return tx.SaveAccount(ctx, acc, []byte("hash-do-token"), acc.Version)
		})
		if err != nil {
			t.Fatalf("reconfiguração da conta com a chave perdida: %v", err)
		}
		// Sem janela para o segredo anterior, que não decifra mais.
		noWindow := 0
		if _, _, err := recovered.Webhooks.RotateSecret(ctx, tn.ID, tn.Actor, endpoint.ID, &noWindow, endpoint.Version); err != nil {
			t.Fatalf("rotação do segredo do endpoint com a chave perdida: %v", err)
		}
		out, err = runLost("t1")
		if err != nil {
			t.Fatalf("keyring-check depois de refazer as integrações: %v\n%s", err, out)
		}
		// Nada em uso depende mais da chave perdida: resta o segredo
		// anterior do endpoint, expirado.
		if strings.Contains(out, "atenção:") || !strings.Contains(out, "chave declarada perdida (fora de uso)") {
			t.Fatalf("relatório depois de refazer as integrações:\n%s", out)
		}
		audit, err := recovered.Store.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(audit.LostProblems()) != 0 || audit.Err() != nil {
			t.Fatalf("dados em uso ainda dependem da chave perdida: %q (%v)", audit.LostProblems(), audit.Err())
		}
		for _, u := range audit.Usages {
			if u.KeyID == "t2" && (u.Failed != 0 || u.InUse == 0) {
				t.Fatalf("dados refeitos com a chave nova: %+v", u)
			}
		}
	})
}
