package payments

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// DiscrepancyKind é o tipo de divergência entre o Recorvia e o provedor.
type DiscrepancyKind string

const (
	// DiscrepancyMissingLocal: cobrança do provedor sem correspondência local.
	DiscrepancyMissingLocal DiscrepancyKind = "missing_local"
	// DiscrepancyMissingProvider: cobrança local inexistente no provedor.
	DiscrepancyMissingProvider DiscrepancyKind = "missing_provider"
	// DiscrepancyAmountMismatch: valor cobrado ou pago divergente.
	DiscrepancyAmountMismatch DiscrepancyKind = "amount_mismatch"
	// DiscrepancyFeeMismatch: taxa do provedor divergente.
	DiscrepancyFeeMismatch DiscrepancyKind = "fee_mismatch"
	// DiscrepancyPaymentNotReflected: pagamento confirmado em um lado e não
	// no outro.
	DiscrepancyPaymentNotReflected DiscrepancyKind = "payment_not_reflected"
	// DiscrepancyRefundPending: reembolso sem confirmação ou com totais
	// divergentes.
	DiscrepancyRefundPending DiscrepancyKind = "refund_pending"
	// DiscrepancyChargebackPending: situação de chargeback divergente.
	DiscrepancyChargebackPending DiscrepancyKind = "chargeback_pending"
	// DiscrepancyUnknownOperation: operação externa com resultado desconhecido.
	DiscrepancyUnknownOperation DiscrepancyKind = "unknown_operation"
)

// DiscrepancyKinds lista os tipos na ordem de exibição.
var DiscrepancyKinds = []DiscrepancyKind{DiscrepancyMissingLocal, DiscrepancyMissingProvider, DiscrepancyAmountMismatch,
	DiscrepancyFeeMismatch, DiscrepancyPaymentNotReflected, DiscrepancyRefundPending, DiscrepancyChargebackPending,
	DiscrepancyUnknownOperation}

// DiscrepancyStatus é a situação da divergência.
type DiscrepancyStatus string

const (
	DiscrepancyOpen      DiscrepancyStatus = "open"
	DiscrepancyResolved  DiscrepancyStatus = "resolved"
	DiscrepancyDismissed DiscrepancyStatus = "dismissed"
)

// Resolution é a ação de resolução de uma divergência.
type Resolution string

const (
	// ResolutionAcceptProvider adota a situação ou o valor do provedor
	// (aplica pagamento não refletido, sincroniza reembolso ou chargeback,
	// aceita a taxa com ajuste no ledger).
	ResolutionAcceptProvider Resolution = "accept_provider"
	// ResolutionKeepLocal mantém o registro local, com justificativa.
	ResolutionKeepLocal Resolution = "keep_local"
	// ResolutionAdjustLedger lança o ajuste de conciliação no ledger sem
	// alterar a cobrança.
	ResolutionAdjustLedger Resolution = "adjust_ledger"
	// ResolutionDismiss registra a divergência como explicada.
	ResolutionDismiss Resolution = "dismiss"
	// ResolutionAutoResolved: divergência não constatada em execução
	// posterior (resolução pelo sistema).
	ResolutionAutoResolved Resolution = "auto_resolved"
)

// RunTrigger identifica a origem da execução.
type RunTrigger string

const (
	TriggerManual    RunTrigger = "manual"
	TriggerScheduled RunTrigger = "scheduled"
)

// RunStatus é a situação de uma execução de conciliação.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
)

// ReconciliationRun é uma execução da conciliação sobre uma janela.
type ReconciliationRun struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	ProviderAccountID uuid.UUID
	Provider          string
	Trigger           RunTrigger
	Status            RunStatus
	WindowStart       time.Time
	WindowEnd         time.Time
	CheckedCount      int
	DiscrepancyCount  int
	Error             string
	RequestedBy       *uuid.UUID
	RequestKey        string
	LeaseUntil        *time.Time
	StartedAt         time.Time
	FinishedAt        *time.Time
}

