// Package ledger implementa o ledger operacional imutável de partidas
// dobradas do tenant. Cada transação tem ao menos duas partidas e soma zero
// em cada moeda (débitos = créditos). Correções usam transações de estorno;
// nenhum lançamento é alterado ou removido.
//
// O ledger registra a movimentação operacional (faturamento, recebíveis,
// confirmação e liquidação no provedor, taxas, reembolsos, chargebacks). Ele
// não substitui a contabilidade fiscal nem o reconhecimento contábil de
// receita.
//
// Ciclo do pagamento pelo provedor (ADR-080): a confirmação leva o valor
// bruto de contas a receber para pagamentos confirmados no provedor
// (payment.confirmed); o recebimento informado pelo provedor transfere o
// bruto para os valores em liquidação e reconhece a taxa (payment.received);
// a liquidação (settlement.*) leva o líquido ao disponível. Toda
// movimentação posterior com o provedor (reembolso, retenção e liberação de
// chargeback, ajuste de conciliação) passa pelos valores em liquidação e é
// liquidada no disponível da mesma forma (Settlement).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// AccountCode identifica uma conta do plano operacional.
type AccountCode string

// Contas do plano operacional.
const (
	AccountsReceivable AccountCode = "accounts_receivable"
	BilledRevenue      AccountCode = "billed_revenue"
	// ProviderConfirmed reúne os pagamentos confirmados pelo provedor e
	// ainda não recebidos (valor bruto a receber do provedor): sai da conta
	// quando o provedor informa o recebimento.
	ProviderConfirmed AccountCode = "provider_confirmed"
	// ProviderClearing é a conta de passagem das movimentações com o
	// provedor: recebe o bruto recebido, paga taxas, reembolsos, retenções de
	// chargeback e ajustes, e é liquidada no disponível (Cash).
	ProviderClearing          AccountCode = "provider_clearing"
	Cash                      AccountCode = "cash"
	ProviderFees              AccountCode = "provider_fees"
	Refunds                   AccountCode = "refunds"
	Chargebacks               AccountCode = "chargebacks"
	CustomerCredits           AccountCode = "customer_credits"
	RevenueAdjustments        AccountCode = "revenue_adjustments"
	BadDebt                   AccountCode = "bad_debt"
	ReconciliationAdjustments AccountCode = "reconciliation_adjustments"
	// CustomerRecoverables reúne os valores a recuperar de clientes: crédito
	// de pagamento excedente já usado quando o pagamento que o gerou foi
	// estornado por chargeback. Sai da conta pela baixa: recebida (disponível
	// liquidado) ou perdida (perdas com inadimplência).
	CustomerRecoverables AccountCode = "customer_recoverables"
)

// AccountKind define a natureza da conta.
type AccountKind string

const (
	Asset         AccountKind = "asset"
	Liability     AccountKind = "liability"
	Revenue       AccountKind = "revenue"
	ContraRevenue AccountKind = "contra_revenue"
	Expense       AccountKind = "expense"
)

// DebitNormal informa se o saldo natural da conta é devedor.
func (k AccountKind) DebitNormal() bool { return k == Asset || k == Expense || k == ContraRevenue }

// AccountDef descreve uma conta do plano.
type AccountDef struct {
	Code AccountCode
	Name string
	Kind AccountKind
}

// Chart é o plano de contas operacional criado para cada tenant.
var Chart = []AccountDef{
	{AccountsReceivable, "Contas a receber", Asset},
	{BilledRevenue, "Receita faturada", Revenue},
	{ProviderConfirmed, "Pagamentos confirmados no provedor", Asset},
	{ProviderClearing, "Valores em liquidação no provedor", Asset},
	{Cash, "Disponível liquidado", Asset},
	{ProviderFees, "Taxas do provedor", Expense},
	{Refunds, "Reembolsos", ContraRevenue},
	{Chargebacks, "Chargebacks", ContraRevenue},
	{CustomerCredits, "Créditos de clientes", Liability},
	{RevenueAdjustments, "Ajustes de faturamento", ContraRevenue},
	{BadDebt, "Perdas com inadimplência", Expense},
	{ReconciliationAdjustments, "Ajustes de conciliação", Expense},
	{CustomerRecoverables, "Valores a recuperar de clientes", Asset},
}

