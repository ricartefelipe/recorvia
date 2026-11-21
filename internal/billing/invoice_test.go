package billing

import (
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var saoPaulo = func() *time.Location {
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		panic(err)
	}
	return loc
}()

var allInvoiceStatuses = []Status{Draft, Open, PartiallyPaid, Paid, Void, Uncollectible}

func brl(v int64) money.Money { return money.New(v, money.BRL) }

func local(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, saoPaulo)
}

var invoiceNow = local(2026, time.November, 16, 14, 30)

func newDraft() *Invoice {
	return NewDraft(uuid.New(), uuid.New(), uuid.New(), nil, KindManual, money.BRL, nil, customers.Snapshot{Name: "Padaria Central"}, invoiceNow)
}

func item(amount int64) Item {
	return Item{ID: uuid.New(), Kind: ItemSubscriptionFee, Description: "Mensalidade", Quantity: big.NewRat(1, 1),
		UnitAmount: big.NewRat(amount, 1), Amount: brl(amount)}
}

func draftWith(t testing.TB, amounts ...int64) *Invoice {
	t.Helper()
	inv := newDraft()
	for _, a := range amounts {
		if err := inv.AddItem(item(a)); err != nil {
			t.Fatalf("AddItem(%d): %v", a, err)
		}
	}
	return inv
}

func issued(t testing.TB, amounts ...int64) *Invoice {
	t.Helper()
	inv := draftWith(t, amounts...)
	if err := inv.Issue("FAT-2026-000001", invoiceNow, DueAt(invoiceNow, 5, saoPaulo)); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	inv.PopChanges()
	return inv
}

func wantAppErr(t *testing.T, err error, kind apperr.Kind, code string) {
	t.Helper()
	if !apperr.IsKind(err, kind) {
		t.Fatalf("esperava erro %v, obtive %v", kind, err)
	}
	if e, _ := apperr.As(err); e.Code != code {
		t.Fatalf("esperava código %q, obtive %q", code, e.Code)
	}
}

// assertConservation verifica que pago + saldo + creditado + baixado +
// crédito aplicado = total, sem componentes negativos.
func assertConservation(t testing.TB, inv *Invoice) {
	t.Helper()
	parts := []money.Money{inv.AmountPaid, inv.AmountDue, inv.AmountCredited, inv.AmountWrittenOff, inv.CreditApplied}
	var sum int64
	for _, p := range parts {
		if p.IsNegative() {
			t.Fatalf("componente negativo: pago=%v saldo=%v creditado=%v baixado=%v crédito=%v",
				inv.AmountPaid, inv.AmountDue, inv.AmountCredited, inv.AmountWrittenOff, inv.CreditApplied)
		}
		sum += p.Amount()
	}
	if inv.Status == Void {
		// Cancelada: o crédito aplicado volta ao cliente e não há saldo devedor.
		if !inv.AmountDue.IsZero() || !inv.AmountPaid.IsZero() || !inv.AmountCredited.IsZero() || !inv.AmountWrittenOff.IsZero() {
			t.Fatalf("fatura cancelada com saldo, pagamento, nota ou baixa: %+v", inv)
		}
		sum = inv.Total.Amount()
	}
	if sum != inv.Total.Amount() {
		t.Fatalf("pago %d + saldo %d + creditado %d + baixado %d + crédito %d = %d, total %d",
			inv.AmountPaid.Amount(), inv.AmountDue.Amount(), inv.AmountCredited.Amount(), inv.AmountWrittenOff.Amount(),
			inv.CreditApplied.Amount(), sum, inv.Total.Amount())
	}
	if inv.Total.Amount() != inv.Subtotal.Amount()-inv.DiscountTotal.Amount() || inv.Total.IsNegative() {
		t.Fatalf("total %v inconsistente com subtotal %v e desconto %v", inv.Total, inv.Subtotal, inv.DiscountTotal)
	}
}

func TestInvoiceCanTransitionMatrix(t *testing.T) {
	allowed := map[Status][]Status{
		Draft:         {Open, Paid},
		Open:          {PartiallyPaid, Paid, Void, Uncollectible},
		PartiallyPaid: {Paid, Uncollectible},
		Uncollectible: {Paid},
		Paid:          {},
		Void:          {},
	}
	for _, from := range allInvoiceStatuses {
		for _, to := range allInvoiceStatuses {
			want := false
			for _, a := range allowed[from] {
				want = want || a == to
			}
			if got := CanTransition(from, to); got != want {
				t.Errorf("CanTransition(%s, %s)=%v, esperado %v", from, to, got, want)
			}
		}
	}
}