// Discrepancy é uma divergência registrada pela conciliação.
type Discrepancy struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	RunID               uuid.UUID
	LastRunID           uuid.UUID
	Kind                DiscrepancyKind
	Status              DiscrepancyStatus
	DedupeKey           string
	Provider            string
	ChargeID            *uuid.UUID
	RefundID            *uuid.UUID
	DisputeID           *uuid.UUID
	OperationID         *uuid.UUID
	ProviderChargeID    string
	LocalAmount         *money.Money
	ProviderAmount      *money.Money
	LocalValue          map[string]any
	ProviderValue       map[string]any
	Occurrences         int
	FirstDetectedAt     time.Time
	LastDetectedAt      time.Time
	Resolution          Resolution
	Justification       string
	ResolvedBy          *uuid.UUID
	ResolvedAt          *time.Time
	LedgerTransactionID *uuid.UUID
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Version             int
}

// Finding é uma divergência constatada numa execução.
type Finding struct {
	Kind             DiscrepancyKind
	DedupeKey        string
	ChargeID         *uuid.UUID
	RefundID         *uuid.UUID
	OperationID      *uuid.UUID
	ProviderChargeID string
	LocalAmount      *money.Money
	ProviderAmount   *money.Money
	Local            map[string]any
	ProviderValue    map[string]any
}

// LocalCharge é uma cobrança local com seus reembolsos, para comparação.
type LocalCharge struct {
	Charge  Charge
	Refunds []Refund
}

// CompareInput reúne os dois lados de uma execução.
type CompareInput struct {
	ProviderCharges []ProviderCharge
	Local           []LocalCharge
	// MissingAtProvider contém IDs do provedor consultados individualmente
	// e inexistentes nele.
	MissingAtProvider map[string]bool
	UnknownOps        []Operation
	Now               time.Time
}

// CompareResult é o resultado da comparação.
type CompareResult struct {
	Findings []Finding
	// CheckedCharges são as cobranças locais efetivamente comparadas.
	CheckedCharges []uuid.UUID
	// CheckedProviderIDs são as cobranças do provedor efetivamente comparadas.
	CheckedProviderIDs []string
	unmatchedProvider  int
}

// Checked é o número de cobranças verificadas (locais e do provedor, sem
// contar duas vezes um par correspondente).
func (r CompareResult) Checked() int {
	return len(r.CheckedCharges) + r.unmatchedProvider
}

// RefundPendingAfter é o tempo a partir do qual um reembolso sem
// confirmação aparece na conciliação.
const RefundPendingAfter = time.Hour

func moneyPtr(m money.Money) *money.Money { return &m }

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }

func providerView(p ProviderCharge) map[string]any {
	v := map[string]any{"provider_charge_id": p.ProviderChargeID, "status": p.RawStatus, "normalized_status": string(p.Status),
		"amount": p.Amount.Amount(), "refunded_amount": p.RefundedAmount.Amount()}
	if p.Fee != nil || p.NetAmount != nil {
		v["fee"] = p.FeeOrDerived().Amount()
	}
	if p.ExternalReference != "" {
		v["external_reference"] = p.ExternalReference
	}
	if p.PaidAt != nil {
		v["paid_at"] = p.PaidAt.UTC().Format(time.RFC3339)
	}
	return v
}

func localView(c Charge) map[string]any {
	v := map[string]any{"charge_id": c.ID.String(), "invoice_id": c.InvoiceID.String(), "status": string(c.Status), "amount": c.Amount.Amount(),
		"amount_paid": c.AmountPaid.Amount(), "fee": c.Fee.Amount(), "amount_refunded": c.AmountRefunded.Amount(),
		"amount_charged_back": c.AmountChargedBack.Amount()}
	if c.ProviderChargeID != "" {
		v["provider_charge_id"] = c.ProviderChargeID
	}
	return v
}

