//go:build integration

package pgstore_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// receiver é o sistema do tenant que recebe os webhooks.
type receiver struct {
	srv      *httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	bodies   [][]byte
	status   atomic.Int32
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(http.StatusNoContent)
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, req.Clone(context.Background()))
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *receiver) port() string {
	u, _ := url.Parse(r.srv.URL)
	return u.Port()
}

// staticResolver resolve nomes de teste para endereços fixos, em sequência.
type staticResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	next    map[string][]netip.Addr
}

func (s *staticResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.next[host]; ok {
		delete(s.next, host)
		return n, nil
	}
	if a, ok := s.answers[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

type transport struct{ c *safehttp.Client }

func (t transport) Post(ctx context.Context, r webhooks.Request) (int, error) {
	return t.c.Post(ctx, r.URL, r.Header, r.Body)
}

// webhookService monta o serviço com o cliente protegido contra SSRF real.
func webhookService(env *fixture.Env, client *safehttp.Client) *webhooks.Service {
	return webhooks.NewService(env.Store.Webhooks(), transport{client}, client, env.Clock, webhooks.Config{Lease: time.Minute,
		Timeout: 5 * time.Second, IsPermanent: func(err error) bool { return errors.Is(err, safehttp.ErrBlocked) }, Jitter: func() float64 { return 0.5 }})
}

func webhookTenant(t *testing.T, env *fixture.Env, prefix string) fixture.Tenant {
	t.Helper()
	tn := env.NewTenant(t, prefix)
	if _, err := env.Platform.ChangePlan(context.Background(), tn.ID, tn.Actor, "profissional", env.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	return tn
}

var loopbackOnly = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}

// Entrega real: o receptor confere a assinatura HMAC com o segredo exibido
// na criação; durante a janela de rotação as duas assinaturas conferem; o
// segredo fica cifrado em repouso.
func TestWebhookDeliverySignatureAndRotation(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-assina")
	rcv := newReceiver(t)
	res := &staticResolver{answers: map[string][]netip.Addr{"hooks.tenant.test": {netip.MustParseAddr("127.0.0.1")}}}
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}, Resolver: res})
	svc := webhookService(env, client)
	ep, secret, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, &tn.Owner.ID, webhooks.Input{URL: "http://hooks.tenant.test:" + rcv.port() + "/recorvia",
		EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM webhook_endpoint_secrets WHERE tenant_id = $1 AND position($2::bytea IN secret_ciphertext) > 0`,
		tn.ID, []byte(secret)); n != 0 {
		t.Fatal("segredo em claro no banco")
	}
	evt := ids.New()
	if err := svc.HandleEvent(ctx, tn.ID, evt, outbox.EventInvoicePaid, now, []byte(`{"invoice_id":"x","amount_due":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, evt, outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
		t.Fatal(err) // reentrega da mesma mensagem: deduplicada pelo inbox
	}
	if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if rcv.count() != 1 {
		t.Fatalf("entregas recebidas = %d", rcv.count())
	}
	r, body := rcv.requests[0], rcv.bodies[0]
	if r.Header.Get(webhooks.HeaderID) != evt.String() || r.Header.Get(webhooks.HeaderEventType) != outbox.EventInvoicePaid ||
		r.Header.Get("Content-Type") != "application/json" || r.Host != "hooks.tenant.test:"+rcv.port() {
		t.Fatalf("cabeçalhos: %v", r.Header)
	}
	if err := webhooks.Verify(secret, r.Header.Get(webhooks.HeaderID), r.Header.Get(webhooks.HeaderTimestamp), r.Header.Get(webhooks.HeaderSignature),
		body, now, 5*time.Minute); err != nil {
		t.Fatalf("assinatura recebida: %v", err)
	}
	hours := 1
	rotated, newSecret, err := svc.RotateSecret(ctx, tn.ID, tn.Actor, ep.ID, &hours, ep.Version)
	if err != nil || len(rotated.Secrets) != 2 || rotated.Secrets[0].ExpiresAt != nil {
		t.Fatalf("rotação: %+v %v", rotated, err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	r, body = rcv.requests[1], rcv.bodies[1]
	for _, s := range []string{secret, newSecret} {
		if err := webhooks.Verify(s, r.Header.Get(webhooks.HeaderID), r.Header.Get(webhooks.HeaderTimestamp), r.Header.Get(webhooks.HeaderSignature),
			body, now, 5*time.Minute); err != nil {
			t.Fatalf("durante a janela as duas assinaturas devem conferir: %v", err)
		}
	}
	env.Clock.Advance(2 * time.Hour)
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, env.Clock.Now(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	r = rcv.requests[2]
	if len(strings.Fields(r.Header.Get(webhooks.HeaderSignature))) != 1 {
		t.Fatalf("após a janela somente o segredo novo assina: %q", r.Header.Get(webhooks.HeaderSignature))
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM webhook_delivery_attempts WHERE tenant_id = $1 AND status_code = 204`, tn.ID); n != 3 {
		t.Fatalf("histórico de tentativas: %d", n)
	}
	page, err := svc.Deliveries(ctx, tn.ID, ep.ID, 2, "")
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("paginação das entregas: %+v %v", page, err)
	}
	rest, err := svc.Deliveries(ctx, tn.ID, ep.ID, 2, page.NextCursor)
	if err != nil || len(rest.Items) != 1 || rest.NextCursor != "" {
		t.Fatalf("segunda página: %+v %v", rest, err)
	}
	// Outro tenant não enxerga o endpoint nem as entregas.
	other := webhookTenant(t, env, "wh-outro")
	if _, err := svc.Deliveries(ctx, other.ID, ep.ID, 10, ""); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("isolamento: %v", err)
	}
}

// Proteção SSRF ponta a ponta: destinos internos são recusados na criação;
// um nome que passa a resolver para loopback (rebinding) é bloqueado na
// conexão e a entrega se encerra sem tocar o destino; redirecionamentos não
// são seguidos.
func TestWebhookSSRFProtection(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-ssrf")
	rcv := newReceiver(t)
	res := &staticResolver{answers: map[string][]netip.Addr{
		"publico.tenant.test":  {netip.MustParseAddr("93.184.216.34")},
		"interno.tenant.test":  {netip.MustParseAddr("10.0.0.7")},
		"metadata.tenant.test": {netip.MustParseAddr("169.254.169.254")},
	}, next: map[string][]netip.Addr{}}
	strict := webhookService(env, safehttp.New(safehttp.Options{Resolver: res}))
	for _, u := range []string{rcv.srv.URL + "/x", "http://interno.tenant.test/x", "http://metadata.tenant.test/latest", "http://[::1]:8080/x",
		"http://169.254.169.254/latest/meta-data"} {
		_, _, err := strict.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: u, EventTypes: []string{outbox.EventInvoicePaid}})
		if ae, ok := apperr.As(err); !ok || ae.Code != "webhook_destination_not_allowed" {
			t.Errorf("%s deveria ser recusado: %v", u, err)
		}
	}
	// Criado com resolução pública; na entrega o nome resolve para loopback.
	ep, _, err := strict.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: "http://publico.tenant.test:" + rcv.port() + "/x",
		EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	res.mu.Lock()
	res.answers["publico.tenant.test"] = []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	res.mu.Unlock()
	if err := strict.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	out, err := strict.DeliverTenant(ctx, tn.ID)
	if err != nil || out.Exhausted != 1 || rcv.count() != 0 {
		t.Fatalf("rebinding deve encerrar a entrega sem tocar o destino: %+v %v (requisições: %d)", out, err, rcv.count())
	}
	var lastErr string
	if err := env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT last_error FROM webhook_deliveries WHERE tenant_id = $1 AND endpoint_id = $2`, tn.ID, ep.ID).Scan(&lastErr)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastErr, "destino não permitido") {
		t.Fatalf("motivo registrado: %q", lastErr)
	}
	// Redirecionamento (exceção explícita para loopback): 3xx não é seguido.
	final := newReceiver(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.srv.URL+"/destino", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	local := webhookService(env, safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}}))
	rep, _, err := local.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: redirect.URL + "/hook", EventTypes: []string{outbox.EventInvoiceIssued}})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoiceIssued, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if out, err := local.DeliverTenant(ctx, tn.ID); err != nil || out.Retrying != 1 || final.count() != 0 {
		t.Fatalf("redirecionamento não seguido e repetido depois: %+v %v (destino: %d)", out, err, final.count())
	}
	page, _ := local.Deliveries(ctx, tn.ID, rep.ID, 10, "")
	if d := page.Items[0]; d.Status != webhooks.Retrying || *d.LastStatusCode != http.StatusTemporaryRedirect || d.LastError != "redirecionamento não seguido" {
		t.Fatalf("entrega após redirecionamento: %+v", d)
	}
}