func TestNewDraftStartsZeroed(t *testing.T) {
	sub := uuid.New()
	period := &calendar.Period{Start: invoiceNow, End: invoiceNow.AddDate(0, 1, 0)}
	inv := NewDraft(uuid.New(), uuid.New(), uuid.New(), &sub, KindSubscriptionCycle, money.BRL, period, customers.Snapshot{}, invoiceNow)
	if inv.Status != Draft || inv.Version != 1 || inv.Kind != KindSubscriptionCycle || *inv.SubscriptionID != sub || inv.BillingPeriod != period {
		t.Fatalf("rascunho: %+v", inv)
	}
	for _, m := range []money.Money{inv.Subtotal, inv.DiscountTotal, inv.Total, inv.CreditApplied, inv.AmountPaid, inv.AmountCredited, inv.AmountWrittenOff, inv.AmountDue} {
		if !m.IsZero() || m.Currency() != money.BRL {
			t.Fatalf("valores devem começar em zero BRL: %v", m)
		}
	}
	if inv.HasChargeableItems() {
		t.Fatal("rascunho vazio não tem itens cobráveis")
	}
}

func TestAddItemRecalculatesTotals(t *testing.T) {
	inv := draftWith(t, 10000, 2500)
	adj := item(-1500)
	adj.Kind = ItemAdjustment
	adj.DiscountAmount = money.Money{} // sem moeda: deve virar zero BRL
	if err := inv.AddItem(adj); err != nil {
		t.Fatal(err)
	}
	if inv.Subtotal.Amount() != 11000 || inv.Total.Amount() != 11000 || inv.AmountDue.Amount() != 11000 {
		t.Fatalf("totais: subtotal=%v total=%v saldo=%v", inv.Subtotal, inv.Total, inv.AmountDue)
	}
	for i, it := range inv.Items {
		if it.Position != i {
			t.Fatalf("posição %d no índice %d", it.Position, i)
		}
	}
	if inv.Items[2].DiscountAmount.Currency() != money.BRL {
		t.Fatal("desconto sem moeda deve ser normalizado")
	}
	if !inv.HasChargeableItems() {
		t.Fatal("há itens com valor")
	}
	zero := draftWith(t, 0)
	if zero.HasChargeableItems() {
		t.Fatal("item de valor zero não é cobrável")
	}
}

func TestAddItemRules(t *testing.T) {
	t.Run("somente em rascunho", func(t *testing.T) {
		inv := issued(t, 1000)
		wantAppErr(t, inv.AddItem(item(500)), apperr.KindConflict, "invoice_not_draft")
		if len(inv.Items) != 1 || inv.Total.Amount() != 1000 {
			t.Fatal("fatura emitida não pode mudar")
		}
	})
	t.Run("moeda diferente", func(t *testing.T) {
		inv := newDraft()
		it := item(100)
		it.Amount = money.New(100, money.Currency{})
		if err := inv.AddItem(it); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes, obtive %v", err)
		}
		if len(inv.Items) != 0 {
			t.Fatal("item de outra moeda não pode ser incluído")
		}
	})
	t.Run("total negativo", func(t *testing.T) {
		inv := newDraft()
		wantAppErr(t, inv.AddItem(item(-1)), apperr.KindInvalid, "negative_invoice")
		if !inv.Total.IsZero() {
			t.Fatalf("total não pode ser alterado: %v", inv.Total)
		}
	})
	t.Run("total acima do limite de negócio", func(t *testing.T) {
		inv := newDraft()
		wantAppErr(t, inv.AddItem(item(money.MaxAmount+1)), apperr.KindInvalid, "invoice_amount_limit")
	})
}

func TestAddItemRejectedLeavesInvoiceUnchanged(t *testing.T) {
	inv := draftWith(t, 1000)
	if err := inv.AddItem(item(-5000)); err == nil {
		t.Fatal("total negativo deve ser rejeitado")
	}
	if len(inv.Items) != 1 {
		t.Fatalf("item rejeitado não pode permanecer na fatura: %d itens", len(inv.Items))
	}
	if err := inv.AddItem(item(5000)); err != nil {
		t.Fatal(err)
	}
	if inv.Total.Amount() != 6000 {
		t.Fatalf("total deveria considerar só os itens aceitos: %v", inv.Total)
	}
}

func TestApplyDiscountLargestRemainder(t *testing.T) {
	inv := draftWith(t, 1000, 2000, 3000)
	applied, err := inv.ApplyDiscount(brl(100))
	if err != nil || applied.Amount() != 100 {
		t.Fatalf("desconto aplicado: %v %v", applied, err)
	}
	// Proporções exatas 16,67 / 33,33 / 50: o centavo restante vai para o
	// maior resto (primeiro item).
	want := []int64{17, 33, 50}
	for i, w := range want {
		if got := inv.Items[i].DiscountAmount.Amount(); got != w {
			t.Fatalf("desconto do item %d = %d, esperado %d", i, got, w)
		}
	}
	if inv.DiscountTotal.Amount() != 100 || inv.Total.Amount() != 5900 || inv.AmountDue.Amount() != 5900 {
		t.Fatalf("totais: desconto=%v total=%v saldo=%v", inv.DiscountTotal, inv.Total, inv.AmountDue)
	}
	assertConservation(t, inv)

	// Determinismo: a mesma entrada produz a mesma distribuição.
	again := draftWith(t, 1000, 2000, 3000)
	if _, err := again.ApplyDiscount(brl(100)); err != nil {
		t.Fatal(err)
	}
	for i := range inv.Items {
		if inv.Items[i].DiscountAmount != again.Items[i].DiscountAmount {
			t.Fatal("distribuição do desconto deve ser determinística")
		}
	}
}

