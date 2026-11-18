package webhooks

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var errInjected = errors.New("falha injetada")

type storedSecret struct {
	endpoint uuid.UUID
	secret   Secret
	// unreadable simula um segredo que não decifra.
	unreadable bool
}

type fakeState struct {
	plan       platformbilling.Plan
	endpoints  map[uuid.UUID]Endpoint
	secrets    map[uuid.UUID]storedSecret
	events     map[uuid.UUID]Event
	deliveries map[uuid.UUID]Delivery
	attempts   []Attempt
	inbox      map[string]bool
	audits     []audit.Entry
}

func (s *fakeState) clone() *fakeState {
	c := *s
	c.endpoints, c.secrets, c.events, c.deliveries = maps.Clone(s.endpoints), maps.Clone(s.secrets), maps.Clone(s.events), maps.Clone(s.deliveries)
	c.attempts = append([]Attempt(nil), s.attempts...)
	c.inbox = maps.Clone(s.inbox)
	c.audits = append([]audit.Entry(nil), s.audits...)
	return &c
}

type fakeStore struct {
	mu      sync.Mutex
	state   *fakeState
	fail    map[string]error
	tenants []uuid.UUID
	// bump simula outra instância assumindo a entrega durante a requisição.
	bump bool
	// locks conta os bloqueios de criação de endpoints.
	locks int
}

func newFakeStore() *fakeStore {
	return &fakeStore{fail: map[string]error{}, state: &fakeState{
		plan:      platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureOutboundWebhooks}},
		endpoints: map[uuid.UUID]Endpoint{}, secrets: map[uuid.UUID]storedSecret{}, events: map[uuid.UUID]Event{},
		deliveries: map[uuid.UUID]Delivery{}, inbox: map[string]bool{},
	}}
}

