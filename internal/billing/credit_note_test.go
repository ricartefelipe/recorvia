package billing

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

type standingRefresh struct {
	subscriptionID uuid.UUID
	actor          audit.Actor
	at             time.Time
}

// creditNoteStore estende o invoiceStore com as operações de notas de
// crédito; a transação desfaz também as notas e as atualizações de situação.
type creditNoteStore struct {
	*invoiceStore
	notes     []CreditNote
	refreshed []standingRefresh
	// busy simula outra emissão concorrente com a mesma chave.
	busy bool
	// failEvent falha somente o evento do tipo informado.
	failEvent string
	failErr   error
}

func (s *creditNoteStore) InCreditNoteTx(ctx context.Context, tenantID uuid.UUID, fn func(CreditNoteTx) error) error {
	notes, refreshed := slices.Clone(s.notes), slices.Clone(s.refreshed)
	err := s.InTenant(ctx, tenantID, func(Tx) error { return fn(s) })
	if err != nil {
		s.notes, s.refreshed = notes, refreshed
	}
	return err
}

func (s *creditNoteStore) InsertCreditNote(_ context.Context, cn CreditNote) (bool, error) {
	if err := s.fail("InsertCreditNote"); err != nil {
		return false, err
	}
	if s.busy {
		return false, nil
	}
	s.notes = append(s.notes, cn)
	return true, nil
}

func (s *creditNoteStore) CreditNoteByRequestKey(_ context.Context, key string) (CreditNote, error) {
	if err := s.fail("CreditNoteByRequestKey"); err != nil {
		return CreditNote{}, err
	}
	for _, n := range s.notes {
		if n.RequestKey == key {
			return n, nil
		}
	}
	return CreditNote{}, apperr.NotFound("credit_note_not_found", "Nota de crédito não encontrada.")
}

func (s *creditNoteStore) CountCreditNotes(_ context.Context, invoiceID uuid.UUID) (int, error) {
	if err := s.fail("CountCreditNotes"); err != nil {
		return 0, err
	}
	n := 0
	for _, cn := range s.notes {
		if cn.InvoiceID == invoiceID {
			n++
		}
	}
	return n, nil
}

