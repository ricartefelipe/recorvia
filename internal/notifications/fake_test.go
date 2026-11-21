package notifications

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var errInjected = errors.New("falha injetada")

// fakeStore guarda o estado em memória; cada InTenant trabalha sobre uma
// cópia e só a confirma sem erro (como uma transação).
type fakeStore struct {
	mu    sync.Mutex
	state *fakeState
	// fail injeta erro na operação nomeada.
	fail map[string]error
	// lockBusy simula bloqueio do cliente detido por outra transação.
	lockBusy map[uuid.UUID]bool
	// bumpOnClaim simula outra instância assumindo a notificação entre a
	// reivindicação e o registro do resultado.
	bumpOnSend bool
	tenants    []uuid.UUID
	// visited registra os tenants de cada transação aberta.
	visited []uuid.UUID
}

type fakeState struct {
	org         Organization
	plan        platformbilling.Plan
	templates   map[TemplateKey]Template
	notifs      map[uuid.UUID]Notification
	events      map[uuid.UUID][]Event
	recipients  map[uuid.UUID][]string
	invoices    map[uuid.UUID]InvoiceInfo
	payments    map[uuid.UUID]PaymentInfo
	invitations map[uuid.UUID]InvitationInfo
	suspended   map[uuid.UUID]bool
	// suspensions guarda a situação das suspensões (true = ativa); ausente
	// quando a suspensão não existe.
	suspensions map[uuid.UUID]bool
	rules       map[uuid.UUID]ReminderRules
	// windows é a janela da política de cada suspensão.
	windows map[uuid.UUID]Window
	inbox   map[string]bool
	audits  []audit.Entry
}

func (s *fakeState) clone() *fakeState {
	c := *s
	c.templates = maps.Clone(s.templates)
	c.notifs = maps.Clone(s.notifs)
	c.events = map[uuid.UUID][]Event{}
	for k, v := range s.events {
		c.events[k] = append([]Event(nil), v...)
	}
	c.recipients = maps.Clone(s.recipients)
	c.invoices = maps.Clone(s.invoices)
	c.payments = maps.Clone(s.payments)
	c.invitations = maps.Clone(s.invitations)
	c.suspended = maps.Clone(s.suspended)
	c.suspensions = maps.Clone(s.suspensions)
	c.rules = maps.Clone(s.rules)
	c.windows = maps.Clone(s.windows)
	c.inbox = maps.Clone(s.inbox)
	c.audits = append([]audit.Entry(nil), s.audits...)
	return &c
}

func newFakeStore(loc *time.Location) *fakeStore {
	return &fakeStore{fail: map[string]error{}, lockBusy: map[uuid.UUID]bool{}, state: &fakeState{
		org:         Organization{Name: "Acme Serviços", Location: loc},
		plan:        platformbilling.Plan{Code: "essencial", Features: []platformbilling.Feature{platformbilling.FeatureEmailDunning}},
		templates:   map[TemplateKey]Template{},
		notifs:      map[uuid.UUID]Notification{},
		events:      map[uuid.UUID][]Event{},
		recipients:  map[uuid.UUID][]string{},
		invoices:    map[uuid.UUID]InvoiceInfo{},
		payments:    map[uuid.UUID]PaymentInfo{},
		invitations: map[uuid.UUID]InvitationInfo{},
		suspended:   map[uuid.UUID]bool{},
		suspensions: map[uuid.UUID]bool{},
		rules:       map[uuid.UUID]ReminderRules{},
		windows:     map[uuid.UUID]Window{},
		inbox:       map[string]bool{},
	}}
}

type fakeTx struct {
	st    *fakeState
	store *fakeStore
}

func (f *fakeStore) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visited = append(f.visited, tenantID)
	if err := f.fail["InTenant"]; err != nil {
		return err
	}
	// Como o pgx, uma transação não começa com o contexto encerrado.
	if err := ctx.Err(); err != nil {
		return err
	}
	work := f.state.clone()
	if err := fn(fakeTx{st: work, store: f}); err != nil {
		return err
	}
	f.state = work
	return nil
}

func (f *fakeStore) TenantsWithDueNotifications(_ context.Context, _ time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	if err := f.fail["TenantsWithDueNotifications"]; err != nil {
		return nil, err
	}
	all := append([]uuid.UUID(nil), f.tenants...)
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
	return out, nil
}

func (f *fakeStore) notification(id uuid.UUID) Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.notifs[id]
}

func (f *fakeStore) history(id uuid.UUID) []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.state.events[id]...)
}

