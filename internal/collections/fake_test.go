package collections

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
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

var errInjected = errors.New("falha injetada")

type fakeState struct {
	org           notifications.Organization
	plan          platformbilling.Plan
	policies      map[uuid.UUID]Policy
	invoices      map[uuid.UUID]notifications.InvoiceInfo
	recipients    map[uuid.UUID][]string
	notifs        map[string]notifications.Notification
	subs          map[uuid.UUID]subscriptions.Subscription
	history       map[uuid.UUID][]subscriptions.HistoryEvent
	suspensions   map[uuid.UUID]Suspension
	delinquencies map[uuid.UUID]time.Time
	resolved      map[uuid.UUID]bool
	inbox         map[string]bool
	events        []outbox.Event
	audits        []audit.Entry
	locked        []string
}

func (s *fakeState) clone() *fakeState {
	c := *s
	c.policies, c.invoices, c.recipients = maps.Clone(s.policies), maps.Clone(s.invoices), maps.Clone(s.recipients)
	c.notifs, c.subs, c.suspensions = maps.Clone(s.notifs), maps.Clone(s.subs), maps.Clone(s.suspensions)
	c.history = map[uuid.UUID][]subscriptions.HistoryEvent{}
	for k, v := range s.history {
		c.history[k] = append([]subscriptions.HistoryEvent(nil), v...)
	}
	c.delinquencies, c.resolved, c.inbox = maps.Clone(s.delinquencies), maps.Clone(s.resolved), maps.Clone(s.inbox)
	c.events = append([]outbox.Event(nil), s.events...)
	c.audits = append([]audit.Entry(nil), s.audits...)
	c.locked = append([]string(nil), s.locked...)
	return &c
}

type fakeStore struct {
	mu      sync.Mutex
	state   *fakeState
	fail    map[string]error
	tenants []uuid.UUID
	// visited registra os tenants de cada transação aberta.
	visited []uuid.UUID
}

func newFakeStore(loc *time.Location) *fakeStore {
	return &fakeStore{fail: map[string]error{}, state: &fakeState{
		org:      notifications.Organization{Name: "Acme Serviços", Location: loc},
		plan:     platformbilling.Plan{Features: []platformbilling.Feature{platformbilling.FeatureEmailDunning}},
		policies: map[uuid.UUID]Policy{}, invoices: map[uuid.UUID]notifications.InvoiceInfo{}, recipients: map[uuid.UUID][]string{},
		notifs: map[string]notifications.Notification{}, subs: map[uuid.UUID]subscriptions.Subscription{},
		history: map[uuid.UUID][]subscriptions.HistoryEvent{}, suspensions: map[uuid.UUID]Suspension{},
		delinquencies: map[uuid.UUID]time.Time{}, resolved: map[uuid.UUID]bool{}, inbox: map[string]bool{},
	}}
}

func (f *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visited = append(f.visited, tenantID)
	work := f.state.clone()
	if err := fn(fakeTx{st: work, f: f}); err != nil {
		return err
	}
	f.state = work
	return nil
}

