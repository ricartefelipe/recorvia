package ledger

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

var (
	tenantID = uuid.MustParse("0190a000-0000-7000-8000-000000000001")
	refID    = uuid.MustParse("0190a000-0000-7000-8000-0000000000aa")
	txID     = uuid.MustParse("0190a000-0000-7000-8000-0000000000bb")
	instant  = time.Date(2026, 11, 16, 13, 30, 0, 0, time.FixedZone("BRT", -3*3600))
)

func brl(v int64) money.Money { return money.New(v, money.BRL) }

func validTx(entries ...Entry) Transaction {
	return Transaction{ID: txID, TenantID: tenantID, Kind: "teste", IdempotencyKey: "teste:1", Entries: entries}
}

// netByAccount soma as partidas por conta (débito positivo, crédito negativo).
func netByAccount(t *testing.T, tx Transaction) map[AccountCode]int64 {
	t.Helper()
	out := map[AccountCode]int64{}
	for _, e := range tx.Entries {
		switch e.Direction {
		case Debit:
			out[e.Account] += e.Amount.Amount()
		case Credit:
			out[e.Account] -= e.Amount.Amount()
		default:
			t.Fatalf("direção inesperada %q", e.Direction)
		}
	}
	return out
}

// assertBalanced verifica as invariantes de forma independente de Validate.
func assertBalanced(t *testing.T, tx Transaction) {
	t.Helper()
	if len(tx.Entries) < 2 {
		t.Fatalf("lançamento com %d partidas", len(tx.Entries))
	}
	var debits, credits int64
	for _, e := range tx.Entries {
		if !e.Amount.IsPositive() {
			t.Fatalf("partida com valor não positivo: %+v", e)
		}
		if e.Amount.Currency() != money.BRL {
			t.Fatalf("moeda inesperada: %v", e.Amount.Currency())
		}
		if _, ok := AccountKindOf(e.Account); !ok {
			t.Fatalf("conta fora do plano: %q", e.Account)
		}
		switch e.Direction {
		case Debit:
			debits += e.Amount.Amount()
		case Credit:
			credits += e.Amount.Amount()
		default:
			t.Fatalf("direção inválida: %q", e.Direction)
		}
	}
	if debits != credits {
		t.Fatalf("débitos %d diferem de créditos %d", debits, credits)
	}
	if err := tx.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	other := money.Currency{} // moeda distinta de BRL, apenas para testar o balanceamento por moeda
	cases := []struct {
		name    string
		tx      Transaction
		wantErr error
	}{
		{"lançamento balanceado é aceito", validTx(Entry{Cash, Debit, brl(100)}, Entry{AccountsReceivable, Credit, brl(100)}), nil},
		{"várias partidas balanceadas são aceitas", validTx(
			Entry{ProviderClearing, Debit, brl(1000)}, Entry{AccountsReceivable, Credit, brl(1000)},
			Entry{ProviderFees, Debit, brl(30)}, Entry{ProviderClearing, Credit, brl(30)}), nil},
		{"sem partidas", validTx(), ErrInvalidEntry},
		{"uma única partida", validTx(Entry{Cash, Debit, brl(100)}), ErrInvalidEntry},
		{"sem chave de idempotência", Transaction{TenantID: tenantID, Entries: []Entry{{Cash, Debit, brl(1)}, {AccountsReceivable, Credit, brl(1)}}}, ErrInvalidEntry},
		{"sem tenant", Transaction{IdempotencyKey: "k", Entries: []Entry{{Cash, Debit, brl(1)}, {AccountsReceivable, Credit, brl(1)}}}, ErrInvalidEntry},
		{"partida com valor zero", validTx(Entry{Cash, Debit, brl(0)}, Entry{AccountsReceivable, Credit, brl(0)}), ErrInvalidEntry},
		{"partida com valor negativo", validTx(Entry{Cash, Debit, brl(-100)}, Entry{AccountsReceivable, Credit, brl(-100)}), ErrInvalidEntry},
		{"conta desconhecida", validTx(Entry{"caixa_paralelo", Debit, brl(100)}, Entry{AccountsReceivable, Credit, brl(100)}), ErrInvalidEntry},
		{"direção inválida", validTx(Entry{Cash, "debito", brl(100)}, Entry{AccountsReceivable, Credit, brl(100)}), ErrInvalidEntry},
		{"débitos maiores que créditos", validTx(Entry{Cash, Debit, brl(101)}, Entry{AccountsReceivable, Credit, brl(100)}), ErrUnbalanced},
		{"somente débitos", validTx(Entry{Cash, Debit, brl(100)}, Entry{AccountsReceivable, Debit, brl(100)}), ErrUnbalanced},
		{"balanceado no total mas não por moeda", validTx(
			Entry{Cash, Debit, brl(100)}, Entry{AccountsReceivable, Credit, money.New(100, other)}), ErrUnbalanced},
		{"balanceado em cada moeda", validTx(
			Entry{Cash, Debit, brl(100)}, Entry{AccountsReceivable, Credit, brl(100)},
			Entry{Cash, Debit, money.New(7, other)}, Entry{AccountsReceivable, Credit, money.New(7, other)}), nil},
		{"soma que excede int64", validTx(
			Entry{Cash, Debit, brl(math.MaxInt64)}, Entry{Cash, Debit, brl(1)}, Entry{AccountsReceivable, Credit, brl(1)}), money.ErrOverflow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.tx.Validate()
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("esperava lançamento válido, obtive %v", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("esperava %v, obtive %v", c.wantErr, err)
			}
		})
	}
}