func TestApplyDiscountTiesGoToFirstItem(t *testing.T) {
	inv := draftWith(t, 100, 100, 100)
	if _, err := inv.ApplyDiscount(brl(1)); err != nil {
		t.Fatal(err)
	}
	if inv.Items[0].DiscountAmount.Amount() != 1 || inv.Items[1].DiscountAmount.Amount() != 0 || inv.Items[2].DiscountAmount.Amount() != 0 {
		t.Fatalf("empate resolvido pelo menor índice: %v %v %v", inv.Items[0].DiscountAmount, inv.Items[1].DiscountAmount, inv.Items[2].DiscountAmount)
	}
}

func TestApplyDiscountLimits(t *testing.T) {
	t.Run("limitado ao subtotal positivo", func(t *testing.T) {
		inv := draftWith(t, 1000, 0)
		applied, err := inv.ApplyDiscount(brl(5000))
		if err != nil || applied.Amount() != 1000 || !inv.Total.IsZero() {
			t.Fatalf("aplicado=%v total=%v err=%v", applied, inv.Total, err)
		}
		if !inv.Items[1].DiscountAmount.IsZero() {
			t.Fatal("item de valor zero não recebe desconto")
		}
		again, err := inv.ApplyDiscount(brl(10))
		if err != nil || !again.IsZero() {
			t.Fatalf("sem saldo positivo não há desconto adicional: %v %v", again, err)
		}
	})
	t.Run("desconto não positivo", func(t *testing.T) {
		inv := draftWith(t, 1000)
		for _, d := range []int64{0, -100} {
			applied, err := inv.ApplyDiscount(brl(d))
			if err != nil || !applied.IsZero() || !inv.DiscountTotal.IsZero() {
				t.Fatalf("desconto %d: %v %v", d, applied, err)
			}
		}
	})
	t.Run("descontos sucessivos respeitam o saldo restante", func(t *testing.T) {
		inv := draftWith(t, 1000, 3000)
		if _, err := inv.ApplyDiscount(brl(3000)); err != nil {
			t.Fatal(err)
		}
		applied, err := inv.ApplyDiscount(brl(3000))
		if err != nil || applied.Amount() != 1000 || !inv.Total.IsZero() {
			t.Fatalf("segundo desconto: %v total=%v err=%v", applied, inv.Total, err)
		}
		for _, it := range inv.Items {
			if it.DiscountAmount.Amount() > it.Amount.Amount() {
				t.Fatalf("desconto maior que o item: %+v", it)
			}
		}
	})
	t.Run("somente em rascunho", func(t *testing.T) {
		inv := issued(t, 1000)
		_, err := inv.ApplyDiscount(brl(10))
		wantAppErr(t, err, apperr.KindConflict, "invoice_not_draft")
	})
}

func TestApplyDiscountWithNegativeItemIsLimitedToNetSubtotal(t *testing.T) {
	inv := draftWith(t, 1000)
	credit := item(-200)
	credit.Kind = ItemProration
	if err := inv.AddItem(credit); err != nil {
		t.Fatal(err)
	}
	applied, err := inv.ApplyDiscount(brl(1000))
	if err != nil {
		t.Fatalf("desconto acima do subtotal líquido deve ser limitado, não rejeitado: %v", err)
	}
	if applied.Amount() != 800 || !inv.Total.IsZero() {
		t.Fatalf("aplicado=%v total=%v", applied, inv.Total)
	}
}