func chargeFinding(kind DiscrepancyKind, c Charge, p *ProviderCharge, key string) Finding {
	f := Finding{Kind: kind, DedupeKey: key, ChargeID: uuidPtr(c.ID), ProviderChargeID: c.ProviderChargeID, Local: localView(c),
		ProviderValue: map[string]any{}}
	if p != nil {
		f.ProviderValue = providerView(*p)
		if f.ProviderChargeID == "" {
			f.ProviderChargeID = p.ProviderChargeID
		}
	}
	return f
}

func isChargebackStatus(s ProviderStatus) bool {
	return s == ProviderChargebackRequested || s == ProviderChargebackDispute
}

// compareCharge compara uma cobrança local com a visão do provedor.
func compareCharge(l LocalCharge, p ProviderCharge, now time.Time) []Finding {
	c := l.Charge
	id := c.ID.String()
	localPaid, providerPaid := IsSettledStatus(c.Status), p.Status.IsPaid()
	switch {
	case providerPaid && !localPaid:
		f := chargeFinding(DiscrepancyPaymentNotReflected, c, &p, "payment_not_reflected:"+id+":provider_paid")
		f.Local["direction"] = "provider_paid"
		f.LocalAmount, f.ProviderAmount = moneyPtr(money.Zero(c.Amount.Currency())), moneyPtr(p.Amount)
		return []Finding{f}
	case localPaid && !providerPaid:
		if p.Status == ProviderUnknownStatus {
			return nil
		}
		f := chargeFinding(DiscrepancyPaymentNotReflected, c, &p, "payment_not_reflected:"+id+":local_paid")
		f.Local["direction"] = "local_paid"
		f.LocalAmount, f.ProviderAmount = moneyPtr(c.AmountPaid), moneyPtr(money.Zero(c.Amount.Currency()))
		return []Finding{f}
	case !localPaid:
		open := c.Status == ChargePending || c.Status == ChargeAwaitingPayment
		if open && (p.Status == ProviderPending || p.Status == ProviderOverdue) && p.Amount != c.Amount {
			f := chargeFinding(DiscrepancyAmountMismatch, c, &p, fmt.Sprintf("amount_mismatch:%s:charge:%d:%d", id, c.Amount.Amount(), p.Amount.Amount()))
			f.Local["basis"] = "charge"
			f.LocalAmount, f.ProviderAmount = moneyPtr(c.Amount), moneyPtr(p.Amount)
			return []Finding{f}
		}
		return nil
	}
	var out []Finding
	if p.Amount != c.AmountPaid {
		f := chargeFinding(DiscrepancyAmountMismatch, c, &p, fmt.Sprintf("amount_mismatch:%s:paid:%d:%d", id, c.AmountPaid.Amount(), p.Amount.Amount()))
		f.Local["basis"] = "paid"
		f.LocalAmount, f.ProviderAmount = moneyPtr(c.AmountPaid), moneyPtr(p.Amount)
		out = append(out, f)
	}
	if p.Fee != nil || p.NetAmount != nil {
		if fee := p.FeeOrDerived(); fee != c.Fee {
			f := chargeFinding(DiscrepancyFeeMismatch, c, &p, fmt.Sprintf("fee_mismatch:%s:%d:%d", id, c.Fee.Amount(), fee.Amount()))
			f.LocalAmount, f.ProviderAmount = moneyPtr(c.Fee), moneyPtr(fee)
			out = append(out, f)
		}
	}
	out = append(out, compareRefunds(l, p, now)...)
	return append(out, compareChargeback(c, p)...)
}

// providerRefundedTotal soma os reembolsos efetivados no provedor.
func providerRefundedTotal(p ProviderCharge) money.Money {
	total := money.Zero(p.Amount.Currency())
	for _, e := range providerRefundEntries(p) {
		if e.State == RefundStateDone {
			if t, err := total.Add(e.Amount); err == nil {
				total = t
			}
		}
	}
	return total
}