func (s *creditNoteStore) RefreshSubscriptionStanding(_ context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error {
	if err := s.fail("RefreshSubscriptionStanding"); err != nil {
		return err
	}
	s.refreshed = append(s.refreshed, standingRefresh{subscriptionID, actor, now})
	return nil
}

func (s *creditNoteStore) Emit(ctx context.Context, e outbox.Event) error {
	if s.failEvent == e.Type {
		return s.failErr
	}
	return s.invoiceStore.Emit(ctx, e)
}

var _ CreditNoteTx = (*creditNoteStore)(nil)

type creditNoteFixture struct {
	store    *creditNoteStore
	clock    *clock.Manual
	svc      *CreditNoteService
	tenantID uuid.UUID
	userID   uuid.UUID
	actor    audit.Actor
}

func newCreditNoteFixture(t *testing.T) *creditNoteFixture {
	t.Helper()
	f := &creditNoteFixture{store: &creditNoteStore{invoiceStore: newInvoiceStore()}, clock: clock.NewManual(invoiceNow.Add(48 * time.Hour)),
		tenantID: uuid.New(), userID: uuid.New()}
	f.actor = audit.User(f.userID, "Ana Souza")
	f.svc = NewCreditNoteService(f.store, f.clock)
	return f
}

// addInvoice grava uma fatura emitida de R$ 100,00 com o pagamento parcial
// informado; subscription vincula a fatura a uma assinatura.
func (f *creditNoteFixture) addInvoice(t *testing.T, paid int64, subscription bool) Invoice {
	t.Helper()
	inv := draftWith(t, 10000)
	inv.TenantID = f.tenantID
	if subscription {
		id := uuid.New()
		inv.SubscriptionID = &id
	}
	if err := inv.Issue("FAT-2026-000123", invoiceNow, DueAt(invoiceNow, 5, saoPaulo)); err != nil {
		t.Fatal(err)
	}
	if paid > 0 {
		if _, _, err := inv.ApplyPayment(brl(paid), invoiceNow.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	inv.PopChanges()
	f.store.invoices[inv.ID] = *inv
	return *inv
}

func (f *creditNoteFixture) issue(t *testing.T, invoiceID uuid.UUID, amount int64, key string) CreditNote {
	t.Helper()
	cn, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, invoiceID, brl(amount), "Desconto comercial concedido", key)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return cn
}

func (f *creditNoteFixture) assertNoEffects(t *testing.T, inv Invoice) {
	t.Helper()
	f.store.assertUntouchedInvoice(t, inv)
	if len(f.store.notes) != 0 || len(f.store.refreshed) != 0 {
		t.Fatalf("nenhuma nota deveria ser gravada: %+v", f.store.notes)
	}
}

func (s *creditNoteStore) assertUntouchedInvoice(t *testing.T, inv Invoice) {
	t.Helper()
	got := s.invoices[inv.ID]
	if got.Status != inv.Status || got.Version != inv.Version || got.AmountCredited != inv.AmountCredited || got.AmountDue != inv.AmountDue {
		t.Fatalf("fatura alterada: %s v%d saldo=%v", got.Status, got.Version, got.AmountDue)
	}
	if len(s.statusWrites) != 0 || len(s.posted) != 0 || len(s.audits) != 0 || len(s.events) != 0 {
		t.Fatal("nenhum efeito deveria ser gravado")
	}
}

func TestCreditNoteReducesAmountDue(t *testing.T) {
	f := newCreditNoteFixture(t)
	inv := f.addInvoice(t, 0, true)
	now := f.clock.Now()

	cn := f.issue(t, inv.ID, 3000, "pedido-1")
	if cn.InvoiceID != inv.ID || cn.TenantID != f.tenantID || cn.Number != "FAT-2026-000123-NC01" || cn.Amount.Amount() != 3000 ||
		cn.Reason != "Desconto comercial concedido" || *cn.CreatedBy != f.userID || cn.RequestKey != "pedido-1" || !cn.CreatedAt.Equal(now) {
		t.Fatalf("nota de crédito: %+v", cn)
	}
	got := f.store.invoices[inv.ID]
	if got.Status != Open || got.AmountCredited.Amount() != 3000 || got.AmountDue.Amount() != 7000 || got.Total.Amount() != 10000 ||
		got.Version != inv.Version+1 || len(got.Items) != 1 || got.Items[0].Amount.Amount() != 10000 {
		t.Fatalf("a fatura emitida não é reescrita; só o saldo diminui: %+v", got)
	}
	assertConservation(t, &got)
	if len(f.store.statusWrites) != 1 {
		t.Fatalf("histórico: %+v", f.store.statusWrites)
	}
	w := f.store.statusWrites[0]
	if w.actor != f.actor || len(w.changes) != 1 || w.changes[0].To != Open || w.changes[0].Reason != "credit_note" ||
		w.changes[0].Details["credit_note_id"] != cn.ID || w.changes[0].Details["credit_note_number"] != cn.Number ||
		w.changes[0].Details["amount"] != int64(3000) {
		t.Fatalf("histórico da nota: %+v", w)
	}
	if len(f.store.posted) != 1 || f.store.posted[0].IdempotencyKey != "credit_note.issued:"+cn.ID.String() {
		t.Fatalf("lançamento da nota: %+v", f.store.posted)
	}
	if net := ledgerNet(t, f.store.posted); net[ledger.RevenueAdjustments] != 3000 || net[ledger.AccountsReceivable] != -3000 {
		t.Fatalf("ledger da nota: %+v", net)
	}
	if len(f.store.events) != 1 || f.store.events[0].Type != outbox.EventInvoiceCredited || f.store.events[0].AggregateID != inv.ID {
		t.Fatalf("evento: %+v", f.store.events)
	}
	p := f.store.events[0].Payload.(map[string]any)
	if p["credit_note_id"] != cn.ID || p["credit_note_number"] != cn.Number || p["amount"] != int64(3000) || p["amount_due"] != int64(7000) ||
		p["status"] != Open || p["number"] != inv.Number {
		t.Fatalf("payload do evento: %+v", p)
	}
	if len(f.store.audits) != 1 || f.store.audits[0].Action != "invoice.credit_note_issued" || f.store.audits[0].Actor != f.actor ||
		f.store.audits[0].Details["amount_due"] != int64(7000) || f.store.audits[0].Details["number"] != cn.Number {
		t.Fatalf("auditoria: %+v", f.store.audits)
	}
	if len(f.store.refreshed) != 0 {
		t.Fatal("fatura ainda em aberto não altera a situação da assinatura")
	}

	second := f.issue(t, inv.ID, 2000, "")
	if second.Number != "FAT-2026-000123-NC02" || second.RequestKey != "" || f.store.invoices[inv.ID].AmountDue.Amount() != 5000 {
		t.Fatalf("segunda nota numerada em sequência: %+v", second)
	}
}

func TestCreditNoteSettlesInvoice(t *testing.T) {
	cases := []struct {
		name         string
		paid         int64
		amount       int64
		subscription bool
		wantRefresh  bool
	}{
		{"saldo integral de fatura de assinatura", 0, 10000, true, true},
		{"saldo restante após pagamento parcial", 4000, 6000, true, true},
		{"fatura avulsa não altera assinatura", 0, 10000, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCreditNoteFixture(t)
			inv := f.addInvoice(t, c.paid, c.subscription)
			cn := f.issue(t, inv.ID, c.amount, "")
			got := f.store.invoices[inv.ID]
			if got.Status != Paid || !got.AmountDue.IsZero() || got.PaidAt == nil || !got.PaidAt.Equal(f.clock.Now()) {
				t.Fatalf("nota que zera o saldo quita a fatura: %s %v", got.Status, got.AmountDue)
			}
			assertConservation(t, &got)
			if ch := f.store.statusWrites[0].changes; len(ch) != 1 || *ch[0].From != inv.Status || ch[0].To != Paid ||
				ch[0].Details["credit_note_id"] != cn.ID {
				t.Fatalf("transição para paga: %+v", ch)
			}
			types := []string{}
			for _, e := range f.store.events {
				types = append(types, e.Type)
			}
			if !equalStrings(types, []string{outbox.EventInvoiceCredited, outbox.EventInvoicePaid}) {
				t.Fatalf("eventos: %v", types)
			}
			paid := f.store.events[1].Payload.(map[string]any)
			if paid["invoice_id"] != inv.ID || paid["amount_due"] != int64(0) || paid["amount_paid"] != c.paid {
				t.Fatalf("evento de quitação: %+v", paid)
			}
			if c.wantRefresh != (len(f.store.refreshed) == 1) {
				t.Fatalf("atualização da situação da assinatura: %+v", f.store.refreshed)
			}
			if c.wantRefresh {
				r := f.store.refreshed[0]
				if r.subscriptionID != *inv.SubscriptionID || r.actor != f.actor || !r.at.Equal(f.clock.Now()) {
					t.Fatalf("situação atualizada para a assinatura da fatura: %+v", r)
				}
			}
		})
	}
}

func TestCreditNoteLimitedToAmountDue(t *testing.T) {
	cases := []struct {
		name   string
		paid   int64
		status Status
		amount int64
		kind   apperr.Kind
		code   string
	}{
		{"acima do saldo", 0, "", 10001, apperr.KindInvalid, "credit_note_exceeds_due"},
		{"acima do saldo após pagamento", 7000, "", 3001, apperr.KindInvalid, "credit_note_exceeds_due"},
		{"valor zero", 0, "", 0, apperr.KindInvalid, "invalid_amount"},
		{"valor negativo", 0, "", -100, apperr.KindInvalid, "invalid_amount"},
		{"fatura paga", 10000, "", 100, apperr.KindConflict, "invoice_not_open"},
		{"fatura cancelada", 0, Void, 100, apperr.KindConflict, "invoice_not_open"},
		{"fatura incobrável", 0, Uncollectible, 100, apperr.KindConflict, "invoice_not_open"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCreditNoteFixture(t)
			inv := f.addInvoice(t, c.paid, true)
			if c.status != "" {
				inv.Status = c.status
				f.store.invoices[inv.ID] = inv
			}
			_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(c.amount), "Desconto comercial", "")
			wantAppErr(t, err, c.kind, c.code)
			f.assertNoEffects(t, inv)
		})
	}
	t.Run("moeda diferente da fatura", func(t *testing.T) {
		f := newCreditNoteFixture(t)
		inv := f.addInvoice(t, 0, true)
		_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, money.New(100, money.Currency{}),
			"Desconto comercial", "")
		wantAppErr(t, err, apperr.KindInvalid, "credit_note_exceeds_due")
		f.assertNoEffects(t, inv)
	})
}