func TestApplyCredit(t *testing.T) {
	t.Run("limitado ao saldo", func(t *testing.T) {
		inv := draftWith(t, 10000)
		used, err := inv.ApplyCredit(brl(15000))
		if err != nil || used.Amount() != 10000 || !inv.AmountDue.IsZero() || inv.CreditApplied.Amount() != 10000 {
			t.Fatalf("usado=%v saldo=%v err=%v", used, inv.AmountDue, err)
		}
		assertConservation(t, inv)
	})
	t.Run("parcial", func(t *testing.T) {
		inv := draftWith(t, 10000)
		used, err := inv.ApplyCredit(brl(3000))
		if err != nil || used.Amount() != 3000 || inv.AmountDue.Amount() != 7000 {
			t.Fatalf("usado=%v saldo=%v err=%v", used, inv.AmountDue, err)
		}
		assertConservation(t, inv)
	})
	t.Run("sem crédito disponível", func(t *testing.T) {
		inv := draftWith(t, 10000)
		used, err := inv.ApplyCredit(brl(0))
		if err != nil || !used.IsZero() || inv.AmountDue.Amount() != 10000 {
			t.Fatalf("usado=%v err=%v", used, err)
		}
	})
	t.Run("moeda diferente", func(t *testing.T) {
		inv := draftWith(t, 10000)
		if _, err := inv.ApplyCredit(money.New(100, money.Currency{})); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
	t.Run("somente em rascunho", func(t *testing.T) {
		inv := issued(t, 1000)
		_, err := inv.ApplyCredit(brl(10))
		wantAppErr(t, err, apperr.KindConflict, "invoice_not_draft")
	})
}

func TestIssue(t *testing.T) {
	due := DueAt(invoiceNow, 5, saoPaulo)
	t.Run("com saldo fica em aberto", func(t *testing.T) {
		inv := draftWith(t, 10000)
		if err := inv.Issue("FAT-2026-000007", invoiceNow, due); err != nil {
			t.Fatal(err)
		}
		if inv.Status != Open || inv.Number != "FAT-2026-000007" || !inv.IssuedAt.Equal(invoiceNow) || !inv.DueAt.Equal(due) || inv.PaidAt != nil {
			t.Fatalf("emissão: %+v", inv)
		}
		ch := inv.PopChanges()
		if len(ch) != 1 || *ch[0].From != Draft || ch[0].To != Open || ch[0].Reason != "issued" {
			t.Fatalf("histórico: %+v", ch)
		}
		if len(inv.PopChanges()) != 0 {
			t.Fatal("PopChanges deve limpar o histórico pendente")
		}
	})
	t.Run("saldo zero nasce paga", func(t *testing.T) {
		inv := draftWith(t, 10000)
		if _, err := inv.ApplyCredit(brl(10000)); err != nil {
			t.Fatal(err)
		}
		if err := inv.Issue("FAT-2026-000008", invoiceNow, due); err != nil {
			t.Fatal(err)
		}
		if inv.Status != Paid || inv.PaidAt == nil || !inv.PaidAt.Equal(invoiceNow) {
			t.Fatalf("fatura coberta por crédito: %s %v", inv.Status, inv.PaidAt)
		}
		if ch := inv.PopChanges(); ch[0].Reason != "issued_fully_credited" {
			t.Fatalf("motivo: %+v", ch)
		}
	})
	t.Run("sem itens", func(t *testing.T) {
		inv := newDraft()
		wantAppErr(t, inv.Issue("F", invoiceNow, due), apperr.KindInvalid, "invoice_without_items")
		if inv.Status != Draft || inv.Number != "" {
			t.Fatal("emissão rejeitada não altera a fatura")
		}
	})
	t.Run("já emitida", func(t *testing.T) {
		inv := issued(t, 100)
		wantAppErr(t, inv.Issue("F2", invoiceNow, due), apperr.KindConflict, "invoice_not_draft")
		if inv.Number != "FAT-2026-000001" {
			t.Fatal("número não pode ser trocado")
		}
	})
}

func TestApplyPaymentPartialThenFull(t *testing.T) {
	inv := issued(t, 10000)
	at := invoiceNow.Add(24 * time.Hour)

	applied, excess, err := inv.ApplyPayment(brl(4000), at)
	if err != nil || applied.Amount() != 4000 || !excess.IsZero() {
		t.Fatalf("parcial: %v %v %v", applied, excess, err)
	}
	if inv.Status != PartiallyPaid || inv.AmountDue.Amount() != 6000 || inv.PaidAt != nil {
		t.Fatalf("após parcial: %s saldo=%v", inv.Status, inv.AmountDue)
	}
	ch := inv.PopChanges()
	if len(ch) != 1 || ch[0].To != PartiallyPaid || ch[0].Details["applied"] != int64(4000) {
		t.Fatalf("histórico do parcial: %+v", ch)
	}

	if _, _, err := inv.ApplyPayment(brl(1000), at); err != nil || inv.Status != PartiallyPaid {
		t.Fatalf("segundo parcial: %v %s", err, inv.Status)
	}
	if len(inv.PopChanges()) != 0 {
		t.Fatal("parcial sobre parcial não gera transição")
	}

	applied, excess, err = inv.ApplyPayment(brl(5000), at)
	if err != nil || applied.Amount() != 5000 || !excess.IsZero() {
		t.Fatalf("quitação: %v %v %v", applied, excess, err)
	}
	if inv.Status != Paid || !inv.AmountDue.IsZero() || inv.AmountPaid.Amount() != 10000 || !inv.PaidAt.Equal(at) {
		t.Fatalf("após quitação: %+v", inv)
	}
	assertConservation(t, inv)
}

func TestApplyPaymentWithExcessReturnsIt(t *testing.T) {
	inv := issued(t, 10000)
	applied, excess, err := inv.ApplyPayment(brl(12500), invoiceNow)
	if err != nil || applied.Amount() != 10000 || excess.Amount() != 2500 {
		t.Fatalf("aplicado=%v excedente=%v err=%v", applied, excess, err)
	}
	if inv.Status != Paid || inv.AmountPaid.Amount() != 10000 {
		t.Fatalf("o excedente não entra no valor pago: %+v", inv)
	}
	ch := inv.PopChanges()
	if ch[0].Details["excess"] != int64(2500) {
		t.Fatalf("excedente registrado no histórico: %+v", ch)
	}
	assertConservation(t, inv)
}

func TestApplyPaymentRules(t *testing.T) {
	t.Run("valor não positivo", func(t *testing.T) {
		inv := issued(t, 1000)
		for _, v := range []int64{0, -1} {
			_, _, err := inv.ApplyPayment(brl(v), invoiceNow)
			wantAppErr(t, err, apperr.KindInvalid, "invalid_amount")
		}
	})
	for _, st := range []Status{Draft, Paid, Void} {
		t.Run("não aceita pagamento em "+string(st), func(t *testing.T) {
			inv := draftWith(t, 1000)
			inv.Status = st
			_, _, err := inv.ApplyPayment(brl(100), invoiceNow)
			wantAppErr(t, err, apperr.KindConflict, "invoice_not_payable")
		})
	}
	t.Run("moeda diferente", func(t *testing.T) {
		inv := issued(t, 1000)
		if _, _, err := inv.ApplyPayment(money.New(100, money.Currency{}), invoiceNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
	t.Run("moeda diferente em fatura incobrável", func(t *testing.T) {
		inv := issued(t, 1000)
		if _, err := inv.MarkUncollectible("cliente insolvente", invoiceNow); err != nil {
			t.Fatal(err)
		}
		if _, _, err := inv.ApplyPayment(money.New(100, money.Currency{}), invoiceNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
}

func TestPaymentOfUncollectibleInvoiceRevertsWriteOff(t *testing.T) {
	inv := issued(t, 10000)
	if _, _, err := inv.ApplyPayment(brl(4000), invoiceNow); err != nil {
		t.Fatal(err)
	}
	written, err := inv.MarkUncollectible("cliente não localizado", invoiceNow)
	if err != nil || written.Amount() != 6000 {
		t.Fatalf("baixa: %v %v", written, err)
	}
	inv.PopChanges()

	applied, excess, err := inv.ApplyPayment(brl(7000), invoiceNow.Add(time.Hour))
	if err != nil || applied.Amount() != 6000 || excess.Amount() != 1000 {
		t.Fatalf("recuperação total: aplicado=%v excedente=%v err=%v", applied, excess, err)
	}
	if inv.Status != Paid || !inv.AmountWrittenOff.IsZero() || inv.AmountPaid.Amount() != 10000 {
		t.Fatalf("baixa revertida integralmente: %+v", inv)
	}
	ch := inv.PopChanges()
	if len(ch) != 1 || *ch[0].From != Uncollectible || ch[0].To != Paid {
		t.Fatalf("transição: %+v", ch)
	}
	assertConservation(t, inv)
}

func TestPartialRecoveryOfUncollectibleInvoiceKeepsItUnpaid(t *testing.T) {
	inv := issued(t, 10000)
	if _, err := inv.MarkUncollectible("cliente não localizado", invoiceNow); err != nil {
		t.Fatal(err)
	}
	applied, _, err := inv.ApplyPayment(brl(2500), invoiceNow.Add(time.Hour))
	if err != nil || applied.Amount() != 2500 {
		t.Fatalf("recuperação parcial: %v %v", applied, err)
	}
	if inv.AmountWrittenOff.Amount() != 7500 {
		t.Fatalf("baixa revertida apenas no valor recebido: %v", inv.AmountWrittenOff)
	}
	if inv.Status == Paid || inv.PaidAt != nil {
		t.Fatalf("fatura com R$ 75,00 ainda baixados não pode ficar paga: %s", inv.Status)
	}
}

func TestVoid(t *testing.T) {
	at := invoiceNow.Add(time.Hour)
	t.Run("fatura em aberto sem pagamentos", func(t *testing.T) {
		inv := issued(t, 10000)
		if err := inv.Void("emitida em duplicidade", at); err != nil {
			t.Fatal(err)
		}
		if inv.Status != Void || !inv.VoidedAt.Equal(at) || inv.VoidReason != "emitida em duplicidade" || !inv.AmountDue.IsZero() {
			t.Fatalf("cancelamento: %+v", inv)
		}
		assertConservation(t, inv)
		if ch := inv.PopChanges(); len(ch) != 1 || ch[0].To != Void || ch[0].Reason != "emitida em duplicidade" {
			t.Fatalf("histórico: %+v", ch)
		}
		_, _, err := inv.ApplyPayment(brl(100), at)
		wantAppErr(t, err, apperr.KindConflict, "invoice_not_payable")
	})
	t.Run("com crédito aplicado pode ser cancelada", func(t *testing.T) {
		inv := draftWith(t, 10000)
		if _, err := inv.ApplyCredit(brl(2000)); err != nil {
			t.Fatal(err)
		}
		if err := inv.Issue("F", invoiceNow, invoiceNow); err != nil {
			t.Fatal(err)
		}
		if err := inv.Void("erro de cadastro", at); err != nil {
			t.Fatal(err)
		}
		// O crédito aplicado volta ao cliente (Service.Void): a fatura
		// cancelada não tem saldo, e o crédito aplicado fica registrado.
		if !inv.AmountDue.IsZero() || inv.CreditApplied.Amount() != 2000 || inv.Total.Amount() != 10000 {
			t.Fatalf("cancelada com crédito aplicado: saldo %v, crédito %v", inv.AmountDue, inv.CreditApplied)
		}
		assertConservation(t, inv)
	})
	t.Run("com pagamento parcial", func(t *testing.T) {
		inv := issued(t, 10000)
		if _, _, err := inv.ApplyPayment(brl(1000), at); err != nil {
			t.Fatal(err)
		}
		wantAppErr(t, inv.Void("erro de cadastro", at), apperr.KindConflict, "invoice_not_voidable")
	})
	t.Run("com pagamento registrado em aberto", func(t *testing.T) {
		inv := issued(t, 10000)
		inv.AmountPaid = brl(1)
		wantAppErr(t, inv.Void("erro de cadastro", at), apperr.KindConflict, "invoice_has_payments")
	})
	t.Run("com nota de crédito", func(t *testing.T) {
		inv := issued(t, 10000)
		if err := inv.ApplyCreditNote(brl(500), at); err != nil {
			t.Fatal(err)
		}
		wantAppErr(t, inv.Void("erro de cadastro", at), apperr.KindConflict, "invoice_has_payments")
	})
	for _, reason := range []string{"", "ab", strings.Repeat("x", 501)} {
		t.Run("motivo com tamanho inválido", func(t *testing.T) {
			inv := issued(t, 10000)
			wantAppErr(t, inv.Void(reason, at), apperr.KindInvalid, "invalid_reason")
			if inv.Status != Open {
				t.Fatal("motivo inválido não cancela")
			}
		})
	}
	for _, st := range []Status{Draft, Paid, Uncollectible, Void} {
		t.Run("não cancela em "+string(st), func(t *testing.T) {
			inv := issued(t, 10000)
			inv.Status = st
			wantAppErr(t, inv.Void("erro de cadastro", at), apperr.KindConflict, "invoice_not_voidable")
		})
	}
}

func TestMarkUncollectible(t *testing.T) {
	at := invoiceNow.Add(90 * 24 * time.Hour)
	inv := issued(t, 10000)
	if _, _, err := inv.ApplyPayment(brl(3000), at); err != nil {
		t.Fatal(err)
	}
	inv.PopChanges()
	written, err := inv.MarkUncollectible("falência decretada", at)
	if err != nil || written.Amount() != 7000 {
		t.Fatalf("baixa do saldo restante: %v %v", written, err)
	}
	if inv.Status != Uncollectible || !inv.AmountDue.IsZero() || inv.AmountWrittenOff.Amount() != 7000 || !inv.UncollectibleAt.Equal(at) {
		t.Fatalf("após baixa: %+v", inv)
	}
	ch := inv.PopChanges()
	if len(ch) != 1 || ch[0].Reason != "falência decretada" || ch[0].Details["written_off"] != int64(7000) {
		t.Fatalf("histórico: %+v", ch)
	}
	assertConservation(t, inv)
	if inv.IsOverdue(at.AddDate(1, 0, 0)) {
		t.Fatal("fatura baixada não é vencida")
	}

	for _, st := range []Status{Draft, Paid, Void, Uncollectible} {
		other := issued(t, 1000)
		other.Status = st
		_, err := other.MarkUncollectible("motivo", at)
		wantAppErr(t, err, apperr.KindConflict, "invoice_not_open")
	}
}

func TestApplyCreditNote(t *testing.T) {
	at := invoiceNow.Add(time.Hour)
	t.Run("parcial mantém a situação e registra histórico", func(t *testing.T) {
		inv := issued(t, 10000)
		if err := inv.ApplyCreditNote(brl(2500), at); err != nil {
			t.Fatal(err)
		}
		if inv.Status != Open || inv.AmountDue.Amount() != 7500 || inv.AmountCredited.Amount() != 2500 {
			t.Fatalf("após nota parcial: %+v", inv)
		}
		ch := inv.PopChanges()
		if len(ch) != 1 || *ch[0].From != Open || ch[0].To != Open || ch[0].Reason != "credit_note" || ch[0].Details["amount"] != int64(2500) {
			t.Fatalf("histórico: %+v", ch)
		}
		assertConservation(t, inv)
	})
	t.Run("total quita a fatura", func(t *testing.T) {
		inv := issued(t, 10000)
		if _, _, err := inv.ApplyPayment(brl(4000), at); err != nil {
			t.Fatal(err)
		}
		if err := inv.ApplyCreditNote(brl(6000), at); err != nil {
			t.Fatal(err)
		}
		if inv.Status != Paid || !inv.PaidAt.Equal(at) {
			t.Fatalf("nota que zera o saldo quita: %s", inv.Status)
		}
		assertConservation(t, inv)
	})
	t.Run("acima do saldo", func(t *testing.T) {
		inv := issued(t, 10000)
		wantAppErr(t, inv.ApplyCreditNote(brl(10001), at), apperr.KindInvalid, "credit_note_exceeds_due")
		wantAppErr(t, inv.ApplyCreditNote(money.New(1, money.Currency{}), at), apperr.KindInvalid, "credit_note_exceeds_due")
		if !inv.AmountCredited.IsZero() {
			t.Fatal("nota rejeitada não altera a fatura")
		}
	})
	t.Run("valor não positivo", func(t *testing.T) {
		inv := issued(t, 10000)
		wantAppErr(t, inv.ApplyCreditNote(brl(0), at), apperr.KindInvalid, "invalid_amount")
	})
	for _, st := range []Status{Draft, Paid, Void, Uncollectible} {
		t.Run("não se aplica em "+string(st), func(t *testing.T) {
			inv := issued(t, 10000)
			inv.Status = st
			wantAppErr(t, inv.ApplyCreditNote(brl(10), at), apperr.KindConflict, "invoice_not_open")
		})
	}
}

func TestIsOverdue(t *testing.T) {
	inv := issued(t, 10000) // vence no fim de 21/11/2026 (horário local)
	due := *inv.DueAt
	if inv.IsOverdue(due.Add(-time.Nanosecond)) {
		t.Fatal("ainda não venceu no último instante do dia de vencimento")
	}
	if !inv.IsOverdue(due) {
		t.Fatal("vence no primeiro instante do dia seguinte")
	}
	if _, _, err := inv.ApplyPayment(brl(100), invoiceNow); err != nil {
		t.Fatal(err)
	}
	if !inv.IsOverdue(due) {
		t.Fatal("parcialmente paga com saldo vencido está vencida")
	}
	if _, _, err := inv.ApplyPayment(brl(9900), invoiceNow); err != nil {
		t.Fatal(err)
	}
	if inv.IsOverdue(due.AddDate(1, 0, 0)) {
		t.Fatal("fatura paga não vence")
	}
	if draftWith(t, 10).IsOverdue(due) {
		t.Fatal("rascunho sem vencimento não vence")
	}
}

func TestDueAtAndDueDate(t *testing.T) {
	cases := []struct {
		name     string
		issuedAt time.Time
		terms    int
		dueAt    time.Time
		dueDate  string
	}{
		{"cinco dias", local(2026, time.November, 16, 14, 0), 5, local(2026, time.November, 22, 0, 0), "2026-11-21"},
		{"vencimento no mesmo dia", local(2026, time.November, 16, 14, 0), 0, local(2026, time.November, 17, 0, 0), "2026-11-16"},
		// 01:30 UTC de 17/11 ainda é 16/11 em São Paulo.
		{"data local da emissão", time.Date(2026, time.November, 17, 1, 30, 0, 0, time.UTC), 1, local(2026, time.November, 18, 0, 0), "2026-11-17"},
		{"virada de ano", local(2026, time.December, 30, 10, 0), 3, local(2027, time.January, 3, 0, 0), "2027-01-02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DueAt(c.issuedAt, c.terms, saoPaulo)
			if !got.Equal(c.dueAt) || got.Location() != time.UTC {
				t.Fatalf("DueAt=%v, esperado %v em UTC", got, c.dueAt)
			}
			if d := DueDate(got, saoPaulo).String(); d != c.dueDate {
				t.Fatalf("DueDate=%s, esperado %s", d, c.dueDate)
			}
		})
	}
}

func TestFormatNumber(t *testing.T) {
	cases := map[string]string{
		FormatNumber("FAT", 2026, 1):         "FAT-2026-000001",
		FormatNumber("FAT", 2026, 42):        "FAT-2026-000042",
		FormatNumber("REC", 2027, 999999):    "REC-2027-999999",
		FormatNumber("FAT", 2026, 1_234_567): "FAT-2026-1234567",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("FormatNumber=%q, esperado %q", got, want)
		}
	}
}

// boundedNonNegative reduz um valor arbitrário ao intervalo [0, limit].
func boundedNonNegative(v, limit int64) int64 { return int64(uint64(v) % uint64(limit+1)) }

// FuzzApplyDiscount: a soma dos descontos nunca excede o subtotal positivo,
// o total nunca fica negativo e a distribuição é determinística.
func FuzzApplyDiscount(f *testing.F) {
	f.Add(int64(1000), int64(2000), int64(3000), int64(100), int64(0))
	f.Add(int64(1), int64(0), int64(0), int64(5), int64(7))
	f.Add(int64(0), int64(0), int64(0), int64(100), int64(100))
	f.Add(int64(999_999), int64(1), int64(33), int64(-50), int64(1_000_000_000))
	f.Fuzz(func(t *testing.T, a, b, c, d1, d2 int64) {
		const limit = 1_000_000_000_00
		amounts := []int64{boundedNonNegative(a, limit), boundedNonNegative(b, limit), boundedNonNegative(c, limit)}
		discounts := []int64{d1 % (3 * limit), d2 % (3 * limit)}
		build := func() (*Invoice, []money.Money) {
			inv := draftWith(t, amounts...)
			var applied []money.Money
			for _, d := range discounts {
				got, err := inv.ApplyDiscount(brl(d))
				if err != nil {
					t.Fatalf("ApplyDiscount(%d) com itens %v: %v", d, amounts, err)
				}
				applied = append(applied, got)
			}
			return inv, applied
		}
		inv, applied := build()
		positive := amounts[0] + amounts[1] + amounts[2]
		var total int64
		for _, ap := range applied {
			if ap.IsNegative() {
				t.Fatalf("desconto aplicado negativo: %v", ap)
			}
			total += ap.Amount()
		}
		if total > positive || inv.DiscountTotal.Amount() != total {
			t.Fatalf("descontos %d excedem o subtotal positivo %d (DiscountTotal=%v)", total, positive, inv.DiscountTotal)
		}
		var perItem int64
		for _, it := range inv.Items {
			if it.DiscountAmount.IsNegative() || it.DiscountAmount.Amount() > it.Amount.Amount() {
				t.Fatalf("desconto do item fora do intervalo: %+v", it)
			}
			perItem += it.DiscountAmount.Amount()
		}
		if perItem != total {
			t.Fatalf("soma dos itens %d difere do desconto total %d", perItem, total)
		}
		if inv.Total.IsNegative() {
			t.Fatalf("total negativo: %v", inv.Total)
		}
		assertConservation(t, inv)
		again, _ := build()
		for i := range inv.Items {
			if inv.Items[i].DiscountAmount != again.Items[i].DiscountAmount {
				t.Fatal("distribuição não determinística")
			}
		}
	})
}

// FuzzApplyPayment: em qualquer sequência de pagamentos, notas de crédito,
// baixas e cancelamentos, pago + saldo + creditado + baixado + crédito
// aplicado = total.
func FuzzApplyPayment(f *testing.F) {
	f.Add(int64(10000), int64(0), []byte{0, 0x40, 0x00, 0, 0xff, 0xff})
	f.Add(int64(10000), int64(3000), []byte{2, 0, 0, 0, 0x20, 0x00, 0, 0xff, 0xff})
	f.Add(int64(1), int64(1), []byte{0, 1, 1})
	f.Add(int64(500_000), int64(0), []byte{1, 0x10, 0x00, 3, 0, 0, 0, 0x80, 0x00, 2, 0, 0, 0, 0x01, 0x00})
	f.Fuzz(func(t *testing.T, rawTotal, rawCredit int64, script []byte) {
		total := 1 + boundedNonNegative(rawTotal, 1_000_000_000)
		credit := boundedNonNegative(rawCredit, total)
		inv := draftWith(t, total)
		if _, err := inv.ApplyCredit(brl(credit)); err != nil {
			t.Fatal(err)
		}
		if err := inv.Issue("FAT-2026-000001", invoiceNow, DueAt(invoiceNow, 5, saoPaulo)); err != nil {
			t.Fatal(err)
		}
		assertConservation(t, inv)
		at := invoiceNow
		for i := 0; i+2 < len(script) && i < 90; i += 3 {
			at = at.Add(time.Hour)
			amount := (int64(script[i+1])<<8 | int64(script[i+2])) * 2 * total / 0xffff
			before := inv.Status
			switch script[i] % 4 {
			case 0:
				applied, excess, err := inv.ApplyPayment(brl(amount), at)
				if err == nil {
					if applied.IsNegative() || excess.IsNegative() || applied.Amount()+excess.Amount() != amount {
						t.Fatalf("aplicado %v + excedente %v != recebido %d", applied, excess, amount)
					}
				} else if amount > 0 && (before == Open || before == PartiallyPaid || before == Uncollectible) {
					t.Fatalf("pagamento válido rejeitado em %s: %v", before, err)
				}
			case 1:
				_ = inv.ApplyCreditNote(brl(amount), at)
			case 2:
				_, _ = inv.MarkUncollectible("baixa", at)
			case 3:
				_ = inv.Void("cancelamento", at)
			}
			assertConservation(t, inv)
			if inv.Status == Paid && !inv.AmountDue.IsZero() {
				t.Fatalf("fatura paga com saldo %v", inv.AmountDue)
			}
			if (inv.Status == Open || inv.Status == PartiallyPaid) && !inv.AmountDue.IsPositive() {
				t.Fatalf("fatura %s sem saldo devedor", inv.Status)
			}
			if inv.Status == Void && !inv.AmountPaid.IsZero() {
				t.Fatal("fatura cancelada com pagamento")
			}
			if before != inv.Status && !CanTransition(before, inv.Status) {
				t.Fatalf("transição inválida %s -> %s", before, inv.Status)
			}
		}
	})
}
