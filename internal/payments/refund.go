package payments

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// RefundStatus é a situação de um reembolso.
type RefundStatus string

const (
	// RefundRequested: pedido registrado, ainda sem confirmação do provedor.
	RefundRequested RefundStatus = "requested"
	// RefundSucceeded: efetivado pelo provedor.
	RefundSucceeded RefundStatus = "succeeded"
	// RefundFailed: recusado pelo provedor ou comprovadamente não executado.
	RefundFailed RefundStatus = "failed"
	// RefundUnknown: o pedido pode ter sido processado (resposta ambígua);
	// o provedor é consultado antes de qualquer repetição.
	RefundUnknown RefundStatus = "unknown"
)

// RefundOrigin indica quem iniciou o reembolso.
type RefundOrigin string

const (
	// RefundOriginRecorvia: pedido feito pelo Recorvia (operação externa).
	RefundOriginRecorvia RefundOrigin = "recorvia"
	// RefundOriginProvider: reembolso feito diretamente no provedor e
	// conhecido por webhook ou consulta.
	RefundOriginProvider RefundOrigin = "provider"
)

// Refund é um reembolso total ou parcial de uma cobrança paga.
type Refund struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	ChargeID uuid.UUID
	Provider string
	Origin   RefundOrigin
	Status   RefundStatus
	Amount   money.Money
	// CreditAmount é a parte do reembolso que consumiu crédito do cliente
	// gerado pelo excedente do pagamento da própria cobrança.
	CreditAmount     money.Money
	Reason           string
	ProviderRefundID string
	FailureReason    string
	RequestedBy      *uuid.UUID
	RequestKey       string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	CompletedAt      *time.Time
	Version          int
}

// Open informa se o reembolso ainda aguarda confirmação do provedor.
func (r Refund) Open() bool { return r.Status == RefundRequested || r.Status == RefundUnknown }

// RefundInput é o pedido de reembolso de uma cobrança paga.
type RefundInput struct {
	ChargeID uuid.UUID
	Amount   money.Money
	Reason   string
	// RequestKey identifica o pedido (derivado do Idempotency-Key); repetir
	// a chave devolve o reembolso já registrado.
	RequestKey string
}