func TestCreditNoteRejectsInvalidReason(t *testing.T) {
	cases := []struct {
		name   string
		reason string
	}{
		{"vazio", ""},
		{"curto", "ab"},
		{"longo", strings.Repeat("a", 501)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCreditNoteFixture(t)
			inv := f.addInvoice(t, 0, true)
			_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(100), c.reason, "")
			wantAppErr(t, err, apperr.KindInvalid, "invalid_credit_note")
			if e, _ := apperr.As(err); len(e.Fields) != 1 || e.Fields[0].Field != "reason" {
				t.Fatalf("campo do erro: %+v", e.Fields)
			}
			if f.store.txCount != 0 {
				t.Fatal("validação ocorre antes de abrir a transação")
			}
			f.assertNoEffects(t, inv)
		})
	}
	t.Run("limites aceitos contam caracteres", func(t *testing.T) {
		f := newCreditNoteFixture(t)
		inv := f.addInvoice(t, 0, true)
		for _, reason := range []string{"açã", strings.Repeat("é", 500)} {
			if _, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(100), reason, ""); err != nil {
				t.Fatalf("motivo de %d caracteres deveria ser aceito: %v", len([]rune(reason)), err)
			}
		}
	})
}

func TestCreditNoteIdempotentByRequestKey(t *testing.T) {
	f := newCreditNoteFixture(t)
	inv := f.addInvoice(t, 0, true)
	first := f.issue(t, inv.ID, 10000, "pedido-quitacao")
	events, posted, audits := len(f.store.events), len(f.store.posted), len(f.store.audits)

	f.clock.Advance(time.Hour)
	again := f.issue(t, inv.ID, 10000, "pedido-quitacao")
	if again != first {
		t.Fatalf("a mesma chave devolve a nota já emitida: %+v", again)
	}
	if len(f.store.notes) != 1 || len(f.store.events) != events || len(f.store.posted) != posted || len(f.store.audits) != audits ||
		len(f.store.refreshed) != 1 {
		t.Fatal("a repetição não pode gerar novos efeitos")
	}
	if got := f.store.invoices[inv.ID]; got.Status != Paid || got.AmountCredited.Amount() != 10000 {
		t.Fatalf("fatura creditada uma única vez: %+v", got)
	}
}