func TestReversalInvertsSidesAndReferencesOriginal(t *testing.T) {
	orig, ok, err := InvoiceIssued(txID, tenantID, refID, "FAT-2026-000001", brl(10000), brl(2500), instant)
	if err != nil || !ok {
		t.Fatalf("InvoiceIssued: ok=%v err=%v", ok, err)
	}
	revID := uuid.MustParse("0190a000-0000-7000-8000-0000000000cc")
	at := instant.Add(48 * time.Hour)
	rev := orig.Reversal(revID, at, "Cancelamento da fatura FAT-2026-000001")

	if rev.ID != revID || rev.ReversesID == nil || *rev.ReversesID != orig.ID {
		t.Fatalf("estorno deve referenciar o original: id=%v reverses=%v", rev.ID, rev.ReversesID)
	}
	if rev.TenantID != orig.TenantID || rev.ReferenceType != orig.ReferenceType || rev.ReferenceID != orig.ReferenceID {
		t.Fatalf("estorno deve manter tenant e referência: %+v", rev)
	}
	if rev.Kind != "invoice.issued.reversal" || rev.IdempotencyKey != "reversal:"+orig.IdempotencyKey {
		t.Fatalf("tipo/chave do estorno: %q %q", rev.Kind, rev.IdempotencyKey)
	}
	if !rev.OccurredAt.Equal(at) || rev.Description != "Cancelamento da fatura FAT-2026-000001" {
		t.Fatalf("instante/descrição do estorno: %v %q", rev.OccurredAt, rev.Description)
	}
	if len(rev.Entries) != len(orig.Entries) {
		t.Fatalf("estorno com %d partidas, original com %d", len(rev.Entries), len(orig.Entries))
	}
	for i := range orig.Entries {
		o, r := orig.Entries[i], rev.Entries[i]
		if o.Account != r.Account || o.Amount != r.Amount || o.Direction == r.Direction {
			t.Fatalf("partida %d não foi invertida: %+v -> %+v", i, o, r)
		}
	}
	assertBalanced(t, rev)

	// Original e estorno somados zeram todas as contas.
	combined := netByAccount(t, orig)
	for acc, v := range netByAccount(t, rev) {
		combined[acc] += v
	}
	for acc, v := range combined {
		if v != 0 {
			t.Fatalf("conta %s com saldo %d após estorno", acc, v)
		}
	}
	// O original não é alterado pelo estorno.
	if orig.Entries[0].Direction != Debit || orig.Entries[0].Account != AccountsReceivable {
		t.Fatalf("original alterado: %+v", orig.Entries[0])
	}
}