func compareRefunds(l LocalCharge, p ProviderCharge, now time.Time) []Finding {
	c := l.Charge
	var out []Finding
	openSum := money.Zero(c.Amount.Currency())
	for _, r := range l.Refunds {
		if !r.Open() {
			continue
		}
		if s, err := openSum.Add(r.Amount); err == nil {
			openSum = s
		}
		if now.Sub(r.CreatedAt) < RefundPendingAfter {
			continue
		}
		f := chargeFinding(DiscrepancyRefundPending, c, &p, "refund_pending:refund:"+r.ID.String())
		f.RefundID = uuidPtr(r.ID)
		f.Local["refund_id"], f.Local["refund_status"], f.Local["refund_amount"] = r.ID.String(), string(r.Status), r.Amount.Amount()
		f.LocalAmount, f.ProviderAmount = moneyPtr(r.Amount), moneyPtr(providerRefundedTotal(p))
		out = append(out, f)
	}
	provider := providerRefundedTotal(p)
	diff, err := provider.Sub(c.AmountRefunded)
	if err != nil || diff.IsZero() {
		return out
	}
	if cmp, err := diff.Cmp(openSum); err == nil && diff.IsPositive() && cmp <= 0 {
		return out // explicada por reembolsos em andamento
	}
	f := chargeFinding(DiscrepancyRefundPending, c, &p,
		fmt.Sprintf("refund_pending:charge:%s:%d:%d", c.ID, c.AmountRefunded.Amount(), provider.Amount()))
	f.LocalAmount, f.ProviderAmount = moneyPtr(c.AmountRefunded), moneyPtr(provider)
	return append(out, f)
}

func compareChargeback(c Charge, p ProviderCharge) []Finding {
	open, lost, won := isChargebackStatus(p.Status), p.Status == ProviderChargebackLost, p.Status == ProviderChargebackReversed
	divergent := (open && c.Status != ChargeDisputed) || (lost && c.Status != ChargeChargedBack) ||
		(won && c.Status == ChargeDisputed) || (!open && !lost && !won && (c.Status == ChargeDisputed || c.Status == ChargeChargedBack))
	if !divergent {
		return nil
	}
	f := chargeFinding(DiscrepancyChargebackPending, c, &p, fmt.Sprintf("chargeback_pending:%s:%s:%s", c.ID, c.Status, p.Status))
	f.LocalAmount, f.ProviderAmount = moneyPtr(c.AmountChargedBack), moneyPtr(p.Amount)
	return []Finding{f}
}