// Falha do destino: retentativa com backoff, reenvio manual auditado e
// entregas concorrentes sem duplicidade.
func TestWebhookRetriesManualRetryAndConcurrency(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 16, now)
	tn := webhookTenant(t, env, "wh-retry")
	rcv := newReceiver(t)
	rcv.status.Store(http.StatusServiceUnavailable)
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}})
	svc := webhookService(env, client)
	ep, _, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: rcv.srv.URL + "/hook", EventTypes: []string{outbox.EventChargeCreated}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventChargeCreated, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil {
				t.Errorf("entrega concorrente: %v", err)
			}
		})
	}
	wg.Wait()
	if rcv.count() != 1 {
		t.Fatalf("entregas concorrentes devem resultar em uma tentativa: %d", rcv.count())
	}
	page, _ := svc.Deliveries(ctx, tn.ID, ep.ID, 10, "")
	d := page.Items[0]
	if d.Status != webhooks.Retrying || d.Attempts != 1 || !d.NextAttemptAt.Equal(now.Add(webhooks.Backoff(1, 0.5))) || *d.LastStatusCode != 503 {
		t.Fatalf("retentativa agendada: %+v", d)
	}
	if out, _ := svc.DeliverTenant(ctx, tn.ID); out.Retrying+out.Succeeded != 0 {
		t.Fatal("antes do backoff não há nova tentativa")
	}
	rcv.status.Store(http.StatusOK)
	got, err := svc.RetryDelivery(ctx, tn.ID, tn.Actor, d.ID)
	if err != nil || got.Status != webhooks.Pending {
		t.Fatalf("reenvio manual: %+v %v", got, err)
	}
	if out, err := svc.DeliverTenant(ctx, tn.ID); err != nil || out.Succeeded != 1 {
		t.Fatalf("reenvio executado: %+v %v", out, err)
	}
	if rcv.requests[0].Header.Get(webhooks.HeaderID) != rcv.requests[1].Header.Get(webhooks.HeaderID) {
		t.Fatal("id do evento estável entre tentativas")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'webhook_delivery.retried'`, tn.ID); n != 1 {
		t.Fatal("reenvio manual auditado")
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM webhook_delivery_attempts WHERE tenant_id = $1`, tn.ID); n != 2 {
		t.Fatalf("histórico de tentativas: %d", n)
	}
	// Endpoint removido: entregas pendentes se encerram e ele sai da listagem.
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventChargeCreated, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteEndpoint(ctx, tn.ID, tn.Actor, ep.ID, ep.Version); err != nil {
		t.Fatal(err)
	}
	if out, err := svc.DeliverTenant(ctx, tn.ID); err != nil || out.Exhausted != 1 || rcv.count() != 2 {
		t.Fatalf("entrega de endpoint removido: %+v %v", out, err)
	}
	if list, _ := svc.Endpoints(ctx, tn.ID); len(list) != 0 {
		t.Fatal("endpoint removido não aparece na listagem")
	}
	// Plano sem webhooks: criação negada.
	basic := env.NewTenant(t, "wh-basico")
	if _, _, err := svc.CreateEndpoint(ctx, basic.ID, basic.Actor, nil, webhooks.Input{URL: rcv.srv.URL, EventTypes: []string{outbox.EventChargeCreated}}); !apperr.IsKind(err, apperr.KindForbidden) {
		t.Fatalf("plano sem webhooks: %v", err)
	}
}