// AccountKindOf retorna a natureza de uma conta do plano.
func AccountKindOf(code AccountCode) (AccountKind, bool) {
	for _, a := range Chart {
		if a.Code == code {
			return a.Kind, true
		}
	}
	return "", false
}

// Direction é o lado da partida.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Entry é uma partida.
type Entry struct {
	Account   AccountCode
	Direction Direction
	Amount    money.Money
}

// Transaction é um lançamento balanceado.
type Transaction struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Kind           string
	Description    string
	OccurredAt     time.Time
	ReferenceType  string
	ReferenceID    uuid.UUID
	IdempotencyKey string
	ReversesID     *uuid.UUID
	Entries        []Entry
}

// ErrUnbalanced indica lançamento cuja soma de débitos difere da de créditos.
var ErrUnbalanced = errors.New("ledger: lançamento desbalanceado")

// ErrInvalidEntry indica partida inválida.
var ErrInvalidEntry = errors.New("ledger: partida inválida")

// Validate verifica as invariantes: ao menos duas partidas, valores
// positivos, contas conhecidas e balanceamento por moeda.
func (t Transaction) Validate() error {
	if len(t.Entries) < 2 || t.IdempotencyKey == "" || t.TenantID == uuid.Nil {
		return ErrInvalidEntry
	}
	balance := map[money.Currency]money.Money{}
	for _, e := range t.Entries {
		if !e.Amount.IsPositive() {
			return fmt.Errorf("%w: valor deve ser positivo", ErrInvalidEntry)
		}
		if _, ok := AccountKindOf(e.Account); !ok {
			return fmt.Errorf("%w: conta %q", ErrInvalidEntry, e.Account)
		}
		cur := e.Amount.Currency()
		b, ok := balance[cur]
		if !ok {
			b = money.Zero(cur)
		}
		var err error
		switch e.Direction {
		case Debit:
			b, err = b.Add(e.Amount)
		case Credit:
			b, err = b.Sub(e.Amount)
		default:
			return fmt.Errorf("%w: direção %q", ErrInvalidEntry, e.Direction)
		}
		if err != nil {
			return err
		}
		balance[cur] = b
	}
	for cur, b := range balance {
		if !b.IsZero() {
			return fmt.Errorf("%w em %s", ErrUnbalanced, cur)
		}
	}
	return nil
}

// Reversal cria o estorno da transação (partidas com lados invertidos).
func (t Transaction) Reversal(id uuid.UUID, at time.Time, description string) Transaction {
	entries := make([]Entry, len(t.Entries))
	for i, e := range t.Entries {
		d := Debit
		if e.Direction == Debit {
			d = Credit
		}
		entries[i] = Entry{Account: e.Account, Direction: d, Amount: e.Amount}
	}
	orig := t.ID
	return Transaction{
		ID: id, TenantID: t.TenantID, Kind: t.Kind + ".reversal", Description: description, OccurredAt: at,
		ReferenceType: t.ReferenceType, ReferenceID: t.ReferenceID, IdempotencyKey: "reversal:" + t.IdempotencyKey,
		ReversesID: &orig, Entries: entries,
	}
}

func pair(debit, credit AccountCode, amount money.Money) []Entry {
	return []Entry{{debit, Debit, amount}, {credit, Credit, amount}}
}

func build(id, tenant uuid.UUID, kind, desc string, at time.Time, refType string, refID uuid.UUID, key string, entries []Entry) (Transaction, bool, error) {
	var nonZero []Entry
	for _, e := range entries {
		if !e.Amount.IsZero() {
			nonZero = append(nonZero, e)
		}
	}
	if len(nonZero) == 0 {
		return Transaction{}, false, nil
	}
	t := Transaction{ID: id, TenantID: tenant, Kind: kind, Description: desc, OccurredAt: at.UTC(),
		ReferenceType: refType, ReferenceID: refID, IdempotencyKey: key, Entries: nonZero}
	if err := t.Validate(); err != nil {
		return Transaction{}, false, err
	}
	return t, true, nil
}