type constructorCase struct {
	name     string
	build    func() (Transaction, bool, error)
	kind     string
	keyPref  string
	refType  string
	expected map[AccountCode]int64 // saldo líquido por conta (débito positivo)
}

func constructorCases() []constructorCase {
	return []constructorCase{
		{"emissão de fatura sem crédito", func() (Transaction, bool, error) {
			return InvoiceIssued(txID, tenantID, refID, "FAT-1", brl(10000), brl(0), instant)
		}, "invoice.issued", "invoice.issued:", "invoice", map[AccountCode]int64{AccountsReceivable: 10000, BilledRevenue: -10000}},
		{"emissão de fatura com crédito aplicado", func() (Transaction, bool, error) {
			return InvoiceIssued(txID, tenantID, refID, "FAT-1", brl(10000), brl(3000), instant)
		}, "invoice.issued", "invoice.issued:", "invoice", map[AccountCode]int64{AccountsReceivable: 7000, BilledRevenue: -10000, CustomerCredits: 3000}},
		{"emissão de fatura coberta integralmente por crédito", func() (Transaction, bool, error) {
			return InvoiceIssued(txID, tenantID, refID, "FAT-1", brl(5000), brl(5000), instant)
		}, "invoice.issued", "invoice.issued:", "invoice", map[AccountCode]int64{BilledRevenue: -5000, CustomerCredits: 5000}},
		{"pagamento no provedor com taxa", func() (Transaction, bool, error) {
			return ProviderPaymentReceived(txID, tenantID, refID, brl(10000), brl(199), instant)
		}, "payment.confirmed", "payment.confirmed:", "payment", map[AccountCode]int64{ProviderClearing: 9801, AccountsReceivable: -10000, ProviderFees: 199}},
		{"pagamento no provedor sem taxa", func() (Transaction, bool, error) {
			return ProviderPaymentReceived(txID, tenantID, refID, brl(10000), brl(0), instant)
		}, "payment.confirmed", "payment.confirmed:", "payment", map[AccountCode]int64{ProviderClearing: 10000, AccountsReceivable: -10000}},
		{"pagamento manual", func() (Transaction, bool, error) {
			return ManualPaymentReceived(txID, tenantID, refID, brl(4200), instant)
		}, "payment.manual", "payment.manual:", "payment", map[AccountCode]int64{Cash: 4200, AccountsReceivable: -4200}},
		{"excedente recebido no provedor", func() (Transaction, bool, error) {
			return Overpayment(txID, tenantID, refID, brl(500), true, instant)
		}, "payment.overpayment", "payment.overpayment:", "payment", map[AccountCode]int64{ProviderClearing: 500, CustomerCredits: -500}},
		{"excedente recebido manualmente", func() (Transaction, bool, error) {
			return Overpayment(txID, tenantID, refID, brl(500), false, instant)
		}, "payment.overpayment", "payment.overpayment:", "payment", map[AccountCode]int64{Cash: 500, CustomerCredits: -500}},
		{"reembolso efetivado", func() (Transaction, bool, error) {
			return RefundSucceeded(txID, tenantID, refID, brl(1500), brl(0), instant)
		}, "refund.succeeded", "refund.succeeded:", "refund", map[AccountCode]int64{Refunds: 1500, ProviderClearing: -1500}},
		{"reembolso com parte do crédito do cliente", func() (Transaction, bool, error) {
			return RefundSucceeded(txID, tenantID, refID, brl(1500), brl(400), instant)
		}, "refund.succeeded", "refund.succeeded:", "refund", map[AccountCode]int64{Refunds: 1100, CustomerCredits: 400, ProviderClearing: -1500}},
		{"reembolso integral de crédito do cliente", func() (Transaction, bool, error) {
			return RefundSucceeded(txID, tenantID, refID, brl(1500), money.Money{}, instant)
		}, "refund.succeeded", "refund.succeeded:", "refund", map[AccountCode]int64{Refunds: 1500, ProviderClearing: -1500}},
		{"chargeback aberto", func() (Transaction, bool, error) {
			return ChargebackOpened(txID, tenantID, refID, brl(800), instant)
		}, "dispute.opened", "dispute.opened:", "dispute", map[AccountCode]int64{Chargebacks: 800, ProviderClearing: -800}},
		{"retenção de chargeback liberada", func() (Transaction, bool, error) {
			return ChargebackHoldReleased(txID, tenantID, refID, brl(300), instant)
		}, "dispute.hold_released", "dispute.hold_released:", "dispute", map[AccountCode]int64{ProviderClearing: 300, Chargebacks: -300}},
		{"chargeback de reembolso recusado", func() (Transaction, bool, error) {
			return ChargebackHoldRestored(txID, tenantID, refID, brl(300), instant)
		}, "dispute.hold_restored", "dispute.hold_restored:", "refund", map[AccountCode]int64{Chargebacks: 300, ProviderClearing: -300}},
		{"baixa como incobrável", func() (Transaction, bool, error) {
			return InvoiceWrittenOff(txID, tenantID, refID, brl(6000), instant)
		}, "invoice.uncollectible", "invoice.uncollectible:", "invoice", map[AccountCode]int64{BadDebt: 6000, AccountsReceivable: -6000}},
		{"nota de crédito", func() (Transaction, bool, error) {
			return CreditNoteIssued(txID, tenantID, refID, brl(700), instant)
		}, "credit_note.issued", "credit_note.issued:", "credit_note", map[AccountCode]int64{RevenueAdjustments: 700, AccountsReceivable: -700}},
		{"crédito concedido ao cliente", func() (Transaction, bool, error) {
			return CustomerCreditGranted(txID, tenantID, refID, brl(2500), instant)
		}, "customer_credit.granted", "customer_credit.granted:", "customer_credit", map[AccountCode]int64{RevenueAdjustments: 2500, CustomerCredits: -2500}},
		{"liquidação recebida", func() (Transaction, bool, error) {
			return SettlementReceived(txID, tenantID, refID, brl(9801), instant)
		}, "settlement.received", "settlement.received:", "settlement", map[AccountCode]int64{Cash: 9801, ProviderClearing: -9801}},
		{"ajuste de conciliação positivo", func() (Transaction, bool, error) {
			return ReconciliationAdjustment(txID, tenantID, refID, brl(35), instant)
		}, "reconciliation.adjustment", "reconciliation.adjustment:", "reconciliation_discrepancy", map[AccountCode]int64{ReconciliationAdjustments: 35, ProviderClearing: -35}},
		{"ajuste de conciliação negativo", func() (Transaction, bool, error) {
			return ReconciliationAdjustment(txID, tenantID, refID, brl(-35), instant)
		}, "reconciliation.adjustment", "reconciliation.adjustment:", "reconciliation_discrepancy", map[AccountCode]int64{ProviderClearing: 35, ReconciliationAdjustments: -35}},
	}
}

