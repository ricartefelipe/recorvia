package payments

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var allChargeStatuses = []ChargeStatus{ChargePending, ChargeAwaitingPayment, ChargePaid, ChargeFailed, ChargeCanceled,
	ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed, ChargeChargedBack}

var chargeNow = time.Date(2026, time.November, 16, 17, 30, 0, 0, time.UTC)

func brl(v int64) money.Money { return money.New(v, money.BRL) }

func newTestCharge(t *testing.T, amount int64) Charge {
	t.Helper()
	c, err := NewCharge(uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), "simulator", 1, brl(amount),
		calendar.Date{Year: 2026, Month: time.November, Day: 21}, chargeNow)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func paidCharge(t *testing.T, amount int64) Charge {
	t.Helper()
	c := newTestCharge(t, amount)
	if err := c.MarkCreated("pay_1", "https://checkout.exemplo/pay_1", chargeNow); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkPaid(brl(amount), brl(199), chargeNow, chargeNow); err != nil {
		t.Fatal(err)
	}
	return c
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

func TestChargeCanTransitionMatrix(t *testing.T) {
	allowed := map[ChargeStatus][]ChargeStatus{
		ChargePending:           {ChargeAwaitingPayment, ChargePaid, ChargeFailed, ChargeCanceled},
		ChargeAwaitingPayment:   {ChargePaid, ChargeFailed, ChargeCanceled},
		ChargePaid:              {ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed},
		ChargePartiallyRefunded: {ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed},
		ChargeDisputed:          {ChargePaid, ChargePartiallyRefunded, ChargeChargedBack},
		ChargeFailed:            {ChargePaid},
		ChargeCanceled:          {ChargePaid},
	}
	for _, from := range allChargeStatuses {
		for _, to := range allChargeStatuses {
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

func TestIsSettledStatus(t *testing.T) {
	settled := map[ChargeStatus]bool{ChargePaid: true, ChargePartiallyRefunded: true, ChargeRefunded: true, ChargeDisputed: true, ChargeChargedBack: true}
	for _, s := range allChargeStatuses {
		if IsSettledStatus(s) != settled[s] {
			t.Errorf("IsSettledStatus(%s)=%v", s, !settled[s])
		}
	}
}

func TestNewCharge(t *testing.T) {
	c := newTestCharge(t, 10000)
	if c.Status != ChargePending || c.Version != 1 || c.Attempt != 1 || c.Amount.Amount() != 10000 {
		t.Fatalf("cobrança nova: %+v", c)
	}
	for _, m := range []money.Money{c.AmountPaid, c.Fee, c.AmountRefunded, c.AmountChargedBack} {
		if !m.IsZero() || m.Currency() != money.BRL {
			t.Fatalf("valores acessórios começam em zero: %v", m)
		}
	}
	for _, v := range []int64{0, -1} {
		_, err := NewCharge(uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), "simulator", 1, brl(v), calendar.Date{}, chargeNow)
		wantAppErr(t, err, apperr.KindInvalid, "invalid_amount")
	}
}

func TestMarkCreated(t *testing.T) {
	c := newTestCharge(t, 10000)
	at := chargeNow.Add(time.Second)
	if err := c.MarkCreated("pay_1", "https://checkout.exemplo/pay_1", at); err != nil {
		t.Fatal(err)
	}
	if c.Status != ChargeAwaitingPayment || c.ProviderChargeID != "pay_1" || c.CheckoutURL != "https://checkout.exemplo/pay_1" || !c.UpdatedAt.Equal(at) {
		t.Fatalf("criação confirmada: %+v", c)
	}
	if err := c.MarkCreated("pay_1", "https://checkout.exemplo/novo", at); err != nil || c.Status != ChargeAwaitingPayment {
		t.Fatalf("confirmação repetida é idempotente: %v %s", err, c.Status)
	}
	wantAppErr(t, c.MarkCreated("pay_2", "", at), apperr.KindConflict, "provider_charge_mismatch")
	if c.ProviderChargeID != "pay_1" {
		t.Fatal("vínculo existente não pode ser trocado")
	}

	paid := paidCharge(t, 10000)
	if err := paid.MarkCreated("pay_1", "https://checkout.exemplo/pay_1", at); err != nil || paid.Status != ChargePaid {
		t.Fatalf("criação confirmada após pagamento não regride: %v %s", err, paid.Status)
	}
}

func TestMarkPaidIsIdempotent(t *testing.T) {
	c := newTestCharge(t, 10000)
	paidAt := chargeNow.Add(-time.Hour)
	if err := c.MarkPaid(brl(10250), brl(199), paidAt, chargeNow); err != nil {
		t.Fatal(err)
	}
	if c.Status != ChargePaid || c.AmountPaid.Amount() != 10250 || c.Fee.Amount() != 199 || !c.PaidAt.Equal(paidAt) {
		t.Fatalf("pagamento: %+v", c)
	}
	if err := c.MarkPaid(brl(1), brl(0), chargeNow, chargeNow); err != nil {
		t.Fatalf("confirmação repetida: %v", err)
	}
	if c.AmountPaid.Amount() != 10250 || c.Fee.Amount() != 199 || !c.PaidAt.Equal(paidAt) {
		t.Fatal("confirmação repetida não altera valores já registrados")
	}
	for _, st := range []ChargeStatus{ChargePartiallyRefunded, ChargeRefunded, ChargeDisputed, ChargeChargedBack} {
		s := paidCharge(t, 100)
		s.Status = st
		if err := s.MarkPaid(brl(100), brl(0), chargeNow, chargeNow); err != nil || s.Status != st {
			t.Fatalf("cobrança liquidada em %s não volta a paga: %v %s", st, err, s.Status)
		}
	}
}

func TestMarkPaidRules(t *testing.T) {
	c := newTestCharge(t, 10000)
	wantAppErr(t, c.MarkPaid(brl(0), brl(0), chargeNow, chargeNow), apperr.KindInvalid, "invalid_amount")
	wantAppErr(t, c.MarkPaid(money.New(100, money.Currency{}), brl(0), chargeNow, chargeNow), apperr.KindInvalid, "invalid_amount")
	for _, st := range []ChargeStatus{ChargeFailed, ChargeCanceled} {
		closed := newTestCharge(t, 10000)
		closed.Status = st
		wantAppErr(t, closed.MarkPaid(brl(0), brl(0), chargeNow, chargeNow), apperr.KindInvalid, "invalid_amount")
		if closed.Status != st || closed.PaidAt != nil {
			t.Fatalf("valor inválido não altera a cobrança em %s", st)
		}
	}
	unknown := newTestCharge(t, 10000)
	unknown.Status = ChargeStatus("desconhecida")
	wantAppErr(t, unknown.MarkPaid(brl(10000), brl(0), chargeNow, chargeNow), apperr.KindConflict, "invalid_charge_transition")
	if unknown.PaidAt != nil || !unknown.AmountPaid.IsZero() {
		t.Fatal("transição rejeitada não registra pagamento")
	}
}

// Recusa e cancelamento locais não encerram a cobrança no provedor: a
// confirmação de pagamento é a única saída desses estados e leva a cobrança
// a paga, para que o valor recebido nunca seja descartado.
func TestMarkPaidConfirmsFailedOrCanceledCharge(t *testing.T) {
	cases := []struct {
		name  string
		close func(*Charge) error
		want  ChargeStatus
	}{
		{"recusada pelo provedor", func(c *Charge) error { return c.MarkFailed("cartão recusado", chargeNow) }, ChargeFailed},
		{"cancelada localmente", func(c *Charge) error { return c.MarkCanceled(chargeNow) }, ChargeCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCharge(t, 10000)
			if err := c.MarkCreated("pay_1", "https://checkout.exemplo/pay_1", chargeNow); err != nil {
				t.Fatal(err)
			}
			if err := tc.close(&c); err != nil || c.Status != tc.want {
				t.Fatalf("encerramento local: %v %s", err, c.Status)
			}
			paidAt, at := chargeNow.Add(time.Hour), chargeNow.Add(2*time.Hour)
			if err := c.MarkPaid(brl(10000), brl(199), paidAt, at); err != nil {
				t.Fatalf("confirmação do provedor para cobrança em %s: %v", tc.want, err)
			}
			if c.Status != ChargePaid || c.AmountPaid.Amount() != 10000 || c.Fee.Amount() != 199 || !c.PaidAt.Equal(paidAt) || !c.UpdatedAt.Equal(at) {
				t.Fatalf("cobrança paga após %s: %+v", tc.want, c)
			}
			if c.FailureReason != "" {
				t.Fatalf("motivo da recusa não descreve a cobrança paga: %q", c.FailureReason)
			}
			for _, to := range []ChargeStatus{ChargePending, ChargeAwaitingPayment, ChargeFailed, ChargeCanceled, ChargeRefunded} {
				if CanTransition(tc.want, to) {
					t.Fatalf("%s só sai para paga, não para %s", tc.want, to)
				}
			}
			wantAppErr(t, c.MarkCanceled(at), apperr.KindConflict, "invalid_charge_transition")
			wantAppErr(t, c.MarkFailed("x", at), apperr.KindConflict, "invalid_charge_transition")
		})
	}
}

func TestMarkFailedAndCanceled(t *testing.T) {
	c := newTestCharge(t, 10000)
	if err := c.MarkFailed(strings.Repeat("é", 600), chargeNow); err != nil {
		t.Fatal(err)
	}
	if c.Status != ChargeFailed || len([]rune(c.FailureReason)) != 500 {
		t.Fatalf("falha com motivo truncado em 500 caracteres: %s %d", c.Status, len([]rune(c.FailureReason)))
	}
	short := newTestCharge(t, 10000)
	if err := short.MarkFailed("cartão recusado", chargeNow); err != nil || short.FailureReason != "cartão recusado" {
		t.Fatalf("motivo curto preservado: %q", short.FailureReason)
	}
	p := paidCharge(t, 10000)
	wantAppErr(t, p.MarkFailed("x", chargeNow), apperr.KindConflict, "invalid_charge_transition")
	wantAppErr(t, p.MarkCanceled(chargeNow), apperr.KindConflict, "invalid_charge_transition")

	a := newTestCharge(t, 10000)
	_ = a.MarkCreated("pay_9", "", chargeNow)
	if err := a.MarkCanceled(chargeNow); err != nil || a.Status != ChargeCanceled {
		t.Fatalf("cancelamento: %v %s", err, a.Status)
	}
}

func TestRefunds(t *testing.T) {
	c := paidCharge(t, 10000)
	if r, _ := c.Refundable(); r.Amount() != 10000 {
		t.Fatalf("saldo reembolsável: %v", r)
	}
	wantAppErr(t, c.ApplyRefund(brl(10001), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
	wantAppErr(t, c.ApplyRefund(brl(0), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
	wantAppErr(t, c.ApplyRefund(money.New(1, money.Currency{}), chargeNow), apperr.KindConflict, "refund_exceeds_balance")

	if err := c.ApplyRefund(brl(3000), chargeNow); err != nil || c.Status != ChargePartiallyRefunded {
		t.Fatalf("reembolso parcial: %v %s", err, c.Status)
	}
	if err := c.ApplyRefund(brl(2000), chargeNow); err != nil || c.Status != ChargePartiallyRefunded {
		t.Fatalf("segundo reembolso parcial: %v %s", err, c.Status)
	}
	if r, _ := c.Refundable(); r.Amount() != 5000 {
		t.Fatalf("saldo após reembolsos: %v", r)
	}
	if err := c.ApplyRefund(brl(5000), chargeNow); err != nil || c.Status != ChargeRefunded || c.AmountRefunded.Amount() != 10000 {
		t.Fatalf("reembolso total: %v %s %v", err, c.Status, c.AmountRefunded)
	}
	wantAppErr(t, c.ApplyRefund(brl(1), chargeNow), apperr.KindConflict, "refund_exceeds_balance")

	pending := newTestCharge(t, 10000)
	wantAppErr(t, pending.ApplyRefund(brl(1), chargeNow), apperr.KindConflict, "refund_exceeds_balance")
}

func TestDisputes(t *testing.T) {
	t.Run("ganha volta a paga", func(t *testing.T) {
		c := paidCharge(t, 10000)
		if err := c.OpenDispute(chargeNow); err != nil || c.Status != ChargeDisputed {
			t.Fatalf("abertura: %v %s", err, c.Status)
		}
		if err := c.CloseDispute(true, brl(0), chargeNow); err != nil || c.Status != ChargePaid || !c.AmountChargedBack.IsZero() {
			t.Fatalf("disputa ganha: %v %s", err, c.Status)
		}
	})
	t.Run("ganha com reembolso anterior volta a parcialmente reembolsada", func(t *testing.T) {
		c := paidCharge(t, 10000)
		_ = c.ApplyRefund(brl(1000), chargeNow)
		_ = c.OpenDispute(chargeNow)
		if err := c.CloseDispute(true, brl(0), chargeNow); err != nil || c.Status != ChargePartiallyRefunded {
			t.Fatalf("disputa ganha: %v %s", err, c.Status)
		}
	})
	t.Run("perdida registra o estorno", func(t *testing.T) {
		c := paidCharge(t, 10000)
		_ = c.OpenDispute(chargeNow)
		if err := c.CloseDispute(false, brl(10000), chargeNow); err != nil || c.Status != ChargeChargedBack || c.AmountChargedBack.Amount() != 10000 {
			t.Fatalf("disputa perdida: %v %s %v", err, c.Status, c.AmountChargedBack)
		}
		if r, _ := c.Refundable(); !r.IsZero() {
			t.Fatalf("nada a reembolsar após chargeback: %v", r)
		}
	})
	t.Run("sem disputa aberta", func(t *testing.T) {
		c := paidCharge(t, 10000)
		wantAppErr(t, c.CloseDispute(true, brl(0), chargeNow), apperr.KindConflict, "no_open_dispute")
	})
	t.Run("perdida em moeda diferente", func(t *testing.T) {
		c := paidCharge(t, 10000)
		_ = c.OpenDispute(chargeNow)
		if err := c.CloseDispute(false, money.New(1, money.Currency{}), chargeNow); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("esperava moedas diferentes: %v", err)
		}
	})
	t.Run("pendente não abre disputa", func(t *testing.T) {
		c := newTestCharge(t, 10000)
		wantAppErr(t, c.OpenDispute(chargeNow), apperr.KindConflict, "invalid_charge_transition")
	})
}

func TestRefundableWithMismatchedCurrency(t *testing.T) {
	c := paidCharge(t, 10000)
	c.AmountRefunded = money.New(1, money.Currency{})
	if _, err := c.Refundable(); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("esperava moedas diferentes: %v", err)
	}
	if err := c.ApplyRefund(brl(1), chargeNow); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("esperava moedas diferentes: %v", err)
	}
}

func TestRecordProviderStatus(t *testing.T) {
	c := newTestCharge(t, 100)
	at := chargeNow.Add(-time.Minute)
	c.RecordProviderStatus("PENDING", &at)
	c.RecordProviderStatus("CONFIRMED", nil)
	if c.ProviderStatus != "CONFIRMED" || !c.ProviderStatusAt.Equal(at) {
		t.Fatalf("instante do provedor só muda quando informado: %s %v", c.ProviderStatus, c.ProviderStatusAt)
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{6, 16 * time.Minute},
		{7, 30 * time.Minute}, // 32 minutos limitado a 30
		{50, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := Backoff(c.attempt); got != c.want {
			t.Errorf("Backoff(%d)=%v, esperado %v", c.attempt, got, c.want)
		}
	}
	prev := time.Duration(0)
	for a := 0; a < 200; a++ {
		d := Backoff(a)
		if d < prev || d > 30*time.Minute {
			t.Fatalf("Backoff(%d)=%v fora da progressão limitada", a, d)
		}
		prev = d
	}
}

func TestOperationClaimable(t *testing.T) {
	now := chargeNow
	past, future := now.Add(-time.Second), now.Add(time.Second)
	cases := []struct {
		name string
		op   Operation
		want bool
	}{
		{"pendente vencida", Operation{Status: OpPending, NextAttemptAt: now}, true},
		{"pendente futura", Operation{Status: OpPending, NextAttemptAt: future}, false},
		{"pendente no limite de tentativas", Operation{Status: OpPending, NextAttemptAt: past, Attempts: MaxOperationAttempts}, false},
		{"desconhecida vencida", Operation{Status: OpUnknown, NextAttemptAt: past, Attempts: MaxOperationAttempts - 1}, true},
		{"desconhecida futura", Operation{Status: OpUnknown, NextAttemptAt: future}, false},
		{"em execução com lease expirado", Operation{Status: OpInFlight, LeaseUntil: &past}, true},
		{"em execução com lease vigente", Operation{Status: OpInFlight, LeaseUntil: &future}, false},
		{"em execução no instante exato do lease", Operation{Status: OpInFlight, LeaseUntil: &now}, false},
		{"em execução sem lease", Operation{Status: OpInFlight}, false},
		{"concluída", Operation{Status: OpSucceeded, NextAttemptAt: past}, false},
		{"falhou", Operation{Status: OpFailed, NextAttemptAt: past}, false},
	}
	for _, c := range cases {
		if got := c.op.Claimable(now); got != c.want {
			t.Errorf("%s: Claimable=%v, esperado %v", c.name, got, c.want)
		}
	}
}

// Operações que chegaram ao limite de tentativas e não são mais
// reivindicadas pela recuperação (sem alerta próprio até esta contagem). Uma
// criação pendente cuja cobrança foi cancelada não conta: nada ficou com
// resultado incerto e a cobrança não será mais criada.
func TestOperationExhausted(t *testing.T) {
	now := chargeNow
	past, future := now.Add(-time.Second), now.Add(time.Second)
	limit := MaxOperationAttempts
	create := func(status OperationStatus, next time.Time, lease *time.Time, attempts int) Operation {
		return Operation{Kind: OpCreateCharge, Status: status, NextAttemptAt: next, LeaseUntil: lease, Attempts: attempts}
	}
	cases := []struct {
		name   string
		op     Operation
		charge ChargeStatus
		want   bool
	}{
		{"pendente no limite e vencida", create(OpPending, past, nil, limit), ChargePending, true},
		{"pendente no limite antes da próxima tentativa", create(OpPending, future, nil, limit), ChargePending, false},
		{"pendente abaixo do limite", create(OpPending, past, nil, limit-1), ChargePending, false},
		{"pendente no limite com a cobrança cancelada", create(OpPending, past, nil, limit), ChargeCanceled, false},
		{"desconhecida no limite", create(OpUnknown, now, nil, limit), ChargePending, true},
		{"desconhecida no limite com a cobrança cancelada", create(OpUnknown, now, nil, limit), ChargeCanceled, true},
		{"em execução no limite com lease vencido", create(OpInFlight, time.Time{}, &past, limit), ChargePending, true},
		{"em execução no limite com lease vencido e a cobrança cancelada", create(OpInFlight, time.Time{}, &past, limit), ChargeCanceled, true},
		{"em execução no limite com lease vigente", create(OpInFlight, time.Time{}, &future, limit), ChargePending, false},
		{"em execução no limite sem lease", create(OpInFlight, time.Time{}, nil, limit), ChargePending, false},
		{"concluída no limite", create(OpSucceeded, past, nil, limit), ChargeAwaitingPayment, false},
		{"falhou no limite", create(OpFailed, past, nil, limit), ChargeFailed, false},
		{"reembolso pendente no limite", Operation{Kind: OpRefund, Status: OpPending, NextAttemptAt: past, Attempts: limit}, ChargePaid, true},
	}
	for _, c := range cases {
		if got := c.op.Exhausted(now, c.charge); got != c.want {
			t.Errorf("%s: Exhausted=%v, esperado %v", c.name, got, c.want)
		}
		if c.want && c.op.Claimable(now) {
			t.Errorf("%s: operação esgotada não pode ser reivindicável", c.name)
		}
	}
}

// Só a criação de cobrança se torna obsoleta, e só quando a cobrança deixou
// de estar pendente.
func TestOperationObsolete(t *testing.T) {
	cases := []struct {
		kind   OperationKind
		charge ChargeStatus
		want   bool
	}{
		{OpCreateCharge, ChargePending, false},
		{OpCreateCharge, ChargeCanceled, true},
		{OpCreateCharge, ChargeFailed, true},
		{OpCreateCharge, ChargeAwaitingPayment, true},
		{OpRefund, ChargePaid, false},
		{OpRefund, ChargeCanceled, false},
	}
	for _, c := range cases {
		if got := (Operation{Kind: c.kind}).Obsolete(c.charge); got != c.want {
			t.Errorf("%s com cobrança %s: Obsolete=%v, esperado %v", c.kind, c.charge, got, c.want)
		}
	}
}

// A reexecução manual antecipa a tentativa (ignora next_attempt_at), mas
// recusa com conflito o que não pode ser executado.
func TestOperationCheckManualRetry(t *testing.T) {
	now := chargeNow
	past, future := now.Add(-time.Second), now.Add(time.Second)
	cases := []struct {
		name string
		op   Operation
		code string
	}{
		{"pendente antes da próxima tentativa", Operation{Status: OpPending, NextAttemptAt: future, Attempts: 1}, ""},
		{"desconhecida antes da próxima tentativa", Operation{Status: OpUnknown, NextAttemptAt: future, Attempts: 2}, ""},
		{"em execução com lease vencido", Operation{Status: OpInFlight, LeaseUntil: &past, Attempts: 1}, ""},
		{"em execução com lease vigente", Operation{Status: OpInFlight, LeaseUntil: &future, Attempts: 1}, "operation_in_progress"},
		{"em execução sem lease", Operation{Status: OpInFlight, Attempts: 1}, "operation_in_progress"},
		{"desconhecida esgotada", Operation{Status: OpUnknown, NextAttemptAt: past, Attempts: MaxOperationAttempts}, "operation_attempts_exhausted"},
		{"em execução esgotada com lease vencido", Operation{Status: OpInFlight, LeaseUntil: &past, Attempts: MaxOperationAttempts},
			"operation_attempts_exhausted"},
		{"pendente esgotada", Operation{Status: OpPending, NextAttemptAt: future, Attempts: MaxOperationAttempts}, ""},
		{"concluída", Operation{Status: OpSucceeded, Attempts: 1}, "operation_completed"},
		{"falhou", Operation{Status: OpFailed, Attempts: 1}, "operation_completed"},
		{"situação desconhecida", Operation{Status: "outra"}, "operation_not_retryable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.op.CheckManualRetry(now)
			if c.code == "" {
				if err != nil {
					t.Fatalf("reexecução permitida: %v", err)
				}
				return
			}
			wantAppErr(t, err, apperr.KindConflict, c.code)
		})
	}
}

func TestProviderStatusIsPaid(t *testing.T) {
	paid := map[ProviderStatus]bool{ProviderConfirmed: true, ProviderReceived: true, ProviderRefundInProgress: true,
		ProviderPartiallyRefunded: true, ProviderRefunded: true, ProviderChargebackRequested: true, ProviderChargebackDispute: true,
		ProviderChargebackReversed: true, ProviderChargebackLost: true}
	for _, s := range []ProviderStatus{ProviderPending, ProviderConfirmed, ProviderReceived, ProviderOverdue, ProviderFailed, ProviderCanceled,
		ProviderRefundInProgress, ProviderPartiallyRefunded, ProviderRefunded, ProviderChargebackRequested, ProviderChargebackDispute,
		ProviderChargebackReversed, ProviderChargebackLost, ProviderUnknownStatus} {
		if s.IsPaid() != paid[s] {
			t.Errorf("%s.IsPaid()=%v", s, !paid[s])
		}
	}
}

func TestFeeOrDerived(t *testing.T) {
	fee, net, bigNet := brl(199), brl(9801), brl(10001)
	cases := []struct {
		name string
		pc   ProviderCharge
		want int64
	}{
		{"taxa informada", ProviderCharge{Amount: brl(10000), Fee: &fee, NetAmount: &net}, 199},
		{"derivada do líquido", ProviderCharge{Amount: brl(10000), NetAmount: &net}, 199},
		{"líquido maior que o bruto", ProviderCharge{Amount: brl(10000), NetAmount: &bigNet}, 0},
		{"sem informação", ProviderCharge{Amount: brl(10000)}, 0},
	}
	for _, c := range cases {
		got := c.pc.FeeOrDerived()
		if got.Amount() != c.want || got.Currency() != money.BRL {
			t.Errorf("%s: %v, esperado %d", c.name, got, c.want)
		}
	}
}

func TestGatewayErrorKind(t *testing.T) {
	cause := errors.New("connection reset")
	ge := &GatewayError{Kind: ErrKindRetryable, StatusCode: 429, Code: "rate_limited", Message: "muitas requisições", Err: cause}
	if GatewayErrorKind(fmt.Errorf("criar cobrança: %w", ge)) != ErrKindRetryable {
		t.Fatal("classificação deve atravessar o encadeamento")
	}
	if !errors.Is(ge, cause) {
		t.Fatal("GatewayError deve expor a causa")
	}
	if msg := ge.Error(); !strings.Contains(msg, "status=429") || !strings.Contains(msg, "rate_limited") {
		t.Fatalf("mensagem: %q", msg)
	}
	if GatewayErrorKind(errors.New("erro qualquer")) != ErrKindAmbiguous {
		t.Fatal("erro não classificado é tratado como ambíguo")
	}
}

func TestHashEndpointToken(t *testing.T) {
	want := sha256.Sum256([]byte("token-do-endpoint"))
	if got := HashEndpointToken("token-do-endpoint"); string(got) != string(want[:]) {
		t.Fatal("hash do token deve ser SHA-256")
	}
}

// FuzzChargeRefundsNeverExceedPaid: qualquer sequência de reembolsos e
// disputas mantém reembolsado + estornado <= pago e o saldo reembolsável
// nunca negativo.
func FuzzChargeRefundsNeverExceedPaid(f *testing.F) {
	f.Add(int64(10000), []byte{0, 30, 0, 70, 1, 0, 2, 50})
	f.Add(int64(1), []byte{0, 255, 1, 0, 3, 100})
	f.Add(int64(999_999), []byte{1, 0, 2, 0, 0, 10, 1, 0, 3, 255})
	f.Fuzz(func(t *testing.T, rawPaid int64, script []byte) {
		paid := 1 + int64(uint64(rawPaid)%uint64(money.MaxAmount))
		c := paidCharge(t, paid)
		for i := 0; i+1 < len(script) && i < 60; i += 2 {
			amount := int64(script[i+1]) * paid / 100
			switch script[i] % 4 {
			case 0:
				_ = c.ApplyRefund(brl(amount), chargeNow)
			case 1:
				_ = c.OpenDispute(chargeNow)
			case 2:
				_ = c.CloseDispute(true, brl(0), chargeNow)
			case 3:
				elig, _ := c.Refundable()
				_ = c.CloseDispute(false, elig, chargeNow)
			}
			elig, err := c.Refundable()
			if err != nil || elig.IsNegative() {
				t.Fatalf("saldo reembolsável inválido: %v %v", elig, err)
			}
			if c.AmountRefunded.Amount()+c.AmountChargedBack.Amount() > c.AmountPaid.Amount() {
				t.Fatalf("reembolsado %v + estornado %v > pago %v", c.AmountRefunded, c.AmountChargedBack, c.AmountPaid)
			}
			if !IsSettledStatus(c.Status) {
				t.Fatalf("cobrança paga não pode sair dos estados liquidados: %s", c.Status)
			}
			if c.Status == ChargeRefunded && c.AmountRefunded != c.AmountPaid {
				t.Fatalf("reembolsada sem reembolso integral: %v de %v", c.AmountRefunded, c.AmountPaid)
			}
		}
	})
}