// InvoiceIssued: faturamento (D contas a receber / C receita faturada) e,
// se houver, aplicação de crédito do cliente (D créditos / C contas a receber).
func InvoiceIssued(id, tenant, invoiceID uuid.UUID, number string, total, creditApplied money.Money, at time.Time) (Transaction, bool, error) {
	entries := pair(AccountsReceivable, BilledRevenue, total)
	entries = append(entries, pair(CustomerCredits, AccountsReceivable, creditApplied)...)
	return build(id, tenant, "invoice.issued", "Emissão da fatura "+number, at, "invoice", invoiceID, "invoice.issued:"+invoiceID.String(), entries)
}

// ProviderPaymentConfirmed: confirmação de pagamento pelo provedor. O valor
// aplicado à fatura sai de contas a receber para pagamentos confirmados no
// provedor, onde fica até o recebimento (ProviderPaymentReceived). A taxa só é
// reconhecida no recebimento, quando o provedor a retém do valor creditado.
func ProviderPaymentConfirmed(id, tenant, paymentID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "payment.confirmed", "Pagamento confirmado no provedor", at, "payment", paymentID,
		"payment.confirmed:"+paymentID.String(), pair(ProviderConfirmed, AccountsReceivable, amount))
}

// ProviderPaymentReceived: recebimento informado pelo provedor. O valor bruto
// confirmado (gross: aplicado à fatura mais o excedente convertido em
// crédito) passa de pagamentos confirmados para os valores em liquidação, e a
// taxa retida pelo provedor sai dos valores em liquidação para a despesa. O
// líquido é levado ao disponível pela liquidação (Settlement).
func ProviderPaymentReceived(id, tenant, paymentID uuid.UUID, gross, fee money.Money, at time.Time) (Transaction, bool, error) {
	entries := pair(ProviderClearing, ProviderConfirmed, gross)
	entries = append(entries, pair(ProviderFees, ProviderClearing, fee)...)
	return build(id, tenant, "payment.received", "Pagamento recebido pelo provedor", at, "payment", paymentID,
		"payment.received:"+paymentID.String(), entries)
}

// ManualPaymentReceived: recebimento registrado manualmente.
func ManualPaymentReceived(id, tenant, paymentID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "payment.manual", "Recebimento registrado manualmente", at, "payment", paymentID,
		"payment.manual:"+paymentID.String(), pair(Cash, AccountsReceivable, amount))
}

// Overpayment: valor recebido além do saldo vira crédito do cliente. Pelo
// provedor, o excedente também fica em pagamentos confirmados até o
// recebimento.
func Overpayment(id, tenant, paymentID uuid.UUID, amount money.Money, viaProvider bool, at time.Time) (Transaction, bool, error) {
	debit := Cash
	if viaProvider {
		debit = ProviderConfirmed
	}
	return build(id, tenant, "payment.overpayment", "Pagamento excedente convertido em crédito", at, "payment", paymentID,
		"payment.overpayment:"+paymentID.String(), pair(debit, CustomerCredits, amount))
}

// RefundSucceeded: reembolso efetivado pelo provedor. O valor sai dos
// valores em liquidação; fromCredit é a parte que devolve crédito do cliente
// gerado por pagamento excedente (reduz a obrigação com o cliente) e o
// restante reduz a receita (conta de reembolsos).
func RefundSucceeded(id, tenant, refundID uuid.UUID, amount, fromCredit money.Money, at time.Time) (Transaction, bool, error) {
	if fromCredit == (money.Money{}) {
		fromCredit = money.Zero(amount.Currency())
	}
	revenue, err := amount.Sub(fromCredit)
	if err != nil {
		return Transaction{}, false, err
	}
	if revenue.IsNegative() || fromCredit.IsNegative() {
		return Transaction{}, false, fmt.Errorf("%w: parte do crédito maior que o reembolso", ErrInvalidEntry)
	}
	entries := pair(Refunds, ProviderClearing, revenue)
	entries = append(entries, pair(CustomerCredits, ProviderClearing, fromCredit)...)
	return build(id, tenant, "refund.succeeded", "Reembolso efetivado", at, "refund", refundID,
		"refund.succeeded:"+refundID.String(), entries)
}

// ChargebackOpened: valor contestado retido pelo provedor.
func ChargebackOpened(id, tenant, disputeID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.opened", "Chargeback aberto", at, "dispute", disputeID,
		"dispute.opened:"+disputeID.String(), pair(Chargebacks, ProviderClearing, amount))
}