func TestConstructorsProduceBalancedTransactions(t *testing.T) {
	for _, c := range constructorCases() {
		t.Run(c.name, func(t *testing.T) {
			tx, ok, err := c.build()
			if err != nil || !ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			assertBalanced(t, tx)
			if tx.Kind != c.kind || tx.ReferenceType != c.refType || tx.ReferenceID != refID {
				t.Fatalf("tipo/referência: %q %q %v", tx.Kind, tx.ReferenceType, tx.ReferenceID)
			}
			if tx.IdempotencyKey != c.keyPref+refID.String() {
				t.Fatalf("chave de idempotência %q", tx.IdempotencyKey)
			}
			if tx.ID != txID || tx.TenantID != tenantID || tx.ReversesID != nil {
				t.Fatalf("identificação: %+v", tx)
			}
			if tx.OccurredAt.Location() != time.UTC || !tx.OccurredAt.Equal(instant) {
				t.Fatalf("OccurredAt deve ser o mesmo instante em UTC: %v", tx.OccurredAt)
			}
			if tx.Description == "" {
				t.Fatal("descrição vazia")
			}
			got := netByAccount(t, tx)
			for acc, want := range c.expected {
				if got[acc] != want {
					t.Errorf("saldo líquido de %s = %d, esperado %d", acc, got[acc], want)
				}
			}
			for acc, v := range got {
				if _, listed := c.expected[acc]; !listed && v != 0 {
					t.Errorf("conta inesperada %s com saldo %d", acc, v)
				}
			}
		})
	}
}

