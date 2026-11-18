package webhooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

type env struct {
	store     *fakeStore
	transport *fakeTransport
	validator *fakeValidator
	clock     *clock.Manual
	svc       *Service
	tenant    uuid.UUID
	actor     audit.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{store: newFakeStore(), transport: &fakeTransport{}, validator: &fakeValidator{}, clock: clock.NewManual(t0), tenant: ids.New(),
		actor: audit.System("teste")}
	e.store.tenants = []uuid.UUID{e.tenant}
	e.svc = NewService(e.store, e.transport, e.validator, e.clock, Config{RequireHTTPS: true, Lease: time.Minute, Timeout: 10 * time.Second,
		IsPermanent: func(err error) bool { return errors.Is(err, errBlocked) }, Jitter: func() float64 { return 0.5 }})
	return e
}

func (e *env) endpoint(t *testing.T, types ...string) (Endpoint, string) {
	t.Helper()
	if len(types) == 0 {
		types = []string{outbox.EventInvoicePaid}
	}
	ep, secret, err := e.svc.CreateEndpoint(context.Background(), e.tenant, e.actor, nil, Input{URL: "https://hooks.cliente.test/recorvia",
		Description: "ERP", EventTypes: types})
	if err != nil {
		t.Fatal(err)
	}
	return ep, secret
}