func (f *fakeStore) InTenant(ctx context.Context, _ uuid.UUID, fn func(Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Como o pgx, uma transação não começa com o contexto encerrado.
	if err := ctx.Err(); err != nil {
		return err
	}
	work := f.state.clone()
	if err := fn(fakeTx{st: work, f: f}); err != nil {
		return err
	}
	f.state = work
	return nil
}

func (f *fakeStore) TenantsWithDueDeliveries(_ context.Context, _ time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	return pageTenants(f.tenants, after, limit), f.fail["TenantsWithDueDeliveries"]
}

// pageTenants simula a descoberta paginada por ID de tenant.
func pageTenants(ids []uuid.UUID, after *uuid.UUID, limit int) []uuid.UUID {
	all := append([]uuid.UUID(nil), ids...)
	sort.Slice(all, func(i, j int) bool { return bytes.Compare(all[i][:], all[j][:]) < 0 })
	var out []uuid.UUID
	for _, id := range all {
		if after != nil && bytes.Compare(id[:], after[:]) <= 0 {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out
}

func (f *fakeStore) deliveries() []Delivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Delivery
	for _, d := range f.state.deliveries {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out
}

type fakeTx struct {
	st *fakeState
	f  *fakeStore
}

func (t fakeTx) err(op string) error { return t.f.fail[op] }

func (t fakeTx) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	return t.st.plan, t.err("PlatformPlan")
}

func (t fakeTx) secretInfos(id uuid.UUID, now time.Time) []SecretInfo {
	out := []SecretInfo{}
	for _, s := range t.st.secrets {
		if s.endpoint == id && s.secret.ValidAt(now) {
			out = append(out, s.secret.SecretInfo)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return currentFirst(out[i], out[j]) })
	return out
}

// currentFirst ordena o segredo corrente primeiro e depois do mais recente ao
// mais antigo (mesma ordem das consultas).
func currentFirst(a, b SecretInfo) bool {
	if (a.ExpiresAt == nil) != (b.ExpiresAt == nil) {
		return a.ExpiresAt == nil
	}
	return a.CreatedAt.After(b.CreatedAt)
}

func (t fakeTx) Endpoints(_ context.Context, now time.Time) ([]Endpoint, error) {
	var out []Endpoint
	for _, e := range t.st.endpoints {
		if e.DeletedAt == nil {
			e.Secrets = t.secretInfos(e.ID, now)
			out = append(out, e)
		}
	}
	return out, t.err("Endpoints")
}

func (t fakeTx) Endpoint(_ context.Context, id uuid.UUID, _ bool, now time.Time) (Endpoint, error) {
	if err := t.err("Endpoint"); err != nil {
		return Endpoint{}, err
	}
	e, ok := t.st.endpoints[id]
	if !ok {
		return Endpoint{}, apperr.NotFound("webhook_endpoint_not_found", "Endpoint de webhook não encontrado.")
	}
	e.Secrets = t.secretInfos(id, now)
	return e, nil
}

func (t fakeTx) LockEndpoints(context.Context) error {
	t.f.locks++
	return t.err("LockEndpoints")
}

func (t fakeTx) CountEndpoints(context.Context) (int, error) {
	n := 0
	for _, e := range t.st.endpoints {
		if e.DeletedAt == nil {
			n++
		}
	}
	return n, t.err("CountEndpoints")
}

func (t fakeTx) InsertEndpoint(_ context.Context, e Endpoint, _ *uuid.UUID) error {
	if err := t.err("InsertEndpoint"); err != nil {
		return err
	}
	t.st.endpoints[e.ID] = e
	return nil
}

func (t fakeTx) UpdateEndpoint(_ context.Context, e Endpoint, expected int) error {
	if err := t.err("UpdateEndpoint"); err != nil {
		return err
	}
	if t.st.endpoints[e.ID].Version != expected {
		return apperr.PreconditionFailed("version_mismatch", "versão")
	}
	e.Version = expected + 1
	t.st.endpoints[e.ID] = e
	return nil
}

func (t fakeTx) InsertSecret(_ context.Context, endpointID uuid.UUID, s Secret) error {
	if err := t.err("InsertSecret"); err != nil {
		return err
	}
	for _, x := range t.st.secrets {
		if x.endpoint == endpointID && x.secret.ExpiresAt == nil {
			return errors.New("dois segredos correntes")
		}
	}
	t.st.secrets[s.ID] = storedSecret{endpoint: endpointID, secret: s}
	return nil
}

func (t fakeTx) ExpireSecrets(_ context.Context, endpointID uuid.UUID, at time.Time) error {
	for id, x := range t.st.secrets {
		if x.endpoint == endpointID && (x.secret.ExpiresAt == nil || x.secret.ExpiresAt.After(at)) {
			x.secret.ExpiresAt = &at
			t.st.secrets[id] = x
		}
	}
	return t.err("ExpireSecrets")
}

func (t fakeTx) RetainSecrets(_ context.Context, endpointID uuid.UUID, keep int, now time.Time) error {
	var valid []storedSecret
	for _, x := range t.st.secrets {
		if x.endpoint == endpointID && x.secret.ValidAt(now) {
			valid = append(valid, x)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool { return currentFirst(valid[i].secret.SecretInfo, valid[j].secret.SecretInfo) })
	for _, x := range valid[min(keep, len(valid)):] {
		x.secret.ExpiresAt = &now
		t.st.secrets[x.secret.ID] = x
	}
	return t.err("RetainSecrets")
}

func (t fakeTx) Secrets(_ context.Context, endpointID uuid.UUID, now time.Time) ([]Secret, error) {
	var out []Secret
	for _, x := range t.st.secrets {
		if x.endpoint == endpointID && x.secret.ValidAt(now) {
			sec := x.secret
			if x.unreadable {
				sec.Value, sec.Unavailable = "", true
			}
			out = append(out, sec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return currentFirst(out[i].SecretInfo, out[j].SecretInfo) })
	if len(out) > MaxSignatures {
		out = out[:MaxSignatures]
	}
	return out, t.err("Secrets")
}

func (t fakeTx) SubscribedEndpoints(_ context.Context, eventType string) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, e := range t.st.endpoints {
		if e.Deliverable() && e.Subscribes(eventType) {
			out = append(out, e.ID)
		}
	}
	return out, t.err("SubscribedEndpoints")
}

func (t fakeTx) InsertEvent(_ context.Context, e Event) (bool, error) {
	if _, ok := t.st.events[e.ID]; ok {
		return false, t.err("InsertEvent")
	}
	t.st.events[e.ID] = e
	return true, t.err("InsertEvent")
}

func (t fakeTx) Event(_ context.Context, id uuid.UUID) (Event, error) {
	return t.st.events[id], t.err("Event")
}

func (t fakeTx) InsertDelivery(_ context.Context, d Delivery) (bool, error) {
	if err := t.err("InsertDelivery"); err != nil {
		return false, err
	}
	for _, x := range t.st.deliveries {
		if x.EndpointID == d.EndpointID && x.EventID == d.EventID {
			return false, nil
		}
	}
	t.st.deliveries[d.ID] = d
	return true, nil
}

func (t fakeTx) Deliveries(_ context.Context, endpointID uuid.UUID, _ int, _ string) (DeliveryPage, error) {
	var p DeliveryPage
	for _, d := range t.st.deliveries {
		if d.EndpointID == endpointID {
			p.Items = append(p.Items, d)
		}
	}
	return p, t.err("Deliveries")
}

func (t fakeTx) Delivery(ctx context.Context, id uuid.UUID, _ bool) (Delivery, error) {
	if err := t.err("Delivery"); err != nil {
		return Delivery{}, err
	}
	// Como o pgx, uma consulta com o contexto encerrado falha.
	if err := ctx.Err(); err != nil {
		return Delivery{}, err
	}
	d, ok := t.st.deliveries[id]
	if !ok {
		return d, apperr.NotFound("webhook_delivery_not_found", "Entrega não encontrada.")
	}
	if t.f.bump && d.Status == Delivering {
		d.Attempts++
	}
	return d, nil
}

func (t fakeTx) DueDeliveries(_ context.Context, now time.Time, limit int) ([]Delivery, error) {
	var out []Delivery
	for _, d := range t.st.deliveries {
		if d.Due(now) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, t.err("DueDeliveries")
}

func (t fakeTx) UpdateDelivery(ctx context.Context, d Delivery, expected int) (bool, error) {
	if err := t.err("UpdateDelivery"); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if t.st.deliveries[d.ID].Attempts != expected {
		return false, nil
	}
	t.st.deliveries[d.ID] = d
	return true, nil
}

func (t fakeTx) InsertAttempt(ctx context.Context, a Attempt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.st.attempts = append(t.st.attempts, a)
	return t.err("InsertAttempt")
}

func (t fakeTx) MarkInbox(_ context.Context, consumer string, id uuid.UUID) (bool, error) {
	if err := t.err("MarkInbox"); err != nil {
		return false, err
	}
	k := consumer + "/" + id.String()
	if t.st.inbox[k] {
		return false, nil
	}
	t.st.inbox[k] = true
	return true, nil
}

func (t fakeTx) Audit(_ context.Context, e audit.Entry) error {
	t.st.audits = append(t.st.audits, e)
	return t.err("Audit")
}

// fakeTransport registra as requisições e devolve as respostas configuradas.
type fakeTransport struct {
	mu        sync.Mutex
	requests  []Request
	responses []func() (int, error)
	// byHost responde às requisições de um host específico com o contexto
	// da tentativa (por exemplo, um destino que nunca responde).
	byHost map[string]func(ctx context.Context) (int, error)
}

func (f *fakeTransport) Post(ctx context.Context, r Request) (int, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	for host, fn := range f.byHost {
		if strings.Contains(r.URL, "://"+host+"/") {
			f.mu.Unlock()
			return fn(ctx)
		}
	}
	defer f.mu.Unlock()
	if len(f.responses) == 0 {
		return 204, nil
	}
	next := f.responses[0]
	f.responses = f.responses[1:]
	return next()
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// routerStore encaminha cada tenant a um store próprio (isolamento entre
// tenants nas entregas).
type routerStore map[uuid.UUID]*fakeStore

func (r routerStore) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	return r[tenantID].InTenant(ctx, tenantID, fn)
}

func (r routerStore) TenantsWithDueDeliveries(_ context.Context, _ time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	ids := make([]uuid.UUID, 0, len(r))
	for id := range r {
		ids = append(ids, id)
	}
	return pageTenants(ids, after, limit), nil
}

type fakeValidator struct{ err error }

func (v fakeValidator) Validate(context.Context, string) error { return v.err }

var errBlocked = errors.New("destino interno bloqueado")