func TestInvoiceIssuedDescriptionContainsNumber(t *testing.T) {
	tx, _, err := InvoiceIssued(txID, tenantID, refID, "FAT-2026-000042", brl(1), brl(0), instant)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tx.Description, "FAT-2026-000042") {
		t.Fatalf("descrição sem o número da fatura: %q", tx.Description)
	}
}

func TestConstructorsWithZeroAmountsProduceNothing(t *testing.T) {
	z := brl(0)
	cases := map[string]func() (Transaction, bool, error){
		"emissão": func() (Transaction, bool, error) { return InvoiceIssued(txID, tenantID, refID, "F", z, z, instant) },
		"pagamento no provedor": func() (Transaction, bool, error) {
			return ProviderPaymentReceived(txID, tenantID, refID, z, z, instant)
		},
		"pagamento manual":  func() (Transaction, bool, error) { return ManualPaymentReceived(txID, tenantID, refID, z, instant) },
		"excedente":         func() (Transaction, bool, error) { return Overpayment(txID, tenantID, refID, z, true, instant) },
		"reembolso":         func() (Transaction, bool, error) { return RefundSucceeded(txID, tenantID, refID, z, z, instant) },
		"chargeback":        func() (Transaction, bool, error) { return ChargebackOpened(txID, tenantID, refID, z, instant) },
		"retenção liberada": func() (Transaction, bool, error) { return ChargebackHoldReleased(txID, tenantID, refID, z, instant) },
		"retenção restaurada": func() (Transaction, bool, error) {
			return ChargebackHoldRestored(txID, tenantID, refID, z, instant)
		},
		"baixa":             func() (Transaction, bool, error) { return InvoiceWrittenOff(txID, tenantID, refID, z, instant) },
		"nota de crédito":   func() (Transaction, bool, error) { return CreditNoteIssued(txID, tenantID, refID, z, instant) },
		"crédito concedido": func() (Transaction, bool, error) { return CustomerCreditGranted(txID, tenantID, refID, z, instant) },
		"liquidação":        func() (Transaction, bool, error) { return SettlementReceived(txID, tenantID, refID, z, instant) },
		"ajuste de conciliação": func() (Transaction, bool, error) {
			return ReconciliationAdjustment(txID, tenantID, refID, z, instant)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			tx, ok, err := build()
			if err != nil || ok {
				t.Fatalf("valor zero não deve gerar lançamento: ok=%v err=%v", ok, err)
			}
			if len(tx.Entries) != 0 {
				t.Fatalf("lançamento vazio esperado, obtive %+v", tx)
			}
		})
	}
}

func TestProviderPaymentWithOnlyFeeKeepsFeePair(t *testing.T) {
	tx, ok, err := ProviderPaymentReceived(txID, tenantID, refID, brl(0), brl(50), instant)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	assertBalanced(t, tx)
	if len(tx.Entries) != 2 {
		t.Fatalf("somente o par da taxa deveria permanecer: %+v", tx.Entries)
	}
}