func (e *env) publish(t *testing.T, eventType string) uuid.UUID {
	t.Helper()
	id := ids.New()
	if err := e.svc.HandleEvent(context.Background(), e.tenant, id, eventType, t0, []byte(`{"invoice_id":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	return id
}

func deliver(t *testing.T, e *env) DeliverResult {
	t.Helper()
	res, err := e.svc.Deliver(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestCreateEndpointRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, secret := e.endpoint(t)
	if !strings.HasPrefix(secret, SecretPrefix) || len(ep.Secrets) != 1 || ep.Secrets[0].ExpiresAt != nil || ep.Status != Active || ep.Version != 1 {
		t.Fatalf("endpoint criado: %+v", ep)
	}
	if e.store.state.audits[0].Action != "webhook_endpoint.created" {
		t.Fatal("criação auditada")
	}
	if _, _, err := e.svc.CreateEndpoint(ctx, e.tenant, e.actor, nil, Input{URL: "http://hooks.cliente.test/x", EventTypes: []string{outbox.EventInvoicePaid}}); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("http em produção: %v", err)
	}
	e.validator.err = errBlocked
	_, _, err := e.svc.CreateEndpoint(ctx, e.tenant, e.actor, nil, Input{URL: "https://interno.cliente.test/x", EventTypes: []string{outbox.EventInvoicePaid}})
	if ae, ok := apperr.As(err); !ok || ae.Code != "webhook_destination_not_allowed" {
		t.Fatalf("destino bloqueado: %v", err)
	}
	e.validator.err = nil
	e.store.state.plan = platformbilling.Plan{}
	if _, _, err := e.svc.CreateEndpoint(ctx, e.tenant, e.actor, nil, Input{URL: "https://a.test/x", EventTypes: []string{outbox.EventInvoicePaid}}); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("plano sem webhooks: %v", err)
	}
	f := newEnv(t)
	f.svc.cfg.MaxEndpoints = 1
	f.endpoint(t)
	if _, _, err := f.svc.CreateEndpoint(ctx, f.tenant, f.actor, nil, Input{URL: "https://a.test/x", EventTypes: []string{outbox.EventInvoicePaid}}); !apperr.IsKind(err, apperr.KindLimitExceeded) {
		t.Fatalf("limite de endpoints: %v", err)
	}
	list, err := f.svc.Endpoints(ctx, f.tenant)
	if err != nil || len(list) != 1 {
		t.Fatalf("listagem: %v %v", list, err)
	}
}

func TestUpdateAndDeleteEndpoint(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.endpoint(t)
	in := Input{URL: "https://novo.cliente.test/hook", EventTypes: []string{outbox.EventInvoiceIssued}}
	if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "active", 7); !apperr.IsKind(err, apperr.KindPreconditionFailed) {
		t.Fatalf("If-Match divergente: %v", err)
	}
	got, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "disabled", ep.Version)
	if err != nil || got.URL != in.URL || got.Status != Disabled || got.Version != 2 {
		t.Fatalf("alteração: %+v %v", got, err)
	}
	if cur, err := e.svc.Endpoint(ctx, e.tenant, ep.ID); err != nil || cur.Version != 2 || cur.URL != in.URL || len(cur.Secrets) != 1 {
		t.Fatalf("consulta do endpoint: %+v %v", cur, err)
	}
	e.validator.err = errBlocked
	if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "active", 0); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("reativação revalida o destino: %v", err)
	}
	e.validator.err = nil
	if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "pausado", 0); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("situação inválida: %v", err)
	}
	if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, Input{URL: "x"}, "active", 0); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("dados inválidos: %v", err)
	}
	if err := e.svc.DeleteEndpoint(ctx, e.tenant, e.actor, ep.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := e.svc.Endpoints(ctx, e.tenant)
	if len(list) != 0 {
		t.Fatal("removido sai da listagem")
	}
	for name, fn := range map[string]func() error{
		"remoção repetida": func() error { return e.svc.DeleteEndpoint(ctx, e.tenant, e.actor, ep.ID) },
		"alteração": func() error {
			_, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "active", 0)
			return err
		},
		"rotação":  func() error { _, _, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, nil); return err },
		"entregas": func() error { _, err := e.svc.Deliveries(ctx, e.tenant, ep.ID, 10, ""); return err },
		"consulta": func() error { _, err := e.svc.Endpoint(ctx, e.tenant, ep.ID); return err },
	} {
		if err := fn(); !apperr.IsKind(err, apperr.KindNotFound) {
			t.Errorf("%s de endpoint removido: %v", name, err)
		}
	}
}

func TestRotateSecretKeepsPreviousDuringWindow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, first := e.endpoint(t)
	hours := 2
	rotated, second, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, &hours)
	if err != nil || second == first || len(rotated.Secrets) != 2 {
		t.Fatalf("rotação: %+v %v", rotated, err)
	}
	if rotated.Secrets[0].ExpiresAt != nil || !rotated.Secrets[1].ExpiresAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("validade dos segredos: %+v", rotated.Secrets)
	}
	e.publish(t, outbox.EventInvoicePaid)
	deliver(t, e)
	req := e.transport.requests[0]
	ts := req.Header[HeaderTimestamp]
	for _, s := range []string{first, second} {
		if err := Verify(s, req.Header[HeaderID], ts, req.Header[HeaderSignature], req.Body, t0, time.Minute); err != nil {
			t.Fatalf("durante a janela as duas assinaturas são enviadas: %v", err)
		}
	}
	e.clock.Advance(3 * time.Hour)
	e.publish(t, outbox.EventInvoicePaid)
	deliver(t, e)
	req = e.transport.requests[1]
	if len(strings.Fields(req.Header[HeaderSignature])) != 1 {
		t.Fatalf("após a janela somente o segredo novo: %q", req.Header[HeaderSignature])
	}
	if err := Verify(first, req.Header[HeaderID], req.Header[HeaderTimestamp], req.Header[HeaderSignature], req.Body, e.clock.Now(), time.Minute); err == nil {
		t.Fatal("segredo antigo expirado não deve conferir")
	}
	zero := 0
	_, third, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, &zero)
	if err != nil {
		t.Fatal(err)
	}
	now, _ := e.svc.Endpoints(ctx, e.tenant)
	if len(now[0].Secrets) != 1 || third == second {
		t.Fatalf("janela zero encerra o segredo anterior: %+v", now[0].Secrets)
	}
	bad := 169
	if _, _, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, &bad); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("janela acima do limite: %v", err)
	}
	if _, _, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, nil); err != nil {
		t.Fatalf("janela padrão: %v", err)
	}
}

func TestFanoutCreatesOneDeliveryPerSubscribedEndpoint(t *testing.T) {
	e := newEnv(t)
	a, _ := e.endpoint(t, outbox.EventInvoicePaid)
	e.endpoint(t, outbox.EventInvoicePaid, outbox.EventInvoiceIssued)
	e.endpoint(t, outbox.EventInvoiceIssued)
	ctx := context.Background()
	msg := e.publish(t, outbox.EventInvoicePaid)
	if err := e.svc.HandleEvent(ctx, e.tenant, msg, outbox.EventInvoicePaid, t0, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	ds := e.store.deliveries()
	if len(ds) != 2 {
		t.Fatalf("esperava 2 entregas: %d", len(ds))
	}
	for _, d := range ds {
		if d.EventID != msg || d.Status != Pending {
			t.Fatalf("entrega: %+v", d)
		}
	}
	if err := e.svc.HandleEvent(ctx, e.tenant, ids.New(), outbox.EventProviderWebhook, t0, nil); err != nil || len(e.store.deliveries()) != 2 {
		t.Fatal("evento interno não é entregue")
	}
	if err := e.svc.HandleEvent(ctx, e.tenant, ids.New(), outbox.EventCustomerCreated, time.Time{}, []byte("não json")); err != nil || len(e.store.deliveries()) != 2 {
		t.Fatal("evento sem assinantes não gera entregas")
	}
	_ = e.svc.DeleteEndpoint(ctx, e.tenant, e.actor, a.ID)
	e.publish(t, outbox.EventInvoicePaid)
	if len(e.store.deliveries()) != 3 {
		t.Fatal("endpoint removido não recebe novos eventos")
	}
	e.store.state.plan = platformbilling.Plan{}
	e.publish(t, outbox.EventInvoicePaid)
	if len(e.store.deliveries()) != 3 {
		t.Fatal("sem o recurso no plano não há fan-out")
	}
}

func TestDeliverSignsAndRecordsAttempts(t *testing.T) {
	e := newEnv(t)
	_, secret := e.endpoint(t)
	msg := e.publish(t, outbox.EventInvoicePaid)
	res := deliver(t, e)
	if res.Succeeded != 1 || len(e.transport.requests) != 1 {
		t.Fatalf("entrega: %+v", res)
	}
	req := e.transport.requests[0]
	if req.URL != "https://hooks.cliente.test/recorvia" || req.Header[HeaderID] != msg.String() || req.Header[HeaderEventType] != outbox.EventInvoicePaid ||
		req.Header[HeaderTimestamp] != strconv.FormatInt(t0.Unix(), 10) || req.Header["Content-Type"] != "application/json" {
		t.Fatalf("cabeçalhos: %+v", req.Header)
	}
	if err := Verify(secret, msg.String(), req.Header[HeaderTimestamp], req.Header[HeaderSignature], req.Body, t0, time.Minute); err != nil {
		t.Fatalf("assinatura: %v", err)
	}
	if !strings.Contains(string(req.Body), `"invoice_id":"abc"`) {
		t.Fatalf("corpo: %s", req.Body)
	}
	d := e.store.deliveries()[0]
	if d.Status != Succeeded || d.Attempts != 1 || *d.LastStatusCode != 204 {
		t.Fatalf("situação: %+v", d)
	}
	if a := e.store.state.attempts; len(a) != 1 || a[0].Number != 1 || *a[0].StatusCode != 204 || a[0].SignatureCount != 1 {
		t.Fatalf("histórico de tentativas: %+v", a)
	}
	if res := deliver(t, e); res.Succeeded != 0 {
		t.Fatal("entregue não é reenviada")
	}
}

func TestDeliverRetriesWithBackoffUntilExhausted(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t)
	e.publish(t, outbox.EventInvoicePaid)
	for i := range DefaultMaxAttempts {
		e.transport.responses = append(e.transport.responses, func() (int, error) { return 0, errors.New("conexão recusada") })
		if i == 2 {
			e.transport.responses[len(e.transport.responses)-1] = func() (int, error) { return 500, nil }
		}
	}
	bodies := map[string]bool{}
	for attempt := 1; attempt <= DefaultMaxAttempts; attempt++ {
		res := deliver(t, e)
		d := e.store.deliveries()[0]
		bodies[string(e.transport.requests[attempt-1].Body)] = true
		if attempt < DefaultMaxAttempts {
			if res.Retrying != 1 || d.Status != Retrying || !d.NextAttemptAt.Equal(e.clock.Now().Add(Backoff(attempt, 0.5))) {
				t.Fatalf("tentativa %d: %+v %+v", attempt, res, d)
			}
			e.clock.Set(*d.NextAttemptAt)
			continue
		}
		if res.Exhausted != 1 || d.Status != Exhausted {
			t.Fatalf("após o limite: %+v %+v", res, d)
		}
	}
	if len(bodies) != 1 {
		t.Fatal("o corpo deve ser idêntico em todas as tentativas")
	}
	if len(e.store.state.attempts) != DefaultMaxAttempts {
		t.Fatalf("histórico: %d tentativas", len(e.store.state.attempts))
	}
}

func TestDeliverPermanentAndClosedEndpoints(t *testing.T) {
	ctx := context.Background()
	t.Run("destino bloqueado", func(t *testing.T) {
		e := newEnv(t)
		e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		e.transport.responses = []func() (int, error){func() (int, error) { return 0, errBlocked }}
		if res := deliver(t, e); res.Exhausted != 1 || e.store.deliveries()[0].LastError != errBlocked.Error() {
			t.Fatalf("bloqueio é permanente: %+v", res)
		}
	})
	t.Run("endpoint desativado", func(t *testing.T) {
		e := newEnv(t)
		ep, _ := e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, Input{URL: ep.URL, EventTypes: ep.EventTypes}, "disabled", 0); err != nil {
			t.Fatal(err)
		}
		if res := deliver(t, e); res.Exhausted != 1 || len(e.transport.requests) != 0 || e.store.deliveries()[0].LastError != ReasonEndpointDisabled {
			t.Fatalf("endpoint desativado: %+v", res)
		}
	})
	t.Run("plano sem webhooks", func(t *testing.T) {
		e := newEnv(t)
		e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		e.store.state.plan = platformbilling.Plan{}
		if res := deliver(t, e); res.Exhausted != 1 || e.store.deliveries()[0].LastError != ReasonFeatureUnavailable {
			t.Fatalf("plano: %+v", res)
		}
	})
	t.Run("sem segredo válido", func(t *testing.T) {
		e := newEnv(t)
		ep, _ := e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		for id, s := range e.store.state.secrets {
			at := t0.Add(-time.Second)
			s.secret.ExpiresAt = &at
			e.store.state.secrets[id] = s
		}
		_ = ep
		if res := deliver(t, e); res.Exhausted != 1 || e.store.deliveries()[0].LastError != ReasonNoSecret {
			t.Fatalf("sem segredo: %+v", res)
		}
	})
	t.Run("outra instância assumiu", func(t *testing.T) {
		e := newEnv(t)
		e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		e.store.bump = true
		if res := deliver(t, e); res.LostLease != 1 || res.Succeeded != 0 {
			t.Fatalf("lease perdido: %+v", res)
		}
	})
}

func TestManualRetry(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.endpoint(t)
	e.publish(t, outbox.EventInvoicePaid)
	d := e.store.deliveries()[0]
	if _, err := e.svc.RetryDelivery(ctx, e.tenant, e.actor, d.ID); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("pendente não aceita reenvio: %v", err)
	}
	deliver(t, e)
	got, err := e.svc.RetryDelivery(ctx, e.tenant, e.actor, d.ID)
	if err != nil || got.Status != Pending || got.MaxAttempts != 2 {
		t.Fatalf("reenvio: %+v %v", got, err)
	}
	if res := deliver(t, e); res.Succeeded != 1 || len(e.transport.requests) != 2 {
		t.Fatalf("reenvio executado: %+v", res)
	}
	if e.transport.requests[0].Header[HeaderID] != e.transport.requests[1].Header[HeaderID] {
		t.Fatal("o id do evento é estável entre reenvios")
	}
	found := false
	for _, a := range e.store.state.audits {
		found = found || a.Action == "webhook_delivery.retried"
	}
	if !found {
		t.Fatal("reenvio manual auditado")
	}
	page, err := e.svc.Deliveries(ctx, e.tenant, ep.ID, 10, "")
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("listagem de entregas: %+v %v", page, err)
	}
	_, _ = e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, Input{URL: ep.URL, EventTypes: ep.EventTypes}, "disabled", 0)
	if _, err := e.svc.RetryDelivery(ctx, e.tenant, e.actor, d.ID); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("endpoint desativado: %v", err)
	}
	if _, err := e.svc.RetryDelivery(ctx, e.tenant, e.actor, ids.New()); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("entrega inexistente: %v", err)
	}
}

func TestServiceErrorsPropagate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.transport = nil
	if _, err := e.svc.Deliver(ctx); err == nil {
		t.Fatal("sem transporte deve falhar")
	}
	for _, op := range []string{"TenantsWithDueDeliveries", "PlatformPlan", "DueDeliveries", "Endpoint", "Secrets", "Event", "UpdateDelivery",
		"Delivery", "InsertAttempt"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.endpoint(t)
			e.publish(t, outbox.EventInvoicePaid)
			e.store.fail[op] = errInjected
			if _, err := e.svc.Deliver(ctx); !errors.Is(err, errInjected) {
				t.Fatalf("erro em %s deve propagar: %v", op, err)
			}
		})
	}
	for _, op := range []string{"MarkInbox", "PlatformPlan", "SubscribedEndpoints", "InsertEvent", "InsertDelivery"} {
		e := newEnv(t)
		e.endpoint(t)
		e.store.fail[op] = errInjected
		if err := e.svc.HandleEvent(ctx, e.tenant, ids.New(), outbox.EventInvoicePaid, t0, nil); !errors.Is(err, errInjected) {
			t.Fatalf("fan-out com erro em %s: %v", op, err)
		}
	}
	for _, op := range []string{"CountEndpoints", "InsertEndpoint", "InsertSecret", "Audit"} {
		e := newEnv(t)
		e.store.fail[op] = errInjected
		if _, _, err := e.svc.CreateEndpoint(ctx, e.tenant, e.actor, nil, Input{URL: "https://a.test/x", EventTypes: []string{outbox.EventInvoicePaid}}); !errors.Is(err, errInjected) {
			t.Fatalf("criação com erro em %s: %v", op, err)
		}
	}
	for _, op := range []string{"ExpireSecrets", "InsertSecret", "PlatformPlan"} {
		e := newEnv(t)
		ep, _ := e.endpoint(t)
		e.store.fail[op] = errInjected
		if _, _, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, nil); !errors.Is(err, errInjected) {
			t.Fatalf("rotação com erro em %s: %v", op, err)
		}
	}
	for _, op := range []string{"UpdateEndpoint", "Endpoint"} {
		e := newEnv(t)
		ep, _ := e.endpoint(t)
		e.store.fail[op] = errInjected
		if err := e.svc.DeleteEndpoint(ctx, e.tenant, e.actor, ep.ID); !errors.Is(err, errInjected) {
			t.Fatalf("remoção com erro em %s: %v", op, err)
		}
		if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, Input{URL: ep.URL, EventTypes: ep.EventTypes}, "active", 0); !errors.Is(err, errInjected) {
			t.Fatalf("alteração com erro em %s: %v", op, err)
		}
	}
	for _, op := range []string{"Endpoints"} {
		e := newEnv(t)
		e.store.fail[op] = errInjected
		if _, err := e.svc.Endpoints(ctx, e.tenant); !errors.Is(err, errInjected) {
			t.Fatal("listagem com erro")
		}
	}
	for _, op := range []string{"Delivery", "UpdateDelivery", "PlatformPlan"} {
		e := newEnv(t)
		e.endpoint(t)
		e.publish(t, outbox.EventInvoicePaid)
		deliver(t, e)
		e.store.fail[op] = errInjected
		if _, err := e.svc.RetryDelivery(ctx, e.tenant, e.actor, e.store.deliveries()[0].ID); !errors.Is(err, errInjected) {
			t.Fatalf("reenvio com erro em %s: %v", op, err)
		}
	}
	ce := newEnv(t)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ce.svc.Deliver(cctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelamento")
	}
}

func TestNewServiceDefaults(t *testing.T) {
	s := NewService(newFakeStore(), nil, nil, clock.NewManual(t0), Config{Lease: time.Minute, Timeout: time.Hour})
	if s.cfg.Timeout != 30*time.Second || s.cfg.MaxEndpoints != DefaultMaxEndpoints || s.cfg.BatchSize != 50 || s.cfg.IsPermanent(errInjected) ||
		s.cfg.UserAgent == "" || s.cfg.Jitter() < 0 {
		t.Fatalf("padrões: %+v", s.cfg)
	}
	if d := NewService(newFakeStore(), nil, nil, clock.NewManual(t0), Config{}); d.cfg.Lease != time.Minute {
		t.Fatal("lease padrão")
	}
	if err := s.checkDestination(context.Background(), "https://a.test"); err != nil {
		t.Fatal("sem validador não há verificação de DNS")
	}
}

// Desativar um endpoint ativo é sempre permitido, inclusive depois de um
// downgrade para plano sem o recurso; reativá-lo exige o recurso.
func TestDisableEndpointWithoutFeature(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, _ := e.endpoint(t)
	e.store.state.plan = platformbilling.Plan{}
	in := Input{URL: ep.URL, Description: ep.Description, EventTypes: ep.EventTypes}
	got, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "disabled", ep.Version)
	if err != nil || got.Status != Disabled {
		t.Fatalf("desativação sem o recurso: %+v %v", got, err)
	}
	if _, err := e.svc.UpdateEndpoint(ctx, e.tenant, e.actor, ep.ID, in, "active", got.Version); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("reativação exige o recurso: %v", err)
	}
}

// A criação bloqueia a contagem de endpoints do tenant (concorrência coberta
// no teste de integração).
func TestCreateEndpointLocksBeforeCounting(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t)
	if e.store.locks != 1 {
		t.Fatalf("criação sem bloqueio: %d", e.store.locks)
	}
	e.store.fail["LockEndpoints"] = errInjected
	if _, _, err := e.svc.CreateEndpoint(context.Background(), e.tenant, e.actor, nil, Input{URL: "https://a.test/x",
		EventTypes: []string{outbox.EventInvoicePaid}}); !errors.Is(err, errInjected) {
		t.Fatalf("erro do bloqueio: %v", err)
	}
}

// A janela da rotação é validada antes da conversão em duração (sem
// estouro da multiplicação).
func TestRotateSecretRejectsWindowsOutsideTheLimit(t *testing.T) {
	e := newEnv(t)
	ep, _ := e.endpoint(t)
	for _, h := range []int{-1, 169, 5124096, math.MaxInt} {
		if _, _, err := e.svc.RotateSecret(context.Background(), e.tenant, e.actor, ep.ID, &h); !apperr.IsKind(err, apperr.KindInvalid) {
			t.Errorf("janela de %d h deve ser recusada: %v", h, err)
		}
	}
	if n := len(e.store.state.secrets); n != 1 {
		t.Fatalf("nenhuma rotação deve ocorrer: %d segredos", n)
	}
}

// Rotações seguidas mantêm no máximo MaxSignatures segredos válidos: a
// listagem mostra exatamente os segredos que assinam as entregas.
func TestRotateSecretKeepsAtMostMaxSignatures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ep, first := e.endpoint(t)
	values := []string{first}
	var listed Endpoint
	for range 6 {
		e.clock.Advance(time.Minute)
		var v string
		var err error
		if listed, v, err = e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, nil); err != nil {
			t.Fatal(err)
		}
		values = append(values, v)
	}
	if len(listed.Secrets) != MaxSignatures {
		t.Fatalf("segredos válidos listados: %d", len(listed.Secrets))
	}
	e.publish(t, outbox.EventInvoicePaid)
	deliver(t, e)
	req := e.transport.requests[0]
	if n := len(strings.Fields(req.Header[HeaderSignature])); n != MaxSignatures {
		t.Fatalf("assinaturas enviadas: %d", n)
	}
	for i, v := range values {
		err := Verify(v, req.Header[HeaderID], req.Header[HeaderTimestamp], req.Header[HeaderSignature], req.Body, e.clock.Now(), time.Minute)
		if listedNow := i >= len(values)-MaxSignatures; (err == nil) != listedNow {
			t.Errorf("segredo %d: listado=%v, verificação=%v", i, listedNow, err)
		}
	}
	e.store.fail["RetainSecrets"] = errInjected
	if _, _, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, ep.ID, nil); !errors.Is(err, errInjected) {
		t.Fatalf("erro ao encerrar segredos excedentes: %v", err)
	}
}

// O resultado da tentativa é gravado mesmo quando o job é cancelado logo
// após a resposta do destino.
func TestDeliverRecordsResultAfterJobCancellation(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t)
	e.publish(t, outbox.EventInvoicePaid)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.transport.responses = []func() (int, error){func() (int, error) { cancel(); return 204, nil }}
	res, err := e.svc.Deliver(ctx)
	if err != nil || res.Succeeded != 1 {
		t.Fatalf("entrega aceita deve ser registrada: %+v %v", res, err)
	}
	if d := e.store.deliveries()[0]; d.Status != Succeeded || len(e.store.state.attempts) != 1 {
		t.Fatalf("situação após o cancelamento: %+v", d)
	}
	e.clock.Advance(2 * time.Minute) // além do lease
	if res := deliver(t, e); res.Succeeded != 0 || e.transport.count() != 1 {
		t.Fatalf("a entrega não pode ser repetida: %+v", res)
	}
}

// Com o job cancelado, as entregas reivindicadas e ainda não tentadas voltam
// à fila sem consumir tentativa.
func TestDeliverReleasesClaimedDeliveriesOnCancellation(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t)
	for range 3 {
		e.publish(t, outbox.EventInvoicePaid)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.transport.responses = []func() (int, error){func() (int, error) { cancel(); return 204, nil }}
	res, err := e.svc.Deliver(ctx)
	if err != nil || res.Succeeded != 1 || res.Deferred != 2 || e.transport.count() != 1 {
		t.Fatalf("resultado: %+v %v", res, err)
	}
	for _, d := range e.store.deliveries() {
		if d.Status != Succeeded && (d.Status != Pending || d.Attempts != 0 || !d.Due(e.clock.Now())) {
			t.Fatalf("entrega não tentada deve voltar pendente: %+v", d)
		}
	}
	if res := deliver(t, e); res.Succeeded != 2 {
		t.Fatalf("a execução seguinte entrega as devolvidas: %+v", res)
	}
}

// Um segredo que não decifra afeta somente as entregas do seu endpoint: sem
// nenhum segredo utilizável, a entrega é encerrada com o motivo; com outro
// segredo válido, ela é assinada só com ele.
func TestDeliverIsolatesUnreadableSecrets(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	create := func(url string) (Endpoint, string) {
		ep, secret, err := e.svc.CreateEndpoint(ctx, e.tenant, e.actor, nil, Input{URL: url, EventTypes: []string{outbox.EventInvoicePaid}})
		if err != nil {
			t.Fatal(err)
		}
		return ep, secret
	}
	broken, _ := create("https://quebrado.cliente.test/hook")
	partial, _ := create("https://parcial.cliente.test/hook")
	healthy, healthySecret := create("https://saudavel.cliente.test/hook")
	e.clock.Advance(time.Minute)
	_, current, err := e.svc.RotateSecret(ctx, e.tenant, e.actor, partial.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, sec := range e.store.state.secrets {
		if sec.endpoint == broken.ID || (sec.endpoint == partial.ID && sec.secret.ExpiresAt != nil) {
			sec.unreadable = true
			e.store.state.secrets[id] = sec
		}
	}
	e.publish(t, outbox.EventInvoicePaid)
	if res := deliver(t, e); res.Succeeded != 2 || res.Exhausted != 1 {
		t.Fatalf("resultado: %+v", res)
	}
	for _, d := range e.store.deliveries() {
		if d.EndpointID == broken.ID && (d.Status != Exhausted || d.LastError != ReasonSecretUnavailable) {
			t.Fatalf("endpoint sem segredo legível: %+v", d)
		}
		if d.EndpointID != broken.ID && d.Status != Succeeded {
			t.Fatalf("endpoint %s deveria receber: %+v", d.EndpointID, d)
		}
	}
	secrets := map[string]string{partial.URL: current, healthy.URL: healthySecret}
	for _, req := range e.transport.requests {
		sig := req.Header[HeaderSignature]
		if strings.Count(sig, "v1,") != 1 {
			t.Fatalf("%s: somente o segredo legível assina: %q", req.URL, sig)
		}
		if err := Verify(secrets[req.URL], req.Header[HeaderID], req.Header[HeaderTimestamp], sig, req.Body, e.clock.Now(), time.Minute); err != nil {
			t.Fatalf("%s: assinatura: %v", req.URL, err)
		}
	}
}

// Disjuntor por endpoint: após tentativas seguidas sem resposta, as demais
// entregas do endpoint na execução são adiadas sem consumir tentativa.
func TestDeliverBreakerDefersEndpointWithoutResponse(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t)
	for range 5 {
		e.publish(t, outbox.EventInvoicePaid)
	}
	for range DefaultBreakerThreshold {
		e.transport.responses = append(e.transport.responses, func() (int, error) { return 0, errors.New("tempo esgotado") })
	}
	res := deliver(t, e)
	if res.Retrying != DefaultBreakerThreshold || res.Deferred != 2 || e.transport.count() != DefaultBreakerThreshold {
		t.Fatalf("disjuntor: %+v (%d requisições)", res, e.transport.count())
	}
	pause := e.clock.Now().Add(DefaultBreakerPause)
	deferred := 0
	for _, d := range e.store.deliveries() {
		if d.Status == Pending {
			deferred++
			if d.Attempts != 0 || !d.NextAttemptAt.Equal(pause) {
				t.Fatalf("entrega adiada: %+v", d)
			}
		}
	}
	if deferred != 2 {
		t.Fatalf("entregas adiadas: %d", deferred)
	}
	e.clock.Set(pause)
	if res := deliver(t, e); res.Succeeded != 5 {
		t.Fatalf("após a pausa todas são tentadas: %+v", res)
	}
}

// Orçamento por tenant: esgotado o tempo, as entregas restantes voltam à
// fila (sem consumir tentativa) e a execução segue para os outros tenants.
func TestDeliverTenantBudget(t *testing.T) {
	e := newEnv(t)
	e.svc.cfg.TenantBudget = 25 * time.Second
	e.endpoint(t)
	for range 5 {
		e.publish(t, outbox.EventInvoicePaid)
	}
	for range 5 {
		e.transport.responses = append(e.transport.responses, func() (int, error) { e.clock.Advance(10 * time.Second); return 204, nil })
	}
	res := deliver(t, e)
	if res.Succeeded != 3 || res.Deferred != 2 {
		t.Fatalf("orçamento do tenant: %+v", res)
	}
	if res := deliver(t, e); res.Succeeded != 2 {
		t.Fatalf("a execução seguinte entrega as restantes: %+v", res)
	}
}

// Um tenant com destinos que nunca respondem não impede as entregas dos
// demais tenants na mesma execução (orçamento por tenant).
func TestDeliverIsolatesSlowTenants(t *testing.T) {
	clk := clock.NewManual(t0)
	slow, fast := ids.New(), ids.New()
	if bytes.Compare(slow[:], fast[:]) > 0 {
		slow, fast = fast, slow // o tenant lento vem primeiro na ordem de descoberta
	}
	stores := routerStore{slow: newFakeStore(), fast: newFakeStore()}
	transport := &fakeTransport{byHost: map[string]func(context.Context) (int, error){
		"lento.cliente.test": func(ctx context.Context) (int, error) {
			<-ctx.Done() // destino que aceita a conexão e nunca responde
			clk.Advance(10 * time.Second)
			return 0, ctx.Err()
		},
	}}
	svc := NewService(stores, transport, &fakeValidator{}, clk, Config{RequireHTTPS: true, Lease: time.Minute, Timeout: 100 * time.Millisecond,
		Jitter: func() float64 { return 0.5 }})
	ctx := context.Background()
	actor := audit.System("teste")
	for i := range 10 {
		if _, _, err := svc.CreateEndpoint(ctx, slow, actor, nil, Input{URL: fmt.Sprintf("https://lento.cliente.test/hook-%d", i),
			EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := svc.CreateEndpoint(ctx, fast, actor, nil, Input{URL: "https://rapido.cliente.test/hook",
		EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
		t.Fatal(err)
	}
	for _, tn := range []uuid.UUID{slow, slow, slow, slow, slow, fast} {
		if err := svc.HandleEvent(ctx, tn, ids.New(), outbox.EventInvoicePaid, t0, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Sem o orçamento, as 50 entregas lentas (100 ms cada) esgotariam o prazo
	// da execução antes do tenant rápido.
	jctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := svc.Deliver(jctx)
	if err != nil {
		t.Fatalf("execução: %+v %v", res, err)
	}
	if d := stores[fast].deliveries(); len(d) != 1 || d[0].Status != Succeeded {
		t.Fatalf("o tenant rápido deve receber na mesma execução: %+v", d)
	}
	if res.Retrying != 2 || res.Deferred != 48 {
		t.Fatalf("o tenant lento usa só o seu orçamento: %+v", res)
	}
}

// Orçamento da execução: esgotado, os tenants restantes ficam para a
// execução seguinte, que começa por eles; a descoberta é paginada.
func TestDeliverRunBudgetRotatesTenants(t *testing.T) {
	clk := clock.NewManual(t0)
	tenants := []uuid.UUID{ids.New(), ids.New(), ids.New()}
	stores := routerStore{}
	for _, id := range tenants {
		stores[id] = newFakeStore()
	}
	transport := &fakeTransport{byHost: map[string]func(context.Context) (int, error){
		"cliente.test": func(context.Context) (int, error) { clk.Advance(40 * time.Second); return 204, nil },
	}}
	svc := NewService(stores, transport, &fakeValidator{}, clk, Config{RequireHTTPS: true, Lease: time.Minute, Timeout: 10 * time.Second,
		RunBudget: time.Minute, TenantPageSize: 1})
	ctx := context.Background()
	for _, id := range tenants {
		if _, _, err := svc.CreateEndpoint(ctx, id, audit.System("teste"), nil, Input{URL: "https://cliente.test/hook",
			EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
			t.Fatal(err)
		}
		if err := svc.HandleEvent(ctx, id, ids.New(), outbox.EventInvoicePaid, t0, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := svc.Deliver(ctx); err != nil || res.Succeeded != 2 {
		t.Fatalf("primeira execução para no orçamento: %+v %v", res, err)
	}
	if res, err := svc.Deliver(ctx); err != nil || res.Succeeded != 1 {
		t.Fatalf("segunda execução começa pelo tenant restante: %+v %v", res, err)
	}
	for _, id := range tenants {
		if d := stores[id].deliveries(); d[0].Status != Succeeded {
			t.Fatalf("tenant %s sem entrega: %+v", id, d)
		}
	}
}