// ChargebackHoldReleased: na disputa perdida, a parte da retenção que não
// foi estornada ao pagador por já ter sido devolvida por reembolso
// (confirmado ou ainda reservado) volta aos valores em liquidação. Junto com
// dispute.opened, deixa em chargebacks exatamente o valor estornado.
func ChargebackHoldReleased(id, tenant, disputeID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.hold_released", "Retenção de chargeback liberada: valor já reembolsado ao pagador", at, "dispute",
		disputeID, "dispute.hold_released:"+disputeID.String(), pair(ProviderClearing, Chargebacks, amount))
}

// ChargebackHoldRestored: reembolso reservado recusado depois da disputa
// perdida. O valor, liberado da retenção na perda por estar reservado ao
// pagador, passa ao chargeback (um lançamento por reembolso).
func ChargebackHoldRestored(id, tenant, refundID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.hold_restored", "Chargeback do valor de reembolso recusado depois da disputa perdida", at, "refund",
		refundID, "dispute.hold_restored:"+refundID.String(), pair(Chargebacks, ProviderClearing, amount))
}

// ChargebackCreditReversed: parte do chargeback que corresponde ao excedente
// do pagamento convertido em crédito do cliente. O valor sai de chargebacks
// (não é receita perdida): consumed reduz a obrigação com o cliente (crédito
// ainda disponível, consumido) e recovery é o crédito que o cliente já havia
// usado e passa a ser valor a recuperar dele. refType e refID identificam a
// origem: a disputa perdida ou o reembolso recusado depois dela, cujo valor
// passou ao chargeback (um lançamento por origem).
func ChargebackCreditReversed(id, tenant uuid.UUID, refType string, refID uuid.UUID, consumed, recovery money.Money, at time.Time) (Transaction, bool, error) {
	if consumed.IsNegative() || recovery.IsNegative() {
		return Transaction{}, false, fmt.Errorf("%w: parte do crédito negativa", ErrInvalidEntry)
	}
	entries := pair(CustomerCredits, Chargebacks, consumed)
	entries = append(entries, pair(CustomerRecoverables, Chargebacks, recovery)...)
	return build(id, tenant, "dispute.credit_reversed", "Chargeback de pagamento excedente: crédito do cliente estornado", at, refType, refID,
		"dispute.credit_reversed:"+refType+":"+refID.String(), entries)
}

// RecoverySettlementReference é o tipo de referência dos lançamentos de baixa
// do valor a recuperar de cliente (um lançamento por baixa).
const RecoverySettlementReference = "dispute_recovery_settlement"

// RecoveryReceived: baixa do valor a recuperar de cliente recebido fora do
// provedor (transferência, acerto comercial). O valor entra no disponível
// liquidado, como o recebimento manual, e sai dos valores a recuperar.
func RecoveryReceived(id, tenant, settlementID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.recovery_received", "Valor a recuperar de cliente recebido", at, RecoverySettlementReference, settlementID,
		"dispute.recovery_received:"+settlementID.String(), pair(Cash, CustomerRecoverables, amount))
}

// RecoveryWrittenOff: baixa do valor a recuperar de cliente de que o tenant
// desistiu. O valor é reconhecido como perda, na mesma conta das faturas
// incobráveis, e sai dos valores a recuperar.
func RecoveryWrittenOff(id, tenant, settlementID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.recovery_written_off", "Valor a recuperar de cliente baixado como perda", at, RecoverySettlementReference,
		settlementID, "dispute.recovery_written_off:"+settlementID.String(), pair(BadDebt, CustomerRecoverables, amount))
}

// InvoiceWrittenOff: saldo considerado incobrável.
func InvoiceWrittenOff(id, tenant, invoiceID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "invoice.uncollectible", "Saldo considerado incobrável", at, "invoice", invoiceID,
		"invoice.uncollectible:"+invoiceID.String(), pair(BadDebt, AccountsReceivable, amount))
}

// BadDebtRecovered: recebimento de valor antes baixado como incobrável
// estorna a perda na mesma medida.
func BadDebtRecovered(id, tenant, paymentID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "invoice.bad_debt_recovered", "Recuperação de valor baixado como incobrável", at, "payment", paymentID,
		"invoice.bad_debt_recovered:"+paymentID.String(), pair(AccountsReceivable, BadDebt, amount))
}