type pausedEndpoints struct {
	*pgstore.Webhooks
	pause *pauseUntilPeer
}

func (s pausedEndpoints) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(webhooks.Tx) error) error {
	return s.Webhooks.InTenant(ctx, tenantID, func(tx webhooks.Tx) error { return fn(pausedEndpointsTx{tx, s.pause}) })
}

type pausedEndpointsTx struct {
	webhooks.Tx
	pause *pauseUntilPeer
}

func (tx pausedEndpointsTx) CountEndpoints(ctx context.Context) (int, error) {
	n, err := tx.Tx.CountEndpoints(ctx)
	if err != nil {
		return 0, err
	}
	return n, tx.pause.arrive(ctx)
}

// Duas criações simultâneas com o tenant a um endpoint do limite: a
// contagem é serializada por tenant e somente uma é aceita.
func TestWebhookEndpointLimitUnderConcurrentCreation(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-limite")
	plain := webhooks.NewService(env.Store.Webhooks(), nil, nil, env.Clock, webhooks.Config{})
	in := webhooks.Input{URL: "https://hooks.cliente.test/x", EventTypes: []string{outbox.EventInvoicePaid}}
	for range webhooks.DefaultMaxEndpoints - 1 {
		if _, _, err := plain.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, in); err != nil {
			t.Fatal(err)
		}
	}
	svc := webhooks.NewService(pausedEndpoints{env.Store.Webhooks(), newPauseUntilPeer(pgtest.OwnerPool(t))}, nil, nil, env.Clock, webhooks.Config{})
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Go(func() { _, _, errs[i] = svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, in) })
	}
	wg.Wait()
	ok, limited := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case apperr.IsKind(err, apperr.KindLimitExceeded):
			limited++
		default:
			t.Fatalf("criação concorrente: %v", err)
		}
	}
	if ok != 1 || limited != 1 {
		t.Fatalf("aceitas %d, recusadas pelo limite %d", ok, limited)
	}
	if n := countRows(t, env, tn.ID, `SELECT count(*) FROM webhook_endpoints WHERE tenant_id = $1 AND deleted_at IS NULL`, tn.ID); n != webhooks.DefaultMaxEndpoints {
		t.Fatalf("endpoints gravados: %d", n)
	}
}

