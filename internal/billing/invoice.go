// Package billing modela as faturas comerciais dos tenants a seus clientes e
// o motor de recorrência que as gera a partir das assinaturas.
//
// Uma fatura emitida não é reescrita nem excluída: itens e valores faturados
// ficam congelados (também no banco, por gatilho). O andamento (pagamentos,
// notas de crédito, cancelamento, baixa como incobrável) evolui com histórico
// de transições e lançamentos no ledger. Fatura comercial não é documento
// fiscal.
package billing

import (
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Status da fatura.
type Status string

const (
	Draft         Status = "draft"
	Open          Status = "open"
	PartiallyPaid Status = "partially_paid"
	Paid          Status = "paid"
	Void          Status = "void"
	Uncollectible Status = "uncollectible"
)

var transitions = map[Status][]Status{
	Draft:         {Open, Paid},
	Open:          {PartiallyPaid, Paid, Void, Uncollectible},
	PartiallyPaid: {Paid, Uncollectible},
	Uncollectible: {Paid},
	Paid:          {},
	Void:          {},
}

// CanTransition informa se a transição é permitida.
func CanTransition(from, to Status) bool { return slices.Contains(transitions[from], to) }

// Kind é a origem da fatura.
type Kind string

const (
	KindSubscriptionCycle Kind = "subscription_cycle"
	KindFinalUsage        Kind = "final_usage"
	KindProration         Kind = "proration"
	KindManual            Kind = "manual"
)

// ItemKind é o tipo de item.
type ItemKind string

const (
	ItemSubscriptionFee ItemKind = "subscription_fee"
	ItemUsage           ItemKind = "usage"
	ItemOverage         ItemKind = "overage"
	ItemProration       ItemKind = "proration"
	ItemAdjustment      ItemKind = "adjustment"
)

// Item é um item discriminado da fatura. Quantity e UnitAmount são exatos;
// Amount é o valor arredondado uma única vez.
//
// DiscountExempt exclui o item da base e da distribuição de descontos do
// rascunho (por exemplo, ajuste de correção de consumo cujo valor já
// considera o desconto do período corrigido). Não é persistido: o desconto
// só é aplicado antes da emissão.
type Item struct {
	ID               uuid.UUID
	Position         int
	Kind             ItemKind
	Description      string
	Quantity         *big.Rat
	UnitAmount       *big.Rat
	Amount           money.Money
	DiscountAmount   money.Money
	Period           *calendar.Period
	PlanID           *uuid.UUID
	PriceVersionID   *uuid.UUID
	PriceSnapshot    map[string]any
	UsageAggregateID *uuid.UUID
	DiscountExempt   bool
}

// StatusChange é uma transição registrada no histórico.
type StatusChange struct {
	From    *Status
	To      Status
	At      time.Time
	Reason  string
	Details map[string]any
}

// Invoice é o agregado de fatura.
type Invoice struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	CustomerID       uuid.UUID
	SubscriptionID   *uuid.UUID
	Number           string
	Status           Status
	Kind             Kind
	Currency         money.Currency
	BillingPeriod    *calendar.Period
	IssuedAt         *time.Time
	DueAt            *time.Time
	Subtotal         money.Money
	DiscountTotal    money.Money
	Total            money.Money
	CreditApplied    money.Money
	AmountPaid       money.Money
	AmountCredited   money.Money
	AmountWrittenOff money.Money
	AmountDue        money.Money
	CustomerSnapshot customers.Snapshot
	// Issuer são os dados do emitente gravados na emissão; nil nas faturas
	// emitidas antes da ADR-079 (o documento usa então os dados atuais da
	// organização).
	Issuer          *Issuer
	Memo            string
	PaidAt          *time.Time
	VoidedAt        *time.Time
	VoidReason      string
	UncollectibleAt *time.Time
	Items           []Item
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Version         int

	changes []StatusChange
}

