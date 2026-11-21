package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

type fakeAuditor struct {
	audit pgstore.KeyringAudit
	err   error
	calls int
}

func (f *fakeAuditor) AuditKeyring(context.Context) (pgstore.KeyringAudit, error) {
	f.calls++
	return f.audit, f.err
}

func keyringOf(t *testing.T, active string, ids ...string) *secrets.Keyring {
	t.Helper()
	keys := map[string][]byte{}
	for i, id := range ids {
		keys[id] = bytes.Repeat([]byte{byte(i + 1)}, 32)
	}
	k, err := secrets.NewKeyring(keys, active)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// auditWith cifra um dado com o keyring de origem e o confere com o keyring
// da aplicação, como faz o adaptador sobre as linhas do banco.
func auditWith(t *testing.T, origin, current *secrets.Keyring, inUse bool) pgstore.KeyringAudit {
	t.Helper()
	ct, kid, err := origin.Encrypt([]byte("segredo"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	a := secrets.NewAudit(current)
	a.Check(pgstore.KeyringPurposeProviderAccount, kid, ct, []byte("aad"), inUse)
	return pgstore.KeyringAudit{AuditReport: a.Report(), Tenants: 1}
}

// corruptedAudit confere duas credenciais íntegras e um segredo de webhook
// adulterado, todos cifrados com a mesma chave.
func corruptedAudit(t *testing.T, k *secrets.Keyring) pgstore.KeyringAudit {
	t.Helper()
	a := secrets.NewAudit(k)
	for _, purpose := range []string{pgstore.KeyringPurposeProviderAccount, pgstore.KeyringPurposeProviderAccount, pgstore.KeyringPurposeWebhookSigning} {
		ct, kid, err := k.Encrypt([]byte("segredo"), []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		if purpose == pgstore.KeyringPurposeWebhookSigning {
			ct[len(ct)-1] ^= 0xff
		}
		a.Check(purpose, kid, ct, []byte("aad"), true)
	}
	return pgstore.KeyringAudit{AuditReport: a.Report(), Tenants: 2}
}

func TestVerifyKeyringModes(t *testing.T) {
	origin := keyringOf(t, "k1", "k1")
	missing := keyringOf(t, "k2", "k2")
	lostDeclared, err := missing.WithLostKeys([]string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		audit    pgstore.KeyringAudit
		auditErr error
		mode     config.KeyringCheckMode
		wantErr  string // trecho esperado; vazio: sem erro
		wantLog  string
		wantCall int
	}{
		{name: "keyring compatível", audit: auditWith(t, origin, origin, true), mode: config.KeyringCheckEnforce,
			wantLog: "keyring conferido com o banco", wantCall: 1},
		{name: "chave ausente impede a inicialização", audit: auditWith(t, origin, missing, true), mode: config.KeyringCheckEnforce,
			wantErr: `chave "k1" ausente do keyring`, wantCall: 1},
		{name: "chave ausente no modo warn só registra", audit: auditWith(t, origin, missing, true), mode: config.KeyringCheckWarn,
			wantLog: "keyring incompatível com o banco; iniciando assim mesmo", wantCall: 1},
		{name: "dado fora de uso não bloqueia", audit: auditWith(t, origin, missing, false), mode: config.KeyringCheckEnforce,
			wantLog: "dado cifrado fora de uso que o keyring não decifra", wantCall: 1},
		{name: "modo off não consulta o banco", audit: auditWith(t, origin, missing, true), mode: config.KeyringCheckOff,
			wantLog: "conferência do keyring desligada"},
		{name: "falha de leitura impede a inicialização", auditErr: errors.New("banco fora do ar"), mode: config.KeyringCheckEnforce,
			wantErr: "conferência do keyring: banco fora do ar", wantCall: 1},
		{name: "falha de leitura no modo warn só registra", auditErr: errors.New("banco fora do ar"), mode: config.KeyringCheckWarn,
			wantLog: "conferência do keyring não concluída; iniciando assim mesmo", wantCall: 1},
		{name: "prazo esgotado no modo warn só registra", auditErr: context.DeadlineExceeded, mode: config.KeyringCheckWarn,
			wantLog: "context deadline exceeded", wantCall: 1},
		{name: "prazo esgotado impede a inicialização", auditErr: context.DeadlineExceeded, mode: config.KeyringCheckEnforce,
			wantErr: "conferência do keyring: context deadline exceeded", wantCall: 1},
		{name: "falha de leitura no modo off não consulta o banco", auditErr: errors.New("banco fora do ar"), mode: config.KeyringCheckOff,
			wantLog: "conferência do keyring desligada"},
		{name: "chave declarada perdida não impede a inicialização", audit: auditWith(t, origin, lostDeclared, true),
			mode: config.KeyringCheckEnforce, wantLog: "dados em uso cifrados com chave declarada perdida", wantCall: 1},
		{name: "registro corrompido com a chave comprovada não impede a inicialização", audit: corruptedAudit(t, origin),
			mode: config.KeyringCheckEnforce, wantLog: "registros em uso que não decifram com uma chave comprovada", wantCall: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			a := &fakeAuditor{audit: tc.audit, err: tc.auditErr}
			err := VerifyKeyring(context.Background(), a, tc.mode, log)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("erro inesperado: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("erro = %v, esperado conter %q", err, tc.wantErr)
			}
			if a.calls != tc.wantCall {
				t.Fatalf("consultas ao banco = %d, esperado %d", a.calls, tc.wantCall)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Fatalf("log sem %q:\n%s", tc.wantLog, logs.String())
			}
		})
	}
}

func TestVerifyKeyringErrorExplainsWhatToDo(t *testing.T) {
	a := &fakeAuditor{audit: auditWith(t, keyringOf(t, "k1", "k1"), keyringOf(t, "k2", "k2"), true)}
	err := VerifyKeyring(context.Background(), a, config.KeyringCheckEnforce, slog.New(slog.DiscardHandler))
	if !errors.Is(err, secrets.ErrKeyringMismatch) {
		t.Fatalf("erro = %v, esperado ErrKeyringMismatch", err)
	}
	for _, frag := range []string{"A aplicação não foi iniciada", "RECORVIA_ENCRYPTION_KEYS", "RECORVIA_ENCRYPTION_LOST_KEYS",
		"docs/operacao/backup-e-restauracao.md"} {
		if !strings.Contains(err.Error(), frag) {
			t.Errorf("erro sem %q: %v", frag, err)
		}
	}
	if strings.Contains(err.Error(), "segredo") {
		t.Fatalf("o erro não pode trazer conteúdo decifrado: %v", err)
	}
}

func TestCheckKeyringReport(t *testing.T) {
	origin := keyringOf(t, "k1", "k1")
	t.Run("compatível", func(t *testing.T) {
		var out bytes.Buffer
		if err := checkKeyring(context.Background(), &fakeAuditor{audit: auditWith(t, origin, origin, true)}, &out); err != nil {
			t.Fatal(err)
		}
		for _, frag := range []string{"chaves configuradas: k1 (ativa: k1); organizações conferidas: 1", "FINALIDADE", "CHAVE",
			pgstore.KeyringPurposeProviderAccount, "ok", "keyring compatível com o banco"} {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("relatório sem %q:\n%s", frag, out.String())
			}
		}
	})
	t.Run("chave ausente", func(t *testing.T) {
		var out bytes.Buffer
		err := checkKeyring(context.Background(), &fakeAuditor{audit: auditWith(t, origin, keyringOf(t, "k2", "k2"), true)}, &out)
		if !errors.Is(err, secrets.ErrKeyringMismatch) {
			t.Fatalf("erro = %v, esperado ErrKeyringMismatch", err)
		}
		if !strings.Contains(out.String(), "chave ausente") || strings.Contains(out.String(), "keyring compatível") {
			t.Fatalf("relatório inesperado:\n%s", out.String())
		}
	})
	t.Run("chave com outro valor", func(t *testing.T) {
		other, err := secrets.NewKeyring(map[string][]byte{"k1": bytes.Repeat([]byte{9}, 32)}, "k1")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		err = checkKeyring(context.Background(), &fakeAuditor{audit: auditWith(t, origin, other, true)}, &out)
		if !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(out.String(), "chave com outro valor") {
			t.Fatalf("erro = %v; relatório:\n%s", err, out.String())
		}
	})
	t.Run("fora de uso", func(t *testing.T) {
		var out bytes.Buffer
		if err := checkKeyring(context.Background(), &fakeAuditor{audit: auditWith(t, origin, keyringOf(t, "k2", "k2"), false)}, &out); err != nil {
			t.Fatalf("dado fora de uso não deveria falhar: %v", err)
		}
		if !strings.Contains(out.String(), "chave ausente (fora de uso)") {
			t.Fatalf("relatório inesperado:\n%s", out.String())
		}
	})
	t.Run("chave declarada perdida", func(t *testing.T) {
		lost, err := keyringOf(t, "k2", "k2").WithLostKeys([]string{"k1"})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := checkKeyring(context.Background(), &fakeAuditor{audit: auditWith(t, origin, lost, true)}, &out); err != nil {
			t.Fatalf("chave declarada perdida não deveria falhar: %v", err)
		}
		for _, frag := range []string{"chaves configuradas: k2 (ativa: k2); declaradas perdidas: k1; organizações conferidas: 1",
			"chave declarada perdida", "keyring compatível com o banco", `atenção: chave "k1" declarada perdida: 1 registro(s)`} {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("relatório sem %q:\n%s", frag, out.String())
			}
		}
	})
	// Registro único adulterado: a mesma chave decifra as credenciais de
	// provedor, então o relatório não a aponta como errada e o subcomando
	// não falha.
	t.Run("registro adulterado com a chave comprovada", func(t *testing.T) {
		var out bytes.Buffer
		if err := checkKeyring(context.Background(), &fakeAuditor{audit: corruptedAudit(t, origin)}, &out); err != nil {
			t.Fatalf("dado corrompido não é engano do keyring: %v", err)
		}
		for _, frag := range []string{"registros ilegíveis", "keyring compatível com o banco",
			"atenção: 1 de 1 registro(s) de " + pgstore.KeyringPurposeWebhookSigning, "incidente de integridade"} {
			if !strings.Contains(out.String(), frag) {
				t.Errorf("relatório sem %q:\n%s", frag, out.String())
			}
		}
		if strings.Contains(out.String(), "chave com outro valor") {
			t.Fatalf("a chave comprovada não pode aparecer como errada:\n%s", out.String())
		}
	})
	t.Run("banco sem dados cifrados", func(t *testing.T) {
		var out bytes.Buffer
		empty := pgstore.KeyringAudit{AuditReport: secrets.NewAudit(origin).Report()}
		if err := checkKeyring(context.Background(), &fakeAuditor{audit: empty}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "nenhum dado cifrado em uso encontrado no banco") {
			t.Fatalf("relatório inesperado:\n%s", out.String())
		}
	})
	t.Run("falha de leitura", func(t *testing.T) {
		err := checkKeyring(context.Background(), &fakeAuditor{err: errors.New("sem conexão")}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "conferência do keyring: sem conexão") {
			t.Fatalf("erro = %v", err)
		}
	})
}

func TestRunKeyringCheckValidatesConfiguration(t *testing.T) {
	env := map[string]string{"RECORVIA_ENCRYPTION_STARTUP_CHECK": "talvez"}
	err := RunKeyringCheck(context.Background(), func(k string) string { return env[k] }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_STARTUP_CHECK") {
		t.Fatalf("erro = %v, esperado erro de formato da configuração", err)
	}
	err = RunKeyringCheck(context.Background(), func(string) string { return "" }, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "RECORVIA_DATABASE_URL") || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_KEYS") {
		t.Fatalf("erro = %v, esperado exigir banco e chaves", err)
	}
	// Chave ativa perdida com a configuração da fase 1 da rotação (a chave
	// nova na lista e a ativa, que não existe mais, mantida): o subcomando
	// recusa a configuração antes de abrir o banco.
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	env = map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": "postgres://app@localhost/recorvia",
		"RECORVIA_ENCRYPTION_KEYS": "k1:" + key + ",k3:" + key, "RECORVIA_ENCRYPTION_ACTIVE_KEY": "k2", "RECORVIA_ENCRYPTION_LOST_KEYS": "k2"}
	var out bytes.Buffer
	err = RunKeyringCheck(context.Background(), func(k string) string { return env[k] }, &out)
	if err == nil || !strings.Contains(err.Error(), "RECORVIA_ENCRYPTION_ACTIVE_KEY deve referenciar uma chave configurada") {
		t.Fatalf("erro = %v, esperado recusar a chave ativa fora da lista", err)
	}
	if errors.Is(err, secrets.ErrKeyringMismatch) || out.Len() != 0 {
		t.Fatalf("a recusa é de configuração, sem relatório: erro = %v, saída = %q", err, out.String())
	}
}
