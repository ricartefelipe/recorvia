// Package ledger implementa o ledger operacional imutável de partidas
// dobradas do tenant. Cada transação tem ao menos duas partidas e soma zero
// em cada moeda (débitos = créditos). Correções usam transações de estorno;
// nenhum lançamento é alterado ou removido.
//
// O ledger registra a movimentação operacional (faturamento, recebíveis,
// liquidação no provedor, taxas, reembolsos, chargebacks). Ele não substitui
// a contabilidade fiscal nem o reconhecimento contábil de receita.
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
	AccountsReceivable        AccountCode = "accounts_receivable"
	BilledRevenue             AccountCode = "billed_revenue"
	ProviderClearing          AccountCode = "provider_clearing"
	Cash                      AccountCode = "cash"
	ProviderFees              AccountCode = "provider_fees"
	Refunds                   AccountCode = "refunds"
	Chargebacks               AccountCode = "chargebacks"
	CustomerCredits           AccountCode = "customer_credits"
	RevenueAdjustments        AccountCode = "revenue_adjustments"
	BadDebt                   AccountCode = "bad_debt"
	ReconciliationAdjustments AccountCode = "reconciliation_adjustments"
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
	{ProviderClearing, "Valores em liquidação no provedor", Asset},
	{Cash, "Disponível liquidado", Asset},
	{ProviderFees, "Taxas do provedor", Expense},
	{Refunds, "Reembolsos", ContraRevenue},
	{Chargebacks, "Chargebacks", ContraRevenue},
	{CustomerCredits, "Créditos de clientes", Liability},
	{RevenueAdjustments, "Ajustes de faturamento", ContraRevenue},
	{BadDebt, "Perdas com inadimplência", Expense},
	{ReconciliationAdjustments, "Ajustes de conciliação", Expense},
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

// ProviderPaymentReceived: confirmação de pagamento no provedor. O valor
// bruto sai de contas a receber para liquidação; a taxa é despesa.
func ProviderPaymentReceived(id, tenant, paymentID uuid.UUID, gross, fee money.Money, at time.Time) (Transaction, bool, error) {
	entries := pair(ProviderClearing, AccountsReceivable, gross)
	entries = append(entries, pair(ProviderFees, ProviderClearing, fee)...)
	return build(id, tenant, "payment.confirmed", "Pagamento confirmado no provedor", at, "payment", paymentID, "payment.confirmed:"+paymentID.String(), entries)
}

// ManualPaymentReceived: recebimento registrado manualmente.
func ManualPaymentReceived(id, tenant, paymentID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "payment.manual", "Recebimento registrado manualmente", at, "payment", paymentID,
		"payment.manual:"+paymentID.String(), pair(Cash, AccountsReceivable, amount))
}

// Overpayment: valor recebido além do saldo vira crédito do cliente.
func Overpayment(id, tenant, paymentID uuid.UUID, amount money.Money, viaProvider bool, at time.Time) (Transaction, bool, error) {
	debit := Cash
	if viaProvider {
		debit = ProviderClearing
	}
	return build(id, tenant, "payment.overpayment", "Pagamento excedente convertido em crédito", at, "payment", paymentID,
		"payment.overpayment:"+paymentID.String(), pair(debit, CustomerCredits, amount))
}

// RefundSucceeded: reembolso efetivado pelo provedor.
func RefundSucceeded(id, tenant, refundID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "refund.succeeded", "Reembolso efetivado", at, "refund", refundID,
		"refund.succeeded:"+refundID.String(), pair(Refunds, ProviderClearing, amount))
}

// ChargebackOpened: valor contestado retido pelo provedor.
func ChargebackOpened(id, tenant, disputeID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "dispute.opened", "Chargeback aberto", at, "dispute", disputeID,
		"dispute.opened:"+disputeID.String(), pair(Chargebacks, ProviderClearing, amount))
}

// InvoiceWrittenOff: saldo considerado incobrável.
func InvoiceWrittenOff(id, tenant, invoiceID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "invoice.uncollectible", "Saldo considerado incobrável", at, "invoice", invoiceID,
		"invoice.uncollectible:"+invoiceID.String(), pair(BadDebt, AccountsReceivable, amount))
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

// SettlementReceived: liquidação do provedor para o disponível.
func SettlementReceived(id, tenant, referenceID uuid.UUID, amount money.Money, at time.Time) (Transaction, bool, error) {
	return build(id, tenant, "settlement.received", "Liquidação recebida do provedor", at, "settlement", referenceID,
		"settlement.received:"+referenceID.String(), pair(Cash, ProviderClearing, amount))
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
