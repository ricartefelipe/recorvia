package platformbilling

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var errBoom = errors.New("falha simulada")

// tenantState é o estado de um tenant no armazenamento em memória.
type tenantState struct {
	loc        *time.Location
	sub        Subscription
	hasSub     bool
	usage      Usage
	orders     []AddonOrder
	statements []Statement
	audits     []audit.Entry
	updates    int
	// addonsByStart define o total de adicionais concluídos por início de período.
	addonsByStart map[time.Time]money.Money
	// existing simula demonstrativos já gravados (chave: início do período).
	existing map[time.Time]bool
}

// fakeStore implementa Store e Tx em memória, com injeção de falhas por
// nome de operação.
type fakeStore struct {
	plans   map[string]Plan
	addons  map[string]Addon
	tenants map[uuid.UUID]*tenantState
	due     []uuid.UUID
	fail    map[string]error
	// cancelAfter cancela o contexto após o número de transações informado.
	cancelAfter int
	cancel      context.CancelFunc
	txCount     int
	dueAt       time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		plans: map[string]Plan{}, addons: map[string]Addon{},
		tenants: map[uuid.UUID]*tenantState{}, fail: map[string]error{},
	}
}

func (f *fakeStore) tenant(id uuid.UUID) *tenantState {
	st, ok := f.tenants[id]
	if !ok {
		st = &tenantState{addonsByStart: map[time.Time]money.Money{}, existing: map[time.Time]bool{}}
		f.tenants[id] = st
	}
	return st
}

func (f *fakeStore) err(op string) error { return f.fail[op] }

func (f *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := f.err("InTenant"); err != nil {
		return err
	}
	f.txCount++
	err := fn(&fakeTx{store: f, st: f.tenant(tenantID)})
	if f.cancel != nil && f.txCount == f.cancelAfter {
		f.cancel()
	}
	return err
}

func (f *fakeStore) Plans(context.Context) ([]Plan, error) {
	if err := f.err("Plans"); err != nil {
		return nil, err
	}
	out := make([]Plan, 0, len(f.plans))
	for _, p := range f.plans {
		out = append(out, p)
	}
	return out, nil
}

func (f *fakeStore) Addons(context.Context) ([]Addon, error) {
	if err := f.err("Addons"); err != nil {
		return nil, err
	}
	out := make([]Addon, 0, len(f.addons))
	for _, a := range f.addons {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeStore) TenantsDueStatement(_ context.Context, now time.Time) ([]uuid.UUID, error) {
	f.dueAt = now
	if err := f.err("TenantsDueStatement"); err != nil {
		return nil, err
	}
	return f.due, nil
}

type fakeTx struct {
	store *fakeStore
	st    *tenantState
}

func (t *fakeTx) Subscription(context.Context) (Subscription, error) {
	if err := t.store.err("Subscription"); err != nil {
		return Subscription{}, err
	}
	if !t.st.hasSub {
		return Subscription{}, apperr.NotFound("platform_subscription_not_found", "Assinatura não encontrada.")
	}
	return t.st.sub, nil
}

func (t *fakeTx) UpdateSubscription(_ context.Context, s Subscription) error {
	if err := t.store.err("UpdateSubscription"); err != nil {
		return err
	}
	t.st.updates++
	t.st.sub = s
	return nil
}

func (t *fakeTx) Plan(_ context.Context, code string) (Plan, error) {
	if err := t.store.err("Plan"); err != nil {
		return Plan{}, err
	}
	p, ok := t.store.plans[code]
	if !ok {
		return Plan{}, apperr.NotFound("platform_plan_not_found", "Plano não encontrado.")
	}
	return p, nil
}

func (t *fakeTx) Addon(_ context.Context, code string) (Addon, error) {
	if err := t.store.err("Addon"); err != nil {
		return Addon{}, err
	}
	a, ok := t.store.addons[code]
	if !ok {
		return Addon{}, apperr.NotFound("addon_not_found", "Serviço adicional não encontrado.")
	}
	return a, nil
}

func (t *fakeTx) Usage(context.Context) (Usage, error) {
	if err := t.store.err("Usage"); err != nil {
		return Usage{}, err
	}
	return t.st.usage, nil
}

func (t *fakeTx) InsertAddonOrder(_ context.Context, o AddonOrder) error {
	if err := t.store.err("InsertAddonOrder"); err != nil {
		return err
	}
	t.st.orders = append(t.st.orders, o)
	return nil
}

func (t *fakeTx) CompletedAddonsTotal(_ context.Context, from, _ time.Time) (money.Money, error) {
	if err := t.store.err("CompletedAddonsTotal"); err != nil {
		return money.Money{}, err
	}
	if v, ok := t.st.addonsByStart[from]; ok {
		return v, nil
	}
	return money.Zero(money.BRL), nil
}

func (t *fakeTx) InsertStatement(_ context.Context, s Statement) (bool, error) {
	if err := t.store.err("InsertStatement"); err != nil {
		return false, err
	}
	if t.st.existing[s.PeriodStart] {
		return false, nil
	}
	t.st.existing[s.PeriodStart] = true
	t.st.statements = append(t.st.statements, s)
	return true, nil
}

func (t *fakeTx) TenantLocation(context.Context) (*time.Location, error) {
	if err := t.store.err("TenantLocation"); err != nil {
		return nil, err
	}
	return t.st.loc, nil
}

func (t *fakeTx) Audit(_ context.Context, e audit.Entry) error {
	if err := t.store.err("Audit"); err != nil {
		return err
	}
	t.st.audits = append(t.st.audits, e)
	return nil
}