func (f *fakeStore) TenantsForCollections(_ context.Context, _ time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	if err := f.fail["TenantsForCollections"]; err != nil {
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

type fakeTx struct {
	st *fakeState
	f  *fakeStore
}

func (t fakeTx) err(op string) error { return t.f.fail[op] }

func (t fakeTx) CustomTemplate(context.Context, notifications.TemplateKey) (*notifications.Template, error) {
	return nil, t.err("CustomTemplate")
}

func (t fakeTx) NotificationExists(_ context.Context, key string) (bool, error) {
	_, ok := t.st.notifs[key]
	return ok, t.err("NotificationExists")
}

func (t fakeTx) InsertNotification(_ context.Context, n *notifications.Notification) (bool, error) {
	if err := t.err("InsertNotification"); err != nil {
		return false, err
	}
	if _, ok := t.st.notifs[n.BusinessKey]; ok {
		return false, nil
	}
	n.PopEvents()
	t.st.notifs[n.BusinessKey] = *n
	return true, nil
}

func (t fakeTx) CancelReminders(_ context.Context, invoiceID uuid.UUID, reason string, _ time.Time) (int, error) {
	if err := t.err("CancelReminders"); err != nil {
		return 0, err
	}
	n := 0
	for k, x := range t.st.notifs {
		if x.InvoiceID != nil && *x.InvoiceID == invoiceID && x.PolicyID != nil && x.Status == notifications.Scheduled {
			x.Status, x.StatusReason = notifications.Canceled, reason
			t.st.notifs[k] = x
			n++
		}
	}
	return n, nil
}

func (t fakeTx) Organization(context.Context) (notifications.Organization, error) {
	return t.st.org, t.err("Organization")
}

func (t fakeTx) CustomerRecipients(_ context.Context, id uuid.UUID) ([]string, error) {
	return t.st.recipients[id], t.err("CustomerRecipients")
}

func (t fakeTx) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	return t.st.plan, t.err("PlatformPlan")
}

func (t fakeTx) LockPolicies(context.Context) error {
	t.st.locked = append(t.st.locked, "policies")
	return t.err("LockPolicies")
}

func (t fakeTx) Policies(context.Context) ([]Policy, error) {
	var out []Policy
	for _, p := range t.st.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, t.err("Policies")
}

func (t fakeTx) Policy(_ context.Context, id uuid.UUID, _ bool) (Policy, error) {
	if err := t.err("Policy"); err != nil {
		return Policy{}, err
	}
	p, ok := t.st.policies[id]
	if !ok {
		return Policy{}, apperr.NotFound("collection_policy_not_found", "Política de cobrança não encontrada.")
	}
	return p, nil
}

func (t fakeTx) DefaultPolicy(context.Context) (*Policy, error) {
	if err := t.err("DefaultPolicy"); err != nil {
		return nil, err
	}
	for _, p := range t.st.policies {
		if p.IsDefault {
			return &p, nil
		}
	}
	return nil, nil
}

func (t fakeTx) InsertPolicy(_ context.Context, p Policy, _ *uuid.UUID) error {
	if err := t.err("InsertPolicy"); err != nil {
		return err
	}
	t.st.policies[p.ID] = p
	return nil
}

func (t fakeTx) UpdatePolicy(_ context.Context, p Policy, expected int) error {
	if err := t.err("UpdatePolicy"); err != nil {
		return err
	}
	if t.st.policies[p.ID].Version != expected {
		return apperr.PreconditionFailed("version_mismatch", "versão")
	}
	p.Version++
	t.st.policies[p.ID] = p
	return nil
}

func (t fakeTx) ClearDefaultPolicy(_ context.Context, keep uuid.UUID) error {
	if err := t.err("ClearDefaultPolicy"); err != nil {
		return err
	}
	for id, p := range t.st.policies {
		if id != keep && p.IsDefault {
			p.IsDefault = false
			p.Version++
			t.st.policies[id] = p
		}
	}
	return nil
}

func (t fakeTx) sortedInvoices(filter func(notifications.InvoiceInfo) bool) []notifications.InvoiceInfo {
	var out []notifications.InvoiceInfo
	for _, inv := range t.st.invoices {
		if filter(inv) {
			out = append(out, inv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out
}

func (t fakeTx) ReminderCandidates(_ context.Context, horizon time.Time, after *InvoiceCursor, limit int) ([]notifications.InvoiceInfo, error) {
	all := t.sortedInvoices(func(i notifications.InvoiceInfo) bool { return i.Open() && !i.DueAt.After(horizon) })
	sort.SliceStable(all, func(i, j int) bool {
		if !all[i].DueAt.Equal(*all[j].DueAt) {
			return all[i].DueAt.Before(*all[j].DueAt)
		}
		return all[i].ID.String() < all[j].ID.String()
	})
	var out []notifications.InvoiceInfo
	for _, inv := range all {
		if after != nil && (inv.DueAt.Before(after.DueAt) || (inv.DueAt.Equal(after.DueAt) && inv.ID.String() <= after.ID.String())) {
			continue
		}
		out = append(out, inv)
		if len(out) == limit {
			break
		}
	}
	return out, t.err("ReminderCandidates")
}

func (t fakeTx) Invoice(_ context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	if err := t.err("Invoice"); err != nil {
		return notifications.InvoiceInfo{}, err
	}
	inv, ok := t.st.invoices[id]
	if !ok {
		return inv, apperr.NotFound("invoice_not_found", "Fatura não encontrada.")
	}
	return inv, nil
}

func (t fakeTx) LockInvoice(ctx context.Context, id uuid.UUID) (notifications.InvoiceInfo, error) {
	if err := t.err("LockInvoice"); err != nil {
		return notifications.InvoiceInfo{}, err
	}
	t.st.locked = append(t.st.locked, "invoice:"+id.String())
	return t.Invoice(ctx, id)
}

func (t fakeTx) LockOverdueInvoices(_ context.Context, subID uuid.UUID, now time.Time) ([]notifications.InvoiceInfo, error) {
	if err := t.err("LockOverdueInvoices"); err != nil {
		return nil, err
	}
	out := t.sortedInvoices(func(i notifications.InvoiceInfo) bool {
		return i.SubscriptionID != nil && *i.SubscriptionID == subID && i.Overdue(now)
	})
	for _, inv := range out {
		t.st.locked = append(t.st.locked, "invoice:"+inv.ID.String())
	}
	return out, nil
}

func (t fakeTx) SubscriptionsToReview(_ context.Context, now time.Time, suspendBefore *time.Time, _ int) ([]uuid.UUID, error) {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, inv := range t.sortedInvoices(func(i notifications.InvoiceInfo) bool { return i.SubscriptionID != nil && i.Overdue(now) }) {
		sub := t.st.subs[*inv.SubscriptionID]
		if sub.Status != subscriptions.Active && sub.Status != subscriptions.PastDue {
			continue
		}
		_, marked := t.st.delinquencies[inv.ID]
		need := sub.Status == subscriptions.Active || !marked || (suspendBefore != nil && !inv.DueAt.After(*suspendBefore))
		if need && !seen[sub.ID] {
			seen[sub.ID] = true
			out = append(out, sub.ID)
		}
	}
	// Assinaturas em past_due sem fatura vencida em aberto.
	for _, sub := range t.st.subs {
		if sub.Status != subscriptions.PastDue || seen[sub.ID] {
			continue
		}
		overdue := t.sortedInvoices(func(i notifications.InvoiceInfo) bool {
			return i.SubscriptionID != nil && *i.SubscriptionID == sub.ID && i.Overdue(now)
		})
		if len(overdue) == 0 {
			seen[sub.ID] = true
			out = append(out, sub.ID)
		}
	}
	return out, t.err("SubscriptionsToReview")
}

func (t fakeTx) UnmarkedOverdueInvoices(_ context.Context, now time.Time, _ int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	reviewed := func(i notifications.InvoiceInfo) bool {
		if i.SubscriptionID == nil {
			return false
		}
		st := t.st.subs[*i.SubscriptionID].Status
		return st == subscriptions.Active || st == subscriptions.PastDue
	}
	for _, inv := range t.sortedInvoices(func(i notifications.InvoiceInfo) bool { return !reviewed(i) && i.Overdue(now) }) {
		if _, ok := t.st.delinquencies[inv.ID]; !ok {
			out = append(out, inv.ID)
		}
	}
	return out, t.err("UnmarkedOverdueInvoices")
}

func (t fakeTx) SuspendedByPolicy(context.Context, int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, s := range t.st.suspensions {
		if s.Status == SuspensionActive {
			out = append(out, s.SubscriptionID)
		}
	}
	return out, t.err("SuspendedByPolicy")
}

func (t fakeTx) MarkDelinquent(_ context.Context, inv notifications.InvoiceInfo, now time.Time) (bool, error) {
	if err := t.err("MarkDelinquent"); err != nil {
		return false, err
	}
	if _, ok := t.st.delinquencies[inv.ID]; ok {
		return false, nil
	}
	t.st.delinquencies[inv.ID] = now
	return true, nil
}

func (t fakeTx) ResolveDelinquencies(context.Context, time.Time) (int, error) {
	n := 0
	for id := range t.st.delinquencies {
		if inv := t.st.invoices[id]; !inv.Open() && !t.st.resolved[id] {
			t.st.resolved[id] = true
			n++
		}
	}
	return n, t.err("ResolveDelinquencies")
}

func (t fakeTx) Subscription(_ context.Context, id uuid.UUID, _ bool) (subscriptions.Subscription, error) {
	if err := t.err("Subscription"); err != nil {
		return subscriptions.Subscription{}, err
	}
	s, ok := t.st.subs[id]
	if !ok {
		return s, apperr.NotFound("subscription_not_found", "Assinatura não encontrada.")
	}
	t.st.locked = append(t.st.locked, "subscription:"+id.String())
	return s, nil
}

func (t fakeTx) UpdateSubscription(_ context.Context, s subscriptions.Subscription) error {
	if err := t.err("UpdateSubscription"); err != nil {
		return err
	}
	s.Version++
	t.st.subs[s.ID] = s
	return nil
}

func (t fakeTx) InsertSubscriptionHistory(_ context.Context, id uuid.UUID, _ audit.Actor, ev []subscriptions.HistoryEvent) error {
	t.st.history[id] = append(t.st.history[id], ev...)
	return t.err("InsertSubscriptionHistory")
}

func (t fakeTx) ActiveSuspension(_ context.Context, subID uuid.UUID) (*Suspension, error) {
	if err := t.err("ActiveSuspension"); err != nil {
		return nil, err
	}
	for _, s := range t.st.suspensions {
		if s.SubscriptionID == subID && s.Status == SuspensionActive {
			return &s, nil
		}
	}
	return nil, nil
}

func (t fakeTx) ReleasedSuspensionFor(_ context.Context, subID, invoiceID uuid.UUID) (bool, error) {
	if err := t.err("ReleasedSuspensionFor"); err != nil {
		return false, err
	}
	for _, s := range t.st.suspensions {
		if s.SubscriptionID == subID && s.InvoiceID == invoiceID && s.Status == SuspensionReleased && s.EndReason == EndManualReactivation {
			return true, nil
		}
	}
	return false, nil
}

func (t fakeTx) InsertSuspension(_ context.Context, s Suspension) error {
	if err := t.err("InsertSuspension"); err != nil {
		return err
	}
	t.st.suspensions[s.ID] = s
	return nil
}

func (t fakeTx) EndSuspension(_ context.Context, s Suspension) error {
	if err := t.err("EndSuspension"); err != nil {
		return err
	}
	t.st.suspensions[s.ID] = s
	return nil
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

func (t fakeTx) Emit(_ context.Context, e outbox.Event) error {
	t.st.events = append(t.st.events, e)
	return t.err("Emit")
}