// Rotações seguidas no mesmo dia: a listagem mostra exatamente os segredos
// que assinam as entregas (no máximo cinco), e todos eles conferem.
func TestWebhookRotationListingMatchesSigningSecrets(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-rotacoes")
	rcv := newReceiver(t)
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}})
	svc := webhookService(env, client)
	ep, first, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: rcv.srv.URL + "/hook", EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	values := []string{first}
	version := ep.Version
	for range 6 {
		env.Clock.Advance(time.Minute)
		rotated, v, err := svc.RotateSecret(ctx, tn.ID, tn.Actor, ep.ID, nil, version)
		if err != nil {
			t.Fatal(err)
		}
		values, version = append(values, v), rotated.Version
	}
	got, err := svc.Endpoint(ctx, tn.ID, ep.ID)
	if err != nil || len(got.Secrets) != webhooks.MaxSignatures {
		t.Fatalf("segredos válidos listados: %+v %v", got.Secrets, err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, env.Clock.Now(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil || rcv.count() != 1 {
		t.Fatalf("entrega: %v (%d recebidas)", err, rcv.count())
	}
	r, body := rcv.requests[0], rcv.bodies[0]
	sig := r.Header.Get(webhooks.HeaderSignature)
	if n := len(strings.Fields(sig)); n != webhooks.MaxSignatures {
		t.Fatalf("assinaturas enviadas: %d", n)
	}
	for i, v := range values {
		err := webhooks.Verify(v, r.Header.Get(webhooks.HeaderID), r.Header.Get(webhooks.HeaderTimestamp), sig, body, env.Clock.Now(), 5*time.Minute)
		if listed := i >= len(values)-webhooks.MaxSignatures; (err == nil) != listed {
			t.Errorf("segredo %d: listado=%v, verificação=%v", i, listed, err)
		}
	}
}

// Um segredo que não decifra (chave retirada do keyring) encerra somente as
// entregas do seu endpoint; os demais endpoints do tenant recebem.
func TestWebhookUnreadableSecretAffectsOnlyItsEndpoint(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-ilegivel")
	rcv := newReceiver(t)
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}})
	svc := webhookService(env, client)
	broken, _, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: rcv.srv.URL + "/quebrado", EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: rcv.srv.URL + "/saudavel", EventTypes: []string{outbox.EventInvoicePaid}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `UPDATE webhook_endpoint_secrets SET secret_key_id = 'chave-retirada' WHERE tenant_id = $1 AND endpoint_id = $2`,
		tn.ID, broken.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	res, err := svc.DeliverTenant(ctx, tn.ID)
	if err != nil || res.Succeeded != 1 || res.Exhausted != 1 {
		t.Fatalf("entregas: %+v %v", res, err)
	}
	if rcv.count() != 1 || rcv.requests[0].URL.Path != "/saudavel" {
		t.Fatalf("somente o endpoint com segredo legível recebe: %d", rcv.count())
	}
	page, err := svc.Deliveries(ctx, tn.ID, broken.ID, 10, "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != webhooks.Exhausted || page.Items[0].LastError != webhooks.ReasonSecretUnavailable {
		t.Fatalf("entrega do endpoint com segredo ilegível: %+v %v", page.Items, err)
	}
}