func (f *fakeStore) all() []Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Notification, 0, len(f.state.notifs))
	for _, n := range f.state.notifs {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusinessKey < out[j].BusinessKey })
	return out
}

func (f *fakeStore) put(n Notification) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.notifs[n.ID] = n
}

func (t fakeTx) err(op string) error { return t.store.fail[op] }

func (t fakeTx) CustomTemplate(_ context.Context, key TemplateKey) (*Template, error) {
	if err := t.err("CustomTemplate"); err != nil {
		return nil, err
	}
	if c, ok := t.st.templates[key]; ok {
		return &c, nil
	}
	return nil, nil
}

func (t fakeTx) CustomTemplates(context.Context) ([]Template, error) {
	if err := t.err("CustomTemplates"); err != nil {
		return nil, err
	}
	var out []Template
	for _, v := range t.st.templates {
		out = append(out, v)
	}
	return out, nil
}

func (t fakeTx) UpsertTemplate(_ context.Context, tp Template, _ *uuid.UUID, now time.Time) (Template, error) {
	if err := t.err("UpsertTemplate"); err != nil {
		return Template{}, err
	}
	prev := t.st.templates[tp.Key]
	tp.Version, tp.UpdatedAt = prev.Version+1, &now
	t.st.templates[tp.Key] = tp
	return tp, nil
}

func (t fakeTx) InsertNotification(_ context.Context, n *Notification) (bool, error) {
	if err := t.err("InsertNotification"); err != nil {
		return false, err
	}
	for _, x := range t.st.notifs {
		if x.BusinessKey == n.BusinessKey {
			return false, nil
		}
	}
	t.st.events[n.ID] = append(t.st.events[n.ID], n.PopEvents()...)
	t.st.notifs[n.ID] = *n
	return true, nil
}

