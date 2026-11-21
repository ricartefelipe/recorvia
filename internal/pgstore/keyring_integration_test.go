//go:build integration

package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways"
	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/paysim"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

func testKeyring(t *testing.T, active string, keys map[string]byte) *secrets.Keyring {
	t.Helper()
	m := map[string][]byte{}
	for id, b := range keys {
		m[id] = bytes.Repeat([]byte{b}, 32)
	}
	k, err := secrets.NewKeyring(m, active)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func usageOf(t *testing.T, a pgstore.KeyringAudit, purpose, keyID string) secrets.KeyUsage {
	t.Helper()
	for _, u := range a.Usages {
		if u.Purpose == purpose && u.KeyID == keyID {
			return u
		}
	}
	t.Fatalf("relatório sem a linha %q/%q: %+v", purpose, keyID, a.Usages)
	return secrets.KeyUsage{}
}

// A conferência percorre todas as organizações de um banco próprio do teste:
// credenciais de provedor, segredos de webhook e notificações pendentes
// cifrados por duas versões de chave (rotação). Com o keyring completo, tudo
// decifra; sem a chave antiga ou com outro valor para ela, a conferência
// aponta a chave, a finalidade e a quantidade, e nada é alterado no banco.
func TestAuditKeyringAgainstDatabase(t *testing.T) {
	ctx := context.Background()
	iso := pgtest.Isolated(t)
	env := fixture.NewOn(t, pgtest.AppDBOf(t, iso, 8), now)
	owner, err := pgxpool.New(ctx, iso.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)

	// Organização A, cifrada com a chave t1 do ambiente de testes: conta de
	// provedor (chave de API e segredo do webhook), endpoint de webhook e uma
	// notificação ainda não enviada.
	a := env.NewTenant(t, "keyring-a")
	if _, err := env.Platform.ChangePlan(ctx, a.ID, a.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	accountID := ids.New()
	err = env.Store.Payments().InTenant(ctx, a.ID, func(tx payments.Tx) error {
		return tx.SaveAccount(ctx, payments.ProviderAccount{ID: accountID, TenantID: a.ID, Provider: "simulator", Environment: "local",
			Status: "active", IsDefault: true, APIKey: "sim_keyring_0001", APIKeyHint: "0001", Secret: strings.Repeat("s", 40),
			CreatedAt: now, UpdatedAt: now, Version: 1}, []byte("hash-do-token-a"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}})
	hooks := webhookService(env, client)
	oldEndpoint, _, err := hooks.CreateEndpoint(ctx, a.ID, a.Actor, nil, webhooks.Input{URL: "http://127.0.0.1:9/antigo",
		EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	notif := notifications.NewService(env.Store.Notifications(), nil, env.Keyring, env.Clock, notifications.Config{})
	_, invoiceID := issuedInvoice(t, env, a, "financeiro@keyring-a.test")
	if err := notif.HandleInvoiceIssued(ctx, a.ID, invoiceID, ids.New()); err != nil {
		t.Fatal(err)
	}

	// Rotação: a chave t2 passa a ser a ativa; a organização B cria um
	// endpoint cujo segredo é cifrado com ela.
	rotated := testKeyring(t, "t2", map[string]byte{"t1": 7, "t2": 8})
	rotatedStore := pgstore.New(env.DB, rotated, env.Clock, nil)
	b := env.NewTenant(t, "keyring-b")
	if _, err := env.Platform.ChangePlan(ctx, b.ID, b.Actor, "profissional"); err != nil {
		t.Fatal(err)
	}
	rotatedHooks := webhooks.NewService(rotatedStore.Webhooks(), transport{client}, client, env.Clock, webhooks.Config{Lease: time.Minute,
		Timeout: 5 * time.Second})
	if _, _, err := rotatedHooks.CreateEndpoint(ctx, b.ID, b.Actor, nil, webhooks.Input{URL: "http://127.0.0.1:9/novo",
		EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
		t.Fatal(err)
	}

	snapshot := func() string {
		t.Helper()
		var sum string
		if err := owner.QueryRow(ctx, `SELECT md5(
			(SELECT COALESCE(string_agg(md5(a::text), '' ORDER BY a.id), '') FROM payment_provider_accounts a) ||
			(SELECT COALESCE(string_agg(md5(s::text), '' ORDER BY s.id), '') FROM webhook_endpoint_secrets s) ||
			(SELECT COALESCE(string_agg(md5(n::text), '' ORDER BY n.id), '') FROM notifications n) ||
			(SELECT COALESCE(string_agg(md5(d::text), '' ORDER BY d.id), '') FROM webhook_deliveries d))`).Scan(&sum); err != nil {
			t.Fatal(err)
		}
		return sum
	}
	before := snapshot()

	t.Run("keyring completo", func(t *testing.T) {
		audit, err := rotatedStore.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); err != nil {
			t.Fatalf("keyring completo deveria ser compatível: %v", err)
		}
		if audit.Tenants != 2 || audit.ActiveKey != "t2" || strings.Join(audit.ConfiguredKeys, ",") != "t1,t2" {
			t.Fatalf("relatório: %d organizações, chaves %v, ativa %s", audit.Tenants, audit.ConfiguredKeys, audit.ActiveKey)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeProviderAccount, "t1"); u.Total != 2 || u.InUse != 2 || u.Failed != 0 {
			t.Fatalf("credenciais de provedor: %+v", u)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t1"); u.Total != 1 || u.InUse != 1 || u.Failed != 0 {
			t.Fatalf("segredo de webhook da chave antiga: %+v", u)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t2"); u.Total != 1 || u.InUse != 1 || u.Failed != 0 {
			t.Fatalf("segredo de webhook da chave ativa: %+v", u)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeNotification, "t1"); u.Total != 1 || u.InUse != 1 || u.Failed != 0 {
			t.Fatalf("notificação pendente: %+v", u)
		}
	})

	t.Run("chave antiga ausente", func(t *testing.T) {
		audit, err := pgstore.New(env.DB, testKeyring(t, "t2", map[string]byte{"t2": 8}), env.Clock, nil).AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = audit.Err()
		if !errors.Is(err, secrets.ErrKeyringMismatch) {
			t.Fatalf("erro = %v, esperado ErrKeyringMismatch", err)
		}
		for _, frag := range []string{
			`chave "t1" ausente do keyring: 2 registro(s) de credencial de provedor de pagamento não podem ser decifrados (2 em uso)`,
			`chave "t1" ausente do keyring: 1 registro(s) de segredo de endpoint de webhook não podem ser decifrados (1 em uso)`,
			`chave "t1" ausente do keyring: 1 registro(s) de conteúdo de notificação pendente não podem ser decifrados (1 em uso)`,
		} {
			if !strings.Contains(err.Error(), frag) {
				t.Errorf("erro sem %q: %v", frag, err)
			}
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t2"); u.Failed != 0 {
			t.Fatalf("o segredo cifrado com a chave presente deveria decifrar: %+v", u)
		}
	})

	t.Run("chave antiga com outro valor", func(t *testing.T) {
		audit, err := pgstore.New(env.DB, testKeyring(t, "t2", map[string]byte{"t1": 99, "t2": 8}), env.Clock, nil).AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = audit.Err()
		if !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(),
			`chave "t1" com valor diferente do usado na cifragem: nenhum dos 2 registro(s) de credencial de provedor de pagamento decifra (2 em uso)`) {
			t.Fatalf("erro = %v", err)
		}
		if strings.Contains(err.Error(), "sim_keyring_0001") {
			t.Fatalf("o erro não pode trazer segredos: %v", err)
		}
	})

	if after := snapshot(); after != before {
		t.Fatal("a conferência do keyring alterou dados cifrados, notificações ou entregas")
	}

	// Dado adulterado não é engano do keyring quando a chave está comprovada
	// por outros registros, de qualquer finalidade: o relatório aponta os
	// registros ilegíveis e a conferência não bloqueia.
	tamper := func(t *testing.T, table, column string, id any) {
		t.Helper()
		if _, err := owner.Exec(ctx, "UPDATE "+table+" SET "+column+" = "+column+` || '\x00'::bytea WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := owner.Exec(ctx, "UPDATE "+table+" SET "+column+" = substring("+column+" from 1 for length("+column+") - 1) WHERE id = $1",
				id); err != nil {
				t.Errorf("restauração do ciphertext: %v", err)
			}
		})
	}
	t.Run("um registro adulterado entre outros da mesma finalidade", func(t *testing.T) {
		tamper(t, "payment_provider_accounts", "api_key_ciphertext", accountID)
		audit, err := rotatedStore.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); err != nil {
			t.Fatalf("dado corrompido com a chave comprovada não deveria bloquear: %v", err)
		}
		u := usageOf(t, audit, pgstore.KeyringPurposeProviderAccount, "t1")
		if u.Failed != 1 || !u.KeyProven || !u.Corrupted() || u.Result() != "registros ilegíveis" {
			t.Fatalf("credenciais: %+v (%s)", u, u.Result())
		}
		if got := audit.CorruptedProblems(); len(got) != 1 ||
			!strings.Contains(got[0], `1 de 2 registro(s) de credencial de provedor de pagamento não decifram com a chave "t1"`) {
			t.Fatalf("problemas = %q", got)
		}
	})
	t.Run("registro único adulterado com a chave comprovada por outra finalidade", func(t *testing.T) {
		var secretID uuid.UUID
		if err := owner.QueryRow(ctx, `SELECT id FROM webhook_endpoint_secrets WHERE tenant_id = $1 AND endpoint_id = $2`, a.ID, oldEndpoint.ID).
			Scan(&secretID); err != nil {
			t.Fatal(err)
		}
		tamper(t, "webhook_endpoint_secrets", "secret_ciphertext", secretID)
		audit, err := rotatedStore.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); err != nil {
			t.Fatalf("a chave t1 decifra as credenciais de provedor: o segredo adulterado não é engano do keyring: %v", err)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeProviderAccount, "t1"); u.Result() != "ok" {
			t.Fatalf("credenciais: %+v (%s)", u, u.Result())
		}
		u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t1")
		if u.Total != 1 || u.Failed != 1 || u.Blocking() || u.Result() != "registros ilegíveis" {
			t.Fatalf("segredo adulterado: %+v (%s)", u, u.Result())
		}
	})
	// Limite do diagnóstico: o segredo da organização B é o único registro
	// cifrado com t2. Adulterado, nada comprova a chave, e a conferência não
	// distingue o caso de uma chave com outro valor.
	t.Run("único registro de uma chave adulterado", func(t *testing.T) {
		var secretID uuid.UUID
		if err := owner.QueryRow(ctx, `SELECT id FROM webhook_endpoint_secrets WHERE tenant_id = $1`, b.ID).Scan(&secretID); err != nil {
			t.Fatal(err)
		}
		tamper(t, "webhook_endpoint_secrets", "secret_ciphertext", secretID)
		audit, err := rotatedStore.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(),
			`chave "t2" com valor diferente do usado na cifragem: nenhum dos 1 registro(s) de segredo de endpoint de webhook decifra (1 em uso)`) {
			t.Fatalf("erro = %v", err)
		}
	})

	// Chave declarada perdida (RECORVIA_ENCRYPTION_LOST_KEYS): os dados em uso
	// cifrados com ela aparecem no relatório e não bloqueiam; as demais
	// chaves continuam conferidas.
	t.Run("chave antiga declarada perdida", func(t *testing.T) {
		lost, err := testKeyring(t, "t2", map[string]byte{"t2": 8}).WithLostKeys([]string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		audit, err := pgstore.New(env.DB, lost, env.Clock, nil).AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); err != nil {
			t.Fatalf("chave declarada perdida não deveria bloquear: %v", err)
		}
		if strings.Join(audit.LostKeys, ",") != "t1" || len(audit.LostProblems()) != 3 {
			t.Fatalf("chaves perdidas %v; problemas %q", audit.LostKeys, audit.LostProblems())
		}
		for _, purpose := range []string{pgstore.KeyringPurposeProviderAccount, pgstore.KeyringPurposeWebhookSigning, pgstore.KeyringPurposeNotification} {
			if u := usageOf(t, audit, purpose, "t1"); !u.Lost || u.Blocking() || u.FailedInUse == 0 || u.Result() != "chave declarada perdida" {
				t.Fatalf("%s: %+v (%s)", purpose, u, u.Result())
			}
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t2"); u.Result() != "ok" {
			t.Fatalf("a chave presente continua conferida: %+v", u)
		}
		// Outra chave ausente continua bloqueando.
		lostOther, err := testKeyring(t, "t3", map[string]byte{"t3": 3}).WithLostKeys([]string{"t1"})
		if err != nil {
			t.Fatal(err)
		}
		audit, err = pgstore.New(env.DB, lostOther, env.Clock, nil).AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), `chave "t2" ausente do keyring`) {
			t.Fatalf("erro = %v, esperado bloquear pela chave t2", err)
		}
	})

	// Dados fora de uso não bloqueiam: conta desativada, endpoint removido e
	// notificação encerrada deixam de exigir a chave antiga.
	t.Run("dados fora de uso não bloqueiam", func(t *testing.T) {
		current, err := hooks.Endpoint(ctx, a.ID, oldEndpoint.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := hooks.DeleteEndpoint(ctx, a.ID, a.Actor, oldEndpoint.ID, current.Version); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE payment_provider_accounts SET status = 'disabled', is_default = false WHERE id = $1`, accountID); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, `UPDATE notifications SET status = 'canceled', status_reason = 'teste' WHERE tenant_id = $1`, a.ID); err != nil {
			t.Fatal(err)
		}
		audit, err := pgstore.New(env.DB, testKeyring(t, "t2", map[string]byte{"t2": 8}), env.Clock, nil).AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); err != nil {
			t.Fatalf("dados fora de uso não deveriam bloquear: %v", err)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeProviderAccount, "t1"); u.InUse != 0 || u.Failed != 2 || !u.Missing || u.Blocking() {
			t.Fatalf("conta desativada: %+v", u)
		}
		if u := usageOf(t, audit, pgstore.KeyringPurposeWebhookSigning, "t1"); u.InUse != 0 || u.Failed != 1 || u.Blocking() {
			t.Fatalf("segredo de endpoint removido: %+v", u)
		}
		if len(audit.Problems(false)) != 2 || len(audit.Problems(true)) != 0 {
			t.Fatalf("problemas: %q", audit.Problems(false))
		}
		for _, u := range audit.Usages {
			if u.Purpose == pgstore.KeyringPurposeNotification {
				t.Fatalf("notificação encerrada não deveria ser conferida: %+v", u)
			}
		}
	})
}

// Com a chave de cifragem trocada ou perdida, a leitura das credenciais do
// provedor falha com um erro de aplicação de código estável (não um erro
// interno), e a conta pode ser configurada de novo: a troca não depende de
// decifrar as credenciais anteriores.
func TestProviderCredentialsUnreadableAndReconfiguration(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := env.NewTenant(t, "chave-perdida")
	st, err := paysim.Load("")
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", func() time.Time { return env.Clock.Now() }, discardLogger())
	simSrv := httptest.NewServer(sim.Handler())
	t.Cleanup(simSrv.Close)
	resolver := gateways.NewResolver(5*time.Second, gateways.Factory{Provider: "simulator", Verifier: simulator.Verifier{},
		Gateway: func(a payments.ProviderAccount, hc *http.Client) (payments.Gateway, error) {
			return simulator.New(simSrv.URL, a.APIKey, hc), nil
		}})
	cfg := payments.Config{WebhookBaseURL: "http://recorvia.test/webhooks/providers", CallTimeout: 5 * time.Second, Lease: 10 * time.Second}
	original := env.Payments(resolver, cfg)
	oldKey := "sim_antiga_" + uuid.NewString()[:8]
	first, err := original.ConfigureAccount(ctx, tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: oldKey, WebhookSecret: strings.Repeat("a", 40), MakeDefault: true})
	if err != nil {
		t.Fatal(err)
	}

	defaultAccount := func(store *pgstore.Store) (payments.ProviderAccount, error) {
		var acc payments.ProviderAccount
		err := store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
			var err error
			acc, err = tx.DefaultAccount(ctx)
			return err
		})
		return acc, err
	}
	cases := []struct {
		name    string
		keyring *secrets.Keyring
		cause   error
	}{
		{"chave ausente", testKeyring(t, "t9", map[string]byte{"t9": 9}), secrets.ErrUnknownKey},
		{"chave com outro valor", testKeyring(t, "t1", map[string]byte{"t1": 99}), secrets.ErrDecrypt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := defaultAccount(pgstore.New(env.DB, tc.keyring, env.Clock, nil))
			e, ok := apperr.As(err)
			if !ok || e.Code != "provider_credentials_unreadable" || e.Kind != apperr.KindUnavailable {
				t.Fatalf("erro = %v, esperado provider_credentials_unreadable (indisponível)", err)
			}
			if !errors.Is(err, tc.cause) {
				t.Fatalf("a causa %v deveria ficar encadeada: %v", tc.cause, err)
			}
			if !strings.Contains(e.Message, "chaves de cifragem") || strings.Contains(e.Message, oldKey) {
				t.Fatalf("mensagem inadequada: %q", e.Message)
			}
		})
	}

	// Chave perdida: a instalação passa a usar outra chave e o tenant
	// configura o provedor de novo, pela mesma operação de sempre.
	lost := testKeyring(t, "t9", map[string]byte{"t9": 9})
	lostStore := pgstore.New(env.DB, lost, env.Clock, nil)
	recovered := payments.NewService(lostStore.Payments(), resolver, env.Clock, cfg)
	accounts, err := recovered.Accounts(ctx, tn.ID)
	if err != nil || len(accounts) != 1 || accounts[0].APIKeyHint != oldKey[len(oldKey)-4:] {
		t.Fatalf("a listagem (sem segredos) deveria continuar disponível: %+v %v", accounts, err)
	}
	newKey := "sim_nova_" + uuid.NewString()[:8]
	second, err := recovered.ConfigureAccount(ctx, tn.ID, tn.Actor, payments.AccountInput{Provider: "simulator", Environment: "local",
		APIKey: newKey, WebhookSecret: strings.Repeat("b", 40), ExpectedVersion: accounts[0].Version})
	if err != nil {
		t.Fatalf("a reconfiguração deveria funcionar sem decifrar as credenciais anteriores: %v", err)
	}
	if second.Account.ID != first.Account.ID || !second.Account.IsDefault {
		t.Fatalf("a reconfiguração deveria manter a conta e o padrão: %+v", second.Account)
	}
	got, err := defaultAccount(lostStore)
	if err != nil || got.APIKey != newKey || got.Secret != strings.Repeat("b", 40) {
		t.Fatalf("credenciais novas ilegíveis com a chave nova: %+v %v", got, err)
	}
	// A chave antiga deixou de servir para esta conta.
	if _, err := defaultAccount(env.Store); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("erro com o keyring antigo = %v, esperado chave desconhecida", err)
	}
}