// O reenvio manual para no limite total de tentativas com erro de negócio,
// sem violar a restrição de max_attempts; desativar um endpoint ativo não
// exige o recurso do plano.
func TestWebhookManualRetryLimitAndDisableAfterDowngrade(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 8, now)
	tn := webhookTenant(t, env, "wh-limite-reenvio")
	rcv := newReceiver(t)
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}})
	svc := webhookService(env, client)
	ep, _, err := svc.CreateEndpoint(ctx, tn.ID, tn.Actor, nil, webhooks.Input{URL: rcv.srv.URL + "/hook", EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleEvent(ctx, tn.ID, ids.New(), outbox.EventInvoicePaid, now, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeliverTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	page, _ := svc.Deliveries(ctx, tn.ID, ep.ID, 10, "")
	d := page.Items[0]
	if _, err := pgtest.OwnerPool(t).Exec(ctx, `UPDATE webhook_deliveries SET attempts = $3, max_attempts = $3 WHERE tenant_id = $1 AND id = $2`,
		tn.ID, d.ID, webhooks.MaxTotalAttempts); err != nil {
		t.Fatal(err)
	}
	_, err = svc.RetryDelivery(ctx, tn.ID, tn.Actor, d.ID)
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindConflict || ae.Code != "delivery_attempts_limit" {
		t.Fatalf("reenvio além do limite: %v", err)
	}
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "essencial", env.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	got, err := svc.UpdateEndpoint(ctx, tn.ID, tn.Actor, ep.ID, webhooks.Input{URL: ep.URL, EventTypes: ep.EventTypes}, "disabled", 0)
	if err != nil || got.Status != webhooks.Disabled {
		t.Fatalf("desativação após o downgrade: %+v %v", got, err)
	}
}