func validateRefundInput(in RefundInput) (RefundInput, error) {
	var f []apperr.FieldError
	if !in.Amount.IsPositive() || in.Amount.CheckBusinessRange() != nil {
		f = append(f, apperr.FieldError{Field: "amount", Message: "Informe um valor positivo em centavos."})
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if n := len([]rune(in.Reason)); n < 3 || n > 500 || strings.ContainsRune(in.Reason, 0) {
		f = append(f, apperr.FieldError{Field: "reason", Message: "Informe o motivo com 3 a 500 caracteres."})
	}
	if len(in.RequestKey) > 300 {
		f = append(f, apperr.FieldError{Field: "request_key", Message: "Chave do pedido muito longa."})
	}
	if len(f) > 0 {
		return in, apperr.Invalid("invalid_refund", "Reembolso inválido.", f...)
	}
	return in, nil
}

// refundMatch associa um reembolso local à evidência do provedor.
type refundMatch struct {
	refundID         uuid.UUID
	providerRefundID string
}

// refundPlan é o efeito de um retrato do provedor sobre os reembolsos
// locais de uma cobrança.
type refundPlan struct {
	confirm []refundMatch
	fail    []refundMatch
	// reopen reúne reembolsos encerrados localmente como recusados que o
	// provedor informa, com associação exata, como efetivados.
	reopen []refundMatch
	// pending reúne reembolsos que o provedor aceitou e ainda processa.
	pending map[uuid.UUID]bool
	// external são reembolsos feitos diretamente no provedor, sem
	// correspondência local, a registrar.
	external []ProviderRefund
}

// covers informa se o retrato do provedor dá notícia do reembolso
// (efetivado, recusado ou em processamento).
func (p refundPlan) covers(id uuid.UUID) bool {
	match := func(m refundMatch) bool { return m.refundID == id }
	return p.pending[id] || slices.ContainsFunc(p.confirm, match) || slices.ContainsFunc(p.fail, match)
}

func (p refundPlan) empty() bool {
	return len(p.confirm) == 0 && len(p.fail) == 0 && len(p.reopen) == 0 && len(p.external) == 0
}

// providerRefundEntries devolve os reembolsos informados pelo provedor. Sem
// lista, o total reembolsado vira uma entrada sem identificação; cobrança
// reembolsada sem valor informado é considerada reembolsada integralmente.
func providerRefundEntries(pc ProviderCharge) []ProviderRefund {
	if len(pc.Refunds) > 0 {
		return pc.Refunds
	}
	amount := pc.RefundedAmount
	if !amount.IsPositive() && pc.Status == ProviderRefunded {
		amount = pc.Amount
	}
	if !amount.IsPositive() {
		return nil
	}
	return []ProviderRefund{{State: RefundStateDone, Amount: amount}}
}

// planRefundSync compara os reembolsos informados pelo provedor com os
// locais. Entradas com referência externa (o ID local) ou com o ID do
// provedor já registrado são associadas exatamente. Entradas sem
// identificação (provedores que não devolvem ID nem referência) são
// atribuídas por valor, em ordem de criação, aos reembolsos locais já
// enviados ao provedor e ainda abertos; o que sobra efetivado sem
// reembolso local aberto é um reembolso feito diretamente no provedor.
// Reembolso local recusado com evidência exata de efetivação no provedor é
// devolvido em reopen, para confirmação explícita. dispatched indica os reembolsos cuja operação já foi enviada ao provedor
// ao menos uma vez: reembolsos ainda não enviados nunca recebem atribuição.
func planRefundSync(pc ProviderCharge, local []Refund, dispatched map[uuid.UUID]bool) refundPlan {
	plan := refundPlan{pending: map[uuid.UUID]bool{}}
	byID := map[string]int{}
	byProvider := map[string]int{}
	for i, r := range local {
		byID[r.ID.String()] = i
		if r.ProviderRefundID != "" {
			byProvider[r.ProviderRefundID] = i
		}
	}
	matched := map[uuid.UUID]bool{}
	cur := pc.Amount.Currency()
	anonDone, anonPending := money.Zero(cur), money.Zero(cur)
	for _, e := range providerRefundEntries(pc) {
		if !e.Amount.IsPositive() {
			continue
		}
		i, ok := byID[e.ExternalReference]
		if !ok && e.ProviderRefundID != "" {
			i, ok = byProvider[e.ProviderRefundID]
		}
		if ok {
			r := local[i]
			matched[r.ID] = true
			m := refundMatch{refundID: r.ID, providerRefundID: e.ProviderRefundID}
			if !r.Open() {
				if r.Status == RefundFailed && e.State == RefundStateDone {
					plan.reopen = append(plan.reopen, m)
				}
				continue
			}
			switch e.State {
			case RefundStateDone:
				plan.confirm = append(plan.confirm, m)
			case RefundStateFailed:
				plan.fail = append(plan.fail, m)
			default:
				plan.pending[r.ID] = true
			}
			continue
		}
		if e.ProviderRefundID != "" || e.ExternalReference != "" {
			// Reembolso identificado pelo provedor sem correspondência local.
			if e.State == RefundStateDone {
				plan.external = append(plan.external, e)
			}
			continue
		}
		var err error
		switch e.State {
		case RefundStateDone:
			anonDone, err = anonDone.Add(e.Amount)
		case RefundStatePending:
			anonPending, err = anonPending.Add(e.Amount)
		}
		if err != nil {
			return refundPlan{pending: map[uuid.UUID]bool{}}
		}
	}
	if !anonDone.IsPositive() && !anonPending.IsPositive() {
		return plan
	}
	// Atribuição por valor: desconta o que já está confirmado localmente sem
	// associação exata e distribui o restante aos reembolsos abertos.
	available := anonDone
	var open []Refund
	for _, r := range local {
		if matched[r.ID] {
			continue
		}
		var err error
		switch {
		case r.Status == RefundSucceeded:
			available, err = available.Sub(r.Amount)
		case r.Open() && dispatched[r.ID]:
			open = append(open, r)
		}
		if err != nil {
			return plan
		}
	}
	slices.SortStableFunc(open, func(a, b Refund) int { return a.CreatedAt.Compare(b.CreatedAt) })
	next := 0
	for ; next < len(open); next++ {
		if cmp, err := open[next].Amount.Cmp(available); err != nil || cmp > 0 {
			break
		}
		plan.confirm = append(plan.confirm, refundMatch{refundID: open[next].ID})
		available, _ = available.Sub(open[next].Amount)
	}
	pendingLeft := anonPending
	for i := next; i < len(open); i++ {
		if cmp, err := open[i].Amount.Cmp(pendingLeft); err != nil || cmp > 0 {
			break
		}
		plan.pending[open[i].ID] = true
		pendingLeft, _ = pendingLeft.Sub(open[i].Amount)
	}
	if next == len(open) && available.IsPositive() {
		plan.external = append(plan.external, ProviderRefund{State: RefundStateDone, Amount: available})
	}
	return plan
}