func TestConstructorsRejectInvalidAmounts(t *testing.T) {
	t.Run("valor negativo não gera lançamento", func(t *testing.T) {
		_, ok, err := ManualPaymentReceived(txID, tenantID, refID, brl(-100), instant)
		if ok || !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("esperava ErrInvalidEntry, obtive ok=%v err=%v", ok, err)
		}
	})
	t.Run("tenant ausente é rejeitado", func(t *testing.T) {
		_, ok, err := ManualPaymentReceived(txID, uuid.Nil, refID, brl(100), instant)
		if ok || !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("esperava ErrInvalidEntry, obtive ok=%v err=%v", ok, err)
		}
	})
	t.Run("parte do crédito maior que o reembolso", func(t *testing.T) {
		_, ok, err := RefundSucceeded(txID, tenantID, refID, brl(100), brl(101), instant)
		if ok || !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("esperava ErrInvalidEntry, obtive ok=%v err=%v", ok, err)
		}
		_, ok, err = RefundSucceeded(txID, tenantID, refID, brl(100), brl(-1), instant)
		if ok || !errors.Is(err, ErrInvalidEntry) {
			t.Fatalf("parte negativa: ok=%v err=%v", ok, err)
		}
		_, ok, err = RefundSucceeded(txID, tenantID, refID, brl(100), money.New(1, money.Currency{}), instant)
		if ok || !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("moedas diferentes: ok=%v err=%v", ok, err)
		}
	})
	t.Run("ajuste negativo sem valor absoluto representável", func(t *testing.T) {
		_, ok, err := ReconciliationAdjustment(txID, tenantID, refID, brl(math.MinInt64), instant)
		if ok || !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("esperava overflow, obtive ok=%v err=%v", ok, err)
		}
	})
}

func TestNaturalBalance(t *testing.T) {
	cases := []struct {
		kind            AccountKind
		debits, credits int64
		want            int64
	}{
		{Asset, 1000, 300, 700},
		{Expense, 250, 0, 250},
		{ContraRevenue, 400, 100, 300},
		{Liability, 300, 1000, 700},
		{Revenue, 0, 10000, 10000},
		{Asset, 100, 400, -300}, // ativo com saldo contrário à natureza
	}
	for _, c := range cases {
		got, err := NaturalBalance(c.kind, brl(c.debits), brl(c.credits))
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		if got.Amount() != c.want {
			t.Errorf("%s D=%d C=%d: saldo %d, esperado %d", c.kind, c.debits, c.credits, got.Amount(), c.want)
		}
	}
	if _, err := NaturalBalance(Asset, brl(1), money.New(1, money.Currency{})); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("moedas diferentes devem falhar: %v", err)
	}
}

func TestDebitNormal(t *testing.T) {
	want := map[AccountKind]bool{Asset: true, Expense: true, ContraRevenue: true, Liability: false, Revenue: false}
	for k, w := range want {
		if k.DebitNormal() != w {
			t.Errorf("%s.DebitNormal()=%v", k, !w)
		}
	}
}

func TestChartAccountsAreUniqueAndResolvable(t *testing.T) {
	seen := map[AccountCode]bool{}
	for _, a := range Chart {
		if seen[a.Code] {
			t.Fatalf("conta duplicada no plano: %s", a.Code)
		}
		seen[a.Code] = true
		kind, ok := AccountKindOf(a.Code)
		if !ok || kind != a.Kind {
			t.Fatalf("AccountKindOf(%s)=%v,%v; esperado %v", a.Code, kind, ok, a.Kind)
		}
		if a.Name == "" {
			t.Fatalf("conta %s sem nome", a.Code)
		}
	}
	if _, ok := AccountKindOf("inexistente"); ok {
		t.Fatal("conta inexistente não deve ser resolvida")
	}
	if kind, _ := AccountKindOf(CustomerCredits); kind != Liability {
		t.Fatalf("créditos de clientes devem ser passivo, obtive %s", kind)
	}
}

