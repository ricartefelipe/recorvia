//go:build integration

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// O subcomando reconcile-pending abre o banco só com a URL e as chaves (sem
// Redis, broker nem identidade), assume a conciliação registrada pela API e
// a executa até o fim. Com o provedor indisponível, a execução termina como
// failed, com a causa, e o subcomando sai com erro: o procedimento de
// recuperação não segue com a janela sem conciliar.
func TestReconcilePendingCommandExecutesRegisteredRuns(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)
	iso := pgtest.Isolated(t)
	env := fixture.NewOn(t, pgtest.AppDBOf(t, iso, 8), start)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "indisponível", http.StatusServiceUnavailable)
	}))
	t.Cleanup(provider.Close)

	cfg, err := config.Load(func(k string) string {
		return map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_PAYSIM_URL": provider.URL}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	core, err := Assemble(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), env.DB, env.Keyring, env.Clock)
	if err != nil {
		t.Fatal(err)
	}
	tn := env.NewTenant(t, "janela-perdida")
	err = env.Store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
		return tx.SaveAccount(ctx, payments.ProviderAccount{ID: ids.New(), TenantID: tn.ID, Provider: "simulator", Environment: "local",
			Status: "active", IsDefault: true, APIKey: "sim_janela_0001", APIKeyHint: "0001", Secret: strings.Repeat("s", 40),
			CreatedAt: start, UpdatedAt: start, Version: 1}, []byte("hash-do-token-janela"))
	})
	if err != nil {
		t.Fatal(err)
	}

	b64 := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	vars := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": iso.AppURL,
		"RECORVIA_ENCRYPTION_KEYS": "t1:" + b64, "RECORVIA_ENCRYPTION_ACTIVE_KEY": "t1", "RECORVIA_PAYSIM_URL": provider.URL}
	runCommand := func() (string, error) {
		t.Helper()
		var out bytes.Buffer
		err := RunReconcilePending(ctx, func(k string) string { return vars[k] }, &out)
		return out.String(), err
	}

	// Sem conciliação registrada, termina sem erro e sem tocar no provedor.
	if out, err := runCommand(); err != nil || !strings.Contains(out, "0 concluídas, 0 com falha, 0 devolvidas") {
		t.Fatalf("sem pendências: %v\n%s", err, out)
	}

	run, err := core.Payments.StartReconciliation(ctx, tn.ID, tn.Actor, nil, payments.RunInput{RequestKey: "janela-" + ids.New().String()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runCommand()
	if err == nil || !strings.Contains(err.Error(), "terminaram com falha") || !strings.Contains(out, "1 com falha") {
		t.Fatalf("conciliação com o provedor indisponível: %v\n%s", err, out)
	}
	done, err := core.Payments.ReconciliationRun(ctx, tn.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != payments.RunFailed || done.Error == "" || done.FinishedAt == nil {
		t.Fatalf("a execução registrada deveria terminar como falha, com a causa: %+v", done)
	}
	// Já encerrada, não é executada de novo.
	if out, err := runCommand(); err != nil || !strings.Contains(out, "0 concluídas, 0 com falha") {
		t.Fatalf("execução encerrada não deveria ser retomada: %v\n%s", err, out)
	}
}