// Issuer identifica o emitente (a organização) como estava na emissão.
type Issuer struct {
	Name           string `json:"name"`
	LegalName      string `json:"legal_name,omitempty"`
	DocumentNumber string `json:"document_number,omitempty"`
}

// NewDraft cria uma fatura em rascunho.
func NewDraft(id, tenantID, customerID uuid.UUID, subscriptionID *uuid.UUID, kind Kind, cur money.Currency, period *calendar.Period, snapshot customers.Snapshot, now time.Time) *Invoice {
	z := money.Zero(cur)
	return &Invoice{
		ID: id, TenantID: tenantID, CustomerID: customerID, SubscriptionID: subscriptionID, Status: Draft, Kind: kind,
		Currency: cur, BillingPeriod: period, Subtotal: z, DiscountTotal: z, Total: z, CreditApplied: z, AmountPaid: z,
		AmountCredited: z, AmountWrittenOff: z, AmountDue: z, CustomerSnapshot: snapshot, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
}

// PopChanges retorna e limpa as transições pendentes de histórico.
func (inv *Invoice) PopChanges() []StatusChange {
	c := inv.changes
	inv.changes = nil
	return c
}

func (inv *Invoice) transition(to Status, at time.Time, reason string, details map[string]any) error {
	if !CanTransition(inv.Status, to) {
		return apperr.Conflict("invalid_invoice_transition", fmt.Sprintf("Transição de fatura não permitida: %s para %s.", inv.Status, to))
	}
	from := inv.Status
	inv.Status = to
	inv.UpdatedAt = at
	inv.changes = append(inv.changes, StatusChange{From: &from, To: to, At: at, Reason: reason, Details: details})
	return nil
}

// AddItem adiciona um item ao rascunho e recalcula os totais.
func (inv *Invoice) AddItem(it Item) error {
	if inv.Status != Draft {
		return apperr.Conflict("invoice_not_draft", "Itens só podem ser adicionados a rascunhos.")
	}
	if it.Amount.Currency() != inv.Currency {
		return money.ErrCurrencyMismatch
	}
	if it.DiscountAmount.Currency().IsZero() {
		it.DiscountAmount = money.Zero(inv.Currency)
	}
	it.Position = len(inv.Items)
	inv.Items = append(inv.Items, it)
	if err := inv.recalc(); err != nil {
		inv.Items = inv.Items[:len(inv.Items)-1]
		return err
	}
	return nil
}

// DiscountableBase é a base de um desconto da fatura: subtotal líquido de
// descontos já aplicados, sem os itens isentos de desconto.
func (inv *Invoice) DiscountableBase() (money.Money, error) {
	base, err := inv.Subtotal.Sub(inv.DiscountTotal)
	if err != nil {
		return money.Money{}, err
	}
	for _, it := range inv.Items {
		if !it.DiscountExempt {
			continue
		}
		net, err := it.Amount.Sub(it.DiscountAmount)
		if err != nil {
			return money.Money{}, err
		}
		if base, err = base.Sub(net); err != nil {
			return money.Money{}, err
		}
	}
	return base, nil
}

// ApplyDiscount distribui um desconto entre os itens positivos não isentos
// pelo maior resto (determinístico), limitado ao subtotal positivo.
func (inv *Invoice) ApplyDiscount(d money.Money) (money.Money, error) {
	if inv.Status != Draft {
		return money.Money{}, apperr.Conflict("invoice_not_draft", "Descontos só podem ser aplicados a rascunhos.")
	}
	if !d.IsPositive() {
		return money.Zero(inv.Currency), nil
	}
	weights := make([]int64, len(inv.Items))
	var positive, exempt int64
	for i, it := range inv.Items {
		remaining := it.Amount.Amount() - it.DiscountAmount.Amount()
		if it.DiscountExempt {
			exempt += remaining
			continue
		}
		if remaining > 0 {
			weights[i] = remaining
			positive += remaining
		}
	}
	// O desconto é limitado ao subtotal líquido sem os itens isentos (itens
	// negativos, como créditos de prorrata, reduzem a base) e distribuído
	// entre os itens positivos não isentos.
	limit := min(positive, inv.Total.Amount()-exempt)
	if limit <= 0 {
		return money.Zero(inv.Currency), nil
	}
	if d.Amount() > limit {
		d = money.New(limit, inv.Currency)
	}
	shares, err := money.Allocate(d, weights)
	if err != nil {
		return money.Money{}, err
	}
	before := make([]money.Money, len(inv.Items))
	for i := range inv.Items {
		before[i] = inv.Items[i].DiscountAmount
		if inv.Items[i].DiscountAmount, err = inv.Items[i].DiscountAmount.Add(shares[i]); err != nil {
			return money.Money{}, err
		}
	}
	if err := inv.recalc(); err != nil {
		for i := range inv.Items {
			inv.Items[i].DiscountAmount = before[i]
		}
		return money.Money{}, err
	}
	return d, nil
}

// ApplyCredit consome até available do crédito do cliente, limitado ao total.
func (inv *Invoice) ApplyCredit(available money.Money) (money.Money, error) {
	if inv.Status != Draft {
		return money.Money{}, apperr.Conflict("invoice_not_draft", "Créditos são aplicados na emissão.")
	}
	if !available.IsPositive() {
		return money.Zero(inv.Currency), nil
	}
	used, err := money.Min(available, inv.AmountDue)
	if err != nil {
		return money.Money{}, err
	}
	if inv.CreditApplied, err = inv.CreditApplied.Add(used); err != nil {
		return money.Money{}, err
	}
	return used, inv.recalc()
}

func (inv *Invoice) recalc() error {
	sub, disc := money.Zero(inv.Currency), money.Zero(inv.Currency)
	for _, it := range inv.Items {
		var err error
		if sub, err = sub.Add(it.Amount); err != nil {
			return err
		}
		if disc, err = disc.Add(it.DiscountAmount); err != nil {
			return err
		}
	}
	total, err := sub.Sub(disc)
	if err != nil {
		return err
	}
	if total.IsNegative() {
		return apperr.Invalid("negative_invoice", "O total da fatura não pode ser negativo.")
	}
	if err := total.CheckBusinessRange(); err != nil {
		return apperr.Invalid("invoice_amount_limit", "O total da fatura excede o limite permitido.")
	}
	due, err := money.Sum(inv.Currency, total)
	if err != nil {
		return err
	}
	for _, v := range []money.Money{inv.CreditApplied, inv.AmountPaid, inv.AmountCredited, inv.AmountWrittenOff} {
		if due, err = due.Sub(v); err != nil {
			return err
		}
	}
	if due.IsNegative() {
		return apperr.Conflict("invoice_overapplied", "Créditos e pagamentos excedem o total da fatura.")
	}
	inv.Subtotal, inv.DiscountTotal, inv.Total, inv.AmountDue = sub, disc, total, due
	return nil
}

// HasChargeableItems informa se há algum item com valor.
func (inv *Invoice) HasChargeableItems() bool {
	for _, it := range inv.Items {
		if !it.Amount.IsZero() {
			return true
		}
	}
	return false
}

// FormatNumber formata o número sequencial da fatura. A sequência é única
// por tenant e nunca reinicia; o ano é informativo.
func FormatNumber(prefix string, year int, seq int64) string {
	return fmt.Sprintf("%s-%04d-%06d", prefix, year, seq)
}

// DueAt calcula o vencimento: fim do dia civil (exclusivo) issue + termsDays
// no fuso do tenant. A fatura vence quando now >= DueAt.
func DueAt(issuedAt time.Time, termsDays int, loc *time.Location) time.Time {
	return calendar.DateOf(issuedAt, loc).AddDays(termsDays + 1).StartIn(loc)
}

// DueDate é a data civil de vencimento exibida ao cliente.
func DueDate(dueAt time.Time, loc *time.Location) calendar.Date {
	return calendar.DateOf(dueAt.Add(-time.Nanosecond), loc)
}

// Issue emite a fatura: atribui número e vencimento. Faturas com saldo zero
// (cobertas por crédito) já nascem pagas.
func (inv *Invoice) Issue(number string, issuedAt, dueAt time.Time) error {
	if inv.Status != Draft {
		return apperr.Conflict("invoice_not_draft", "A fatura já foi emitida.")
	}
	if len(inv.Items) == 0 {
		return apperr.Invalid("invoice_without_items", "A fatura não possui itens.")
	}
	inv.Number, inv.IssuedAt, inv.DueAt = number, &issuedAt, &dueAt
	if inv.AmountDue.IsZero() {
		inv.PaidAt = &issuedAt
		return inv.transition(Paid, issuedAt, "issued_fully_credited", nil)
	}
	return inv.transition(Open, issuedAt, "issued", nil)
}

// IsOverdue informa se há saldo vencido.
func (inv *Invoice) IsOverdue(now time.Time) bool {
	return (inv.Status == Open || inv.Status == PartiallyPaid) && inv.DueAt != nil && !now.Before(*inv.DueAt)
}

// ApplyPayment aplica um recebimento. Retorna o valor aplicado e o excedente
// (pago a maior), que o chamador converte em crédito do cliente. Pagamento de
// fatura baixada como incobrável reverte a baixa na mesma proporção.
func (inv *Invoice) ApplyPayment(amount money.Money, at time.Time) (applied, excess money.Money, err error) {
	applied, excess, _, err = inv.ApplyPaymentWithRecovery(amount, at)
	return applied, excess, err
}

// ApplyPaymentWithRecovery é ApplyPayment informando também quanto da baixa
// como incobrável foi revertido (o chamador estorna a perda no ledger). Uma
// fatura incobrável só fica paga quando toda a baixa é recuperada.
func (inv *Invoice) ApplyPaymentWithRecovery(amount money.Money, at time.Time) (applied, excess, recovered money.Money, err error) {
	recovered = money.Zero(inv.Currency)
	if !amount.IsPositive() {
		return money.Money{}, money.Money{}, money.Money{}, apperr.Invalid("invalid_amount", "O valor do pagamento deve ser positivo.")
	}
	fail := func(e error) (money.Money, money.Money, money.Money, error) {
		return money.Money{}, money.Money{}, money.Money{}, e
	}
	switch inv.Status {
	case Open, PartiallyPaid:
		if applied, err = money.Min(amount, inv.AmountDue); err != nil {
			return fail(err)
		}
	case Uncollectible:
		owed, e := inv.AmountDue.Add(inv.AmountWrittenOff)
		if e != nil {
			return fail(e)
		}
		if applied, err = money.Min(amount, owed); err != nil {
			return fail(err)
		}
		if recovered, err = money.Min(applied, inv.AmountWrittenOff); err != nil {
			return fail(err)
		}
		if inv.AmountWrittenOff, err = inv.AmountWrittenOff.Sub(recovered); err != nil {
			return fail(err)
		}
	default:
		return fail(apperr.Conflict("invoice_not_payable", "A fatura não aceita pagamentos nesta situação."))
	}
	if inv.AmountPaid, err = inv.AmountPaid.Add(applied); err != nil {
		return fail(err)
	}
	if err = inv.recalc(); err != nil {
		return fail(err)
	}
	if excess, err = amount.Sub(applied); err != nil {
		return fail(err)
	}
	details := map[string]any{"applied": applied.Amount(), "excess": excess.Amount()}
	switch {
	case inv.AmountDue.IsZero() && inv.AmountWrittenOff.IsZero():
		inv.PaidAt = &at
		err = inv.transition(Paid, at, "payment", details)
	case inv.Status == Uncollectible:
		// Recuperação parcial: o restante continua baixado.
		details["recovered"], details["still_written_off"] = recovered.Amount(), inv.AmountWrittenOff.Amount()
		inv.changes = append(inv.changes, StatusChange{From: ptr(inv.Status), To: inv.Status, At: at, Reason: "payment", Details: details})
	case inv.Status != PartiallyPaid:
		err = inv.transition(PartiallyPaid, at, "payment", details)
	}
	if err != nil {
		return fail(err)
	}
	return applied, excess, recovered, nil
}

// Void cancela uma fatura em aberto sem pagamentos nem notas de crédito. O
// chamador estorna o lançamento de emissão no ledger e devolve ao cliente o
// crédito aplicado; por isso a fatura cancelada não tem saldo devedor
// (constraint invoices_due_consistency, ADR-079). CreditApplied continua
// registrando o crédito que foi aplicado na emissão (e devolvido).
func (inv *Invoice) Void(reason string, at time.Time) error {
	if inv.Status != Open {
		return apperr.Conflict("invoice_not_voidable", "Somente faturas em aberto sem pagamentos podem ser canceladas.")
	}
	if !inv.AmountPaid.IsZero() || !inv.AmountCredited.IsZero() {
		return apperr.Conflict("invoice_has_payments", "A fatura possui pagamentos ou notas de crédito; use nota de crédito para corrigi-la.")
	}
	if l := len([]rune(reason)); l < 3 || l > 500 {
		return apperr.Invalid("invalid_reason", "Informe o motivo do cancelamento (3 a 500 caracteres).")
	}
	inv.VoidedAt, inv.VoidReason = &at, reason
	inv.AmountDue = money.Zero(inv.Currency)
	return inv.transition(Void, at, reason, nil)
}

// MarkUncollectible baixa o saldo como incobrável.
func (inv *Invoice) MarkUncollectible(reason string, at time.Time) (money.Money, error) {
	if inv.Status != Open && inv.Status != PartiallyPaid {
		return money.Money{}, apperr.Conflict("invoice_not_open", "Somente faturas em aberto podem ser baixadas.")
	}
	writeOff := inv.AmountDue
	var err error
	if inv.AmountWrittenOff, err = inv.AmountWrittenOff.Add(writeOff); err != nil {
		return money.Money{}, err
	}
	if err := inv.recalc(); err != nil {
		return money.Money{}, err
	}
	inv.UncollectibleAt = &at
	return writeOff, inv.transition(Uncollectible, at, reason, map[string]any{"written_off": writeOff.Amount()})
}

// ApplyCreditNote reduz o saldo devedor por nota de crédito.
func (inv *Invoice) ApplyCreditNote(amount money.Money, at time.Time) error {
	if inv.Status != Open && inv.Status != PartiallyPaid {
		return apperr.Conflict("invoice_not_open", "Notas de crédito só se aplicam a faturas com saldo em aberto.")
	}
	if !amount.IsPositive() {
		return apperr.Invalid("invalid_amount", "O valor da nota de crédito deve ser positivo.")
	}
	if c, err := amount.Cmp(inv.AmountDue); err != nil || c > 0 {
		return apperr.Invalid("credit_note_exceeds_due", "A nota de crédito excede o saldo devedor.")
	}
	var err error
	if inv.AmountCredited, err = inv.AmountCredited.Add(amount); err != nil {
		return err
	}
	if err := inv.recalc(); err != nil {
		return err
	}
	if inv.AmountDue.IsZero() {
		inv.PaidAt = &at
		return inv.transition(Paid, at, "credit_note", map[string]any{"amount": amount.Amount()})
	}
	inv.changes = append(inv.changes, StatusChange{From: ptr(inv.Status), To: inv.Status, At: at, Reason: "credit_note",
		Details: map[string]any{"amount": amount.Amount()}})
	return nil
}

func ptr[T any](v T) *T { return &v }