func TestCreditNoteRequestKeyFromAnotherInvoice(t *testing.T) {
	f := newCreditNoteFixture(t)
	first := f.addInvoice(t, 0, true)
	other := f.addInvoice(t, 0, true)
	f.issue(t, first.ID, 1000, "pedido-1")
	writes := len(f.store.statusWrites)

	_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, other.ID, brl(1000), "Desconto comercial", "pedido-1")
	wantAppErr(t, err, apperr.KindConflict, "request_key_reused")
	if got := f.store.invoices[other.ID]; got.Version != other.Version || !got.AmountCredited.IsZero() {
		t.Fatal("a outra fatura não pode ser alterada")
	}
	if len(f.store.notes) != 1 || len(f.store.statusWrites) != writes {
		t.Fatal("nenhuma nota nova")
	}
}

func TestCreditNoteConcurrentRequest(t *testing.T) {
	f := newCreditNoteFixture(t)
	inv := f.addInvoice(t, 0, true)
	f.store.busy = true
	_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(1000), "Desconto comercial", "pedido-1")
	wantAppErr(t, err, apperr.KindConflict, "credit_note_in_progress")
	f.assertNoEffects(t, inv)
}

func TestCreditNoteRollsBackOnFailure(t *testing.T) {
	methods := []string{"Invoice", "CreditNoteByRequestKey", "CountCreditNotes", "InsertCreditNote", "UpdateInvoiceProgress",
		"InsertStatusHistory", "PostLedger", "Audit", "RefreshSubscriptionStanding"}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			f := newCreditNoteFixture(t)
			inv := f.addInvoice(t, 0, true)
			boom := errors.New("falha em " + m)
			f.store.errs[m] = boom
			_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(10000), "Desconto comercial", "pedido-1")
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			f.assertNoEffects(t, inv)
		})
	}
	for _, ev := range []string{outbox.EventInvoiceCredited, outbox.EventInvoicePaid} {
		t.Run("Emit "+ev, func(t *testing.T) {
			f := newCreditNoteFixture(t)
			inv := f.addInvoice(t, 0, true)
			boom := errors.New("falha no evento " + ev)
			f.store.failEvent, f.store.failErr = ev, boom
			_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, inv.ID, brl(10000), "Desconto comercial", "")
			if !errors.Is(err, boom) {
				t.Fatalf("esperava %v, obtive %v", boom, err)
			}
			f.assertNoEffects(t, inv)
		})
	}
	t.Run("fatura inexistente", func(t *testing.T) {
		f := newCreditNoteFixture(t)
		_, err := f.svc.Issue(context.Background(), f.tenantID, f.actor, &f.userID, uuid.New(), brl(100), "Desconto comercial", "")
		wantAppErr(t, err, apperr.KindNotFound, "invoice_not_found")
		if len(f.store.notes) != 0 {
			t.Fatal("nenhuma nota")
		}
	})
}