// nonNegative reduz um valor arbitrário ao intervalo [0, MaxAmount].
func nonNegative(v int64) int64 {
	return int64(uint64(v) % uint64(money.MaxAmount+1))
}

// FuzzLedgerBalanced garante que, para valores não negativos arbitrários, os
// construtores nunca produzem lançamento desbalanceado nem partida com valor
// menor ou igual a zero.
func FuzzLedgerBalanced(f *testing.F) {
	f.Add(int64(10000), int64(3000), true)
	f.Add(int64(0), int64(0), false)
	f.Add(int64(1), money.MaxAmount, true)
	f.Add(money.MaxAmount, int64(1), false)
	f.Add(int64(-1), int64(math.MinInt64), true)
	f.Fuzz(func(t *testing.T, rawA, rawB int64, flag bool) {
		a, b := nonNegative(rawA), nonNegative(rawB)
		builders := map[string]func() (Transaction, bool, error){
			"InvoiceIssued": func() (Transaction, bool, error) {
				return InvoiceIssued(txID, tenantID, refID, "F", brl(a), brl(b), instant)
			},
			"ProviderPaymentReceived": func() (Transaction, bool, error) {
				return ProviderPaymentReceived(txID, tenantID, refID, brl(a), brl(b), instant)
			},
			"ManualPaymentReceived": func() (Transaction, bool, error) {
				return ManualPaymentReceived(txID, tenantID, refID, brl(a), instant)
			},
			"Overpayment": func() (Transaction, bool, error) { return Overpayment(txID, tenantID, refID, brl(a), flag, instant) },
			"RefundSucceeded": func() (Transaction, bool, error) {
				return RefundSucceeded(txID, tenantID, refID, brl(a), brl(a/3), instant)
			},
			"ChargebackOpened": func() (Transaction, bool, error) { return ChargebackOpened(txID, tenantID, refID, brl(a), instant) },
			"ChargebackHoldReleased": func() (Transaction, bool, error) {
				return ChargebackHoldReleased(txID, tenantID, refID, brl(a), instant)
			},
			"ChargebackHoldRestored": func() (Transaction, bool, error) {
				return ChargebackHoldRestored(txID, tenantID, refID, brl(a), instant)
			},
			"InvoiceWrittenOff": func() (Transaction, bool, error) { return InvoiceWrittenOff(txID, tenantID, refID, brl(a), instant) },
			"CreditNoteIssued":  func() (Transaction, bool, error) { return CreditNoteIssued(txID, tenantID, refID, brl(a), instant) },
			"CustomerCreditGranted": func() (Transaction, bool, error) {
				return CustomerCreditGranted(txID, tenantID, refID, brl(a), instant)
			},
			"SettlementReceived": func() (Transaction, bool, error) { return SettlementReceived(txID, tenantID, refID, brl(a), instant) },
			"ReconciliationAdjustment": func() (Transaction, bool, error) {
				v := a
				if flag {
					v = -a
				}
				return ReconciliationAdjustment(txID, tenantID, refID, brl(v), instant)
			},
		}
		anyNonZero := map[string]bool{"InvoiceIssued": a != 0 || b != 0, "ProviderPaymentReceived": a != 0 || b != 0}
		for name, build := range builders {
			tx, ok, err := build()
			if err != nil {
				t.Fatalf("%s(a=%d, b=%d): erro inesperado %v", name, a, b, err)
			}
			expectOK := a != 0
			if v, two := anyNonZero[name]; two {
				expectOK = v
			}
			if ok != expectOK {
				t.Fatalf("%s(a=%d, b=%d): ok=%v, esperado %v", name, a, b, ok, expectOK)
			}
			if !ok {
				continue
			}
			assertBalanced(t, tx)
			assertBalanced(t, tx.Reversal(refID, instant, "estorno"))
		}
	})
}