// Compare confronta cobranças locais e do provedor. Cobranças locais com ID
// do provedor ausentes da listagem só são comparadas quando consultadas
// individualmente (presentes em ProviderCharges ou em MissingAtProvider).
// A referência externa só associa a cobrança do provedor à local ainda sem
// vínculo: cobrança do provedor com a referência de uma cobrança local já
// vinculada a outro ID (por exemplo, duplicada por uma criação ambígua) ou
// segunda cobrança com a mesma referência é apontada como sem
// correspondência local, nunca descartada.
func Compare(in CompareInput) CompareResult {
	byPID := map[string]int{}
	byRef := map[uuid.UUID]int{}
	for i, l := range in.Local {
		if l.Charge.ProviderChargeID != "" {
			byPID[l.Charge.ProviderChargeID] = i
		}
		byRef[l.Charge.ID] = i
	}
	var res CompareResult
	seenLocal := map[int]bool{}
	seenPID := map[string]bool{}
	for _, p := range in.ProviderCharges {
		if p.ProviderChargeID == "" || seenPID[p.ProviderChargeID] {
			continue
		}
		seenPID[p.ProviderChargeID] = true
		res.CheckedProviderIDs = append(res.CheckedProviderIDs, p.ProviderChargeID)
		i, ok := byPID[p.ProviderChargeID]
		var referenced *Charge
		if !ok {
			if ref, err := uuid.Parse(p.ExternalReference); err == nil {
				if j, found := byRef[ref]; found {
					c := in.Local[j].Charge
					referenced = &c
					i, ok = j, c.ProviderChargeID == "" && !seenLocal[j]
				}
			}
		}
		if !ok {
			res.unmatchedProvider++
			f := Finding{Kind: DiscrepancyMissingLocal, DedupeKey: "missing_local:" + p.ProviderChargeID,
				ProviderChargeID: p.ProviderChargeID, ProviderAmount: moneyPtr(p.Amount), Local: map[string]any{}, ProviderValue: providerView(p)}
			if referenced != nil {
				// A referência aponta uma cobrança local vinculada a outra
				// cobrança do provedor (ou já associada nesta execução).
				f.Local = map[string]any{"referenced_charge_id": referenced.ID.String(), "referenced_charge_status": string(referenced.Status)}
				if referenced.ProviderChargeID != "" {
					f.Local["referenced_provider_charge_id"] = referenced.ProviderChargeID
				}
			}
			res.Findings = append(res.Findings, f)
			continue
		}
		seenLocal[i] = true
		res.CheckedCharges = append(res.CheckedCharges, in.Local[i].Charge.ID)
		res.Findings = append(res.Findings, compareCharge(in.Local[i], p, in.Now)...)
	}
	for i, l := range in.Local {
		c := l.Charge
		if seenLocal[i] || c.ProviderChargeID == "" || !in.MissingAtProvider[c.ProviderChargeID] {
			continue
		}
		res.CheckedCharges = append(res.CheckedCharges, c.ID)
		f := chargeFinding(DiscrepancyMissingProvider, c, nil, "missing_provider:"+c.ID.String())
		f.LocalAmount = moneyPtr(c.Amount)
		res.Findings = append(res.Findings, f)
	}
	for _, op := range in.UnknownOps {
		// Operação que ficou em execução depois do lease (processo
		// interrompido durante a chamada) tem resultado desconhecido.
		if op.Status != OpUnknown && !op.Stalled(in.Now) {
			continue
		}
		f := Finding{Kind: DiscrepancyUnknownOperation, DedupeKey: "unknown_operation:" + op.ID.String(), ChargeID: uuidPtr(op.ChargeID),
			RefundID: op.RefundID, OperationID: uuidPtr(op.ID), ProviderValue: map[string]any{},
			Local: map[string]any{"operation_id": op.ID.String(), "kind": string(op.Kind), "status": string(op.Status), "attempts": op.Attempts,
				"last_error": op.LastError, "external_reference": op.ExternalReference}}
		res.Findings = append(res.Findings, f)
	}
	slices.SortStableFunc(res.Findings, func(a, b Finding) int { return strings.Compare(a.DedupeKey, b.DedupeKey) })
	return res
}

// resolutionApplies informa se a ação de resolução vale para o tipo.
func resolutionApplies(kind DiscrepancyKind, r Resolution) bool {
	switch r {
	case ResolutionDismiss, ResolutionKeepLocal:
		return true
	case ResolutionAdjustLedger:
		return kind == DiscrepancyAmountMismatch || kind == DiscrepancyFeeMismatch
	case ResolutionAcceptProvider:
		return kind != DiscrepancyMissingLocal
	}
	return false
}

// ResolveInput é o pedido de resolução de uma divergência.
type ResolveInput struct {
	ID            uuid.UUID
	Resolution    Resolution
	Justification string
}

func validateResolve(in ResolveInput) (ResolveInput, error) {
	var f []apperr.FieldError
	switch in.Resolution {
	case ResolutionAcceptProvider, ResolutionKeepLocal, ResolutionAdjustLedger, ResolutionDismiss:
	default:
		f = append(f, apperr.FieldError{Field: "resolution", Message: "Use accept_provider, keep_local, adjust_ledger ou dismiss."})
	}
	in.Justification = strings.TrimSpace(in.Justification)
	if n := len([]rune(in.Justification)); n < 10 || n > 2000 || strings.ContainsRune(in.Justification, 0) {
		f = append(f, apperr.FieldError{Field: "justification", Message: "A justificativa deve ter de 10 a 2000 caracteres."})
	}
	if len(f) > 0 {
		return in, apperr.Invalid("invalid_resolution", "Resolução inválida.", f...)
	}
	return in, nil
}