// CreditNoteIssued: nota de crédito que reduz o saldo da fatura.
func CreditNoteIssued(id, tenant, creditNoteID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "credit_note.issued", "Nota de crédito", at, "credit_note", creditNoteID,
		"credit_note.issued:"+creditNoteID.String(), pair(RevenueAdjustments, AccountsReceivable, amount))
}

// CustomerCreditGranted: crédito concedido ao cliente para faturas futuras.
func CustomerCreditGranted(id, tenant, creditID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "customer_credit.granted", "Crédito concedido ao cliente", at, "customer_credit", creditID,
		"customer_credit.granted:"+creditID.String(), pair(RevenueAdjustments, CustomerCredits, amount))
}

// Settlement: liquidação no disponível da movimentação t com o provedor. O
// efeito líquido de t nos valores em liquidação é transferido para o
// disponível liquidado: débito líquido (recebimento, liberação de retenção,
// ajuste a favor) vira crédito do provedor no disponível (settlement.received);
// crédito líquido (taxa, reembolso, retenção de chargeback, ajuste contra)
// vira débito do provedor no disponível (settlement.debited). A chave deriva
// da chave de t, o que torna a liquidação idempotente como o lançamento que a
// originou. ok=false quando t não movimenta os valores em liquidação.
func Settlement(id uuid.UUID, t Transaction) (Transaction, bool, error) {
	var net money.Money
	seen := false
	for _, e := range t.Entries {
		if e.Account != ProviderClearing {
			continue
		}
		if !seen {
			net, seen = money.Zero(e.Amount.Currency()), true
		}
		var err error
		if e.Direction == Debit {
			net, err = net.Add(e.Amount)
		} else {
			net, err = net.Sub(e.Amount)
		}
		if err != nil {
			return Transaction{}, false, fmt.Errorf("%w: %w", ErrInvalidEntry, err)
		}
	}
	if !seen || net.IsZero() {
		return Transaction{}, false, nil
	}
	kind, desc, entries := "settlement.received", "Liquidação no disponível: crédito do provedor", pair(Cash, ProviderClearing, net)
	if net.IsNegative() {
		abs, err := net.Abs()
		if err != nil {
			return Transaction{}, false, err
		}
		kind, desc, entries = "settlement.debited", "Liquidação no disponível: débito do provedor", pair(ProviderClearing, Cash, abs)
	}
	return build(id, t.TenantID, kind, desc, t.OccurredAt, t.ReferenceType, t.ReferenceID, "settlement:"+t.IdempotencyKey, entries)
}

// ReconciliationAdjustment: ajuste decidido na conciliação. Valor positivo
// reconhece despesa contra valores em liquidação; negativo reverte.
func ReconciliationAdjustment(id, tenant, discrepancyID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	entries := pair(ReconciliationAdjustments, ProviderClearing, amount)
	if amount.IsNegative() {
		abs, err := amount.Abs()
		if err != nil {
			return Transaction{}, false, err
		}
		entries = pair(ProviderClearing, ReconciliationAdjustments, abs)
	}
	return build(id, tenant, "reconciliation.adjustment", "Ajuste de conciliação", at, "reconciliation_discrepancy", discrepancyID,
		"reconciliation.adjustment:"+discrepancyID.String(), entries)
}

// AccountBalance é o saldo de uma conta.
type AccountBalance struct {
	Code    AccountCode
	Name    string
	Kind    AccountKind
	Debits  money.Money
	Credits money.Money
	// Balance usa o sinal natural da conta (positivo = saldo na natureza).
	Balance money.Money
}

// NaturalBalance calcula o saldo na natureza da conta.
func NaturalBalance(kind AccountKind, debits, credits money.Money) (money.Money, error) {
	if kind.DebitNormal() {
		return debits.Sub(credits)
	}
	return credits.Sub(debits)
}

// TransactionView é um lançamento com suas partidas, para consulta.
type TransactionView struct {
	Transaction
	CreatedAt time.Time
}

// Reader consulta o ledger.
type Reader interface {
	Balances(ctx context.Context, tenantID uuid.UUID) ([]AccountBalance, error)
	Transactions(ctx context.Context, tenantID uuid.UUID, referenceType string, referenceID *uuid.UUID, limit int, cursor string) ([]TransactionView, string, error)
}