func (t fakeTx) DueNotifications(_ context.Context, now time.Time, limit int) ([]Notification, error) {
	if err := t.err("DueNotifications"); err != nil {
		return nil, err
	}
	var out []Notification
	for _, n := range t.st.notifs {
		if n.Due(now) {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BusinessKey < out[j].BusinessKey })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (t fakeTx) Notification(ctx context.Context, id uuid.UUID, _ bool) (Notification, error) {
	if err := t.err("Notification"); err != nil {
		return Notification{}, err
	}
	// Como o pgx, uma consulta com o contexto encerrado falha.
	if err := ctx.Err(); err != nil {
		return Notification{}, err
	}
	n, ok := t.st.notifs[id]
	if !ok {
		return Notification{}, apperr.NotFound("notification_not_found", "Notificação não encontrada.")
	}
	if t.store.bumpOnSend && n.Status == Sending {
		n.Attempts++
	}
	return n, nil
}

func (t fakeTx) UpdateNotification(ctx context.Context, n *Notification, expected int) (bool, error) {
	if err := t.err("UpdateNotification"); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	cur := t.st.notifs[n.ID]
	if cur.Attempts != expected {
		return false, nil
	}
	t.st.events[n.ID] = append(t.st.events[n.ID], n.PopEvents()...)
	t.st.notifs[n.ID] = *n
	return true, nil
}

func (t fakeTx) ListNotifications(_ context.Context, f ListFilter) (Page, error) {
	if err := t.err("ListNotifications"); err != nil {
		return Page{}, err
	}
	var p Page
	for _, n := range t.st.notifs {
		if (f.Status == "" || n.Status == f.Status) && (f.InvoiceID == nil || (n.InvoiceID != nil && *n.InvoiceID == *f.InvoiceID)) {
			p.Items = append(p.Items, n)
		}
	}
	return p, nil
}

func (t fakeTx) NewerReminderExists(_ context.Context, invoiceID uuid.UUID, offset int) (bool, error) {
	if err := t.err("NewerReminderExists"); err != nil {
		return false, err
	}
	for _, n := range t.st.notifs {
		if n.InvoiceID != nil && *n.InvoiceID == invoiceID && n.StepOffsetDays != nil && *n.StepOffsetDays > offset {
			return true, nil
		}
	}
	return false, nil
}

func (t fakeTx) TryLockCustomer(_ context.Context, id uuid.UUID) (bool, error) {
	if err := t.err("TryLockCustomer"); err != nil {
		return false, err
	}
	return !t.store.lockBusy[id], nil
}

func (t fakeTx) RemindersSent(_ context.Context, customerID, excludeID uuid.UUID, from, to time.Time) (int, error) {
	if err := t.err("RemindersSent"); err != nil {
		return 0, err
	}
	n := 0
	for _, x := range t.st.notifs {
		if x.CustomerID == nil || *x.CustomerID != customerID || x.PolicyID == nil || x.ID == excludeID {
			continue
		}
		if x.Status == Sending || (x.Status == Sent && !x.SentAt.Before(from) && x.SentAt.Before(to)) {
			n++
		}
	}
	return n, nil
}

func (t fakeTx) ReminderRules(_ context.Context, id uuid.UUID) (ReminderRules, error) {
	if err := t.err("ReminderRules"); err != nil {
		return ReminderRules{}, err
	}
	return t.st.rules[id], nil
}

func (t fakeTx) SuspensionWindow(_ context.Context, id uuid.UUID) (Window, bool, error) {
	if err := t.err("SuspensionWindow"); err != nil {
		return Window{}, false, err
	}
	w, ok := t.st.windows[id]
	return w, ok, nil
}

func (t fakeTx) NotificationEvents(_ context.Context, id uuid.UUID) ([]Event, error) {
	if err := t.err("NotificationEvents"); err != nil {
		return nil, err
	}
	return append([]Event(nil), t.st.events[id]...), nil
}

func (t fakeTx) Organization(context.Context) (Organization, error) {
	if err := t.err("Organization"); err != nil {
		return Organization{}, err
	}
	return t.st.org, nil
}

func (t fakeTx) CustomerRecipients(_ context.Context, id uuid.UUID) ([]string, error) {
	if err := t.err("CustomerRecipients"); err != nil {
		return nil, err
	}
	return t.st.recipients[id], nil
}

func (t fakeTx) Invoice(_ context.Context, id uuid.UUID) (InvoiceInfo, error) {
	if err := t.err("Invoice"); err != nil {
		return InvoiceInfo{}, err
	}
	inv, ok := t.st.invoices[id]
	if !ok {
		return InvoiceInfo{}, apperr.NotFound("invoice_not_found", "Fatura não encontrada.")
	}
	return inv, nil
}

func (t fakeTx) Payment(_ context.Context, id uuid.UUID) (PaymentInfo, error) {
	if err := t.err("Payment"); err != nil {
		return PaymentInfo{}, err
	}
	return t.st.payments[id], nil
}

func (t fakeTx) Invitation(_ context.Context, id uuid.UUID, _ time.Time) (InvitationInfo, error) {
	if err := t.err("Invitation"); err != nil {
		return InvitationInfo{}, err
	}
	return t.st.invitations[id], nil
}

func (t fakeTx) SubscriptionSuspended(_ context.Context, id uuid.UUID) (bool, error) {
	if err := t.err("SubscriptionSuspended"); err != nil {
		return false, err
	}
	return t.st.suspended[id], nil
}

func (t fakeTx) SuspensionActive(_ context.Context, id uuid.UUID) (bool, bool, error) {
	if err := t.err("SuspensionActive"); err != nil {
		return false, false, err
	}
	active, ok := t.st.suspensions[id]
	return active, ok, nil
}

func (t fakeTx) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	if err := t.err("PlatformPlan"); err != nil {
		return platformbilling.Plan{}, err
	}
	return t.st.plan, nil
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
	if err := t.err("Audit"); err != nil {
		return err
	}
	t.st.audits = append(t.st.audits, e)
	return nil
}

// fakeSender registra os e-mails e devolve os erros configurados em ordem.
type fakeSender struct {
	mu   sync.Mutex
	sent []Email
	errs []error
	// refused é devolvido como recusa parcial de destinatários.
	refused []string
	// afterSend é chamado depois de cada envio aceito (por exemplo, para
	// cancelar o job logo após o aceite).
	afterSend func()
}

func (s *fakeSender) Send(_ context.Context, e Email) (Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return Receipt{}, err
		}
	}
	s.sent = append(s.sent, e)
	if s.afterSend != nil {
		s.afterSend()
	}
	return Receipt{MessageID: "<" + e.ID.String() + "@recorvia.test>", Refused: s.refused}, nil
}

type permanentErr struct{}

func (permanentErr) Error() string { return "550 caixa inexistente" }

type fakeOpener struct{ err error }

func (o fakeOpener) Decrypt(ct []byte, keyID string, aad []byte) ([]byte, error) {
	if o.err != nil {
		return nil, o.err
	}
	if keyID != "k1" || string(aad) == "" {
		return nil, errors.New("chave ou contexto inválidos")
	}
	return ct, nil
}
