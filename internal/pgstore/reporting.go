package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/reporting"
)

// Reporting implementa reporting.Source.
type Reporting struct{ s *Store }

// Reporting retorna a fonte do dashboard.
func (s *Store) Reporting() *Reporting { return &Reporting{s: s} }

// Location retorna o fuso do tenant.
func (r *Reporting) Location(ctx context.Context, tenantID uuid.UUID) (*time.Location, error) {
	var loc *time.Location
	err := r.s.inTenant(ctx, tenantID, func(b base) error {
		var err error
		loc, err = b.TenantLocation(ctx)
		return err
	})
	return loc, err
}

// Raw lê as agregações do mês [w.From, w.To) e as posições em w.PositionAt.
func (r *Reporting) Raw(ctx context.Context, tenantID uuid.UUID, w reporting.Window) (reporting.Raw, error) {
	from, to, positionAt := w.From, w.To, w.PositionAt
	var raw reporting.Raw
	err := r.s.inTenant(ctx, tenantID, func(b base) error {
		comps, err := b.q.DashboardMRRComponents(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, c := range comps {
			gross, err := ratFromNumeric(c.GrossTotal)
			if err != nil {
				return err
			}
			discount, err := ratFromNumeric(c.DiscountTotal)
			if err != nil {
				return err
			}
			raw.MRR = append(raw.MRR, reporting.MRRComponent{Interval: calendar.Interval(c.BillingInterval), Gross: gross, Discount: discount})
		}
		billed, err := b.q.DashboardBilled(ctx, dbgen.DashboardBilledParams{TenantID: tenantID, FromTs: &from, ToTs: &to})
		if err != nil {
			return err
		}
		raw.Billed, raw.BilledRecurring, raw.BilledUsage = billed.Billed, billed.BilledRecurring, billed.BilledUsage
		if raw.Received, err = b.q.DashboardReceived(ctx, dbgen.DashboardReceivedParams{TenantID: tenantID, FromTs: from, ToTs: to}); err != nil {
			return err
		}
		since := positionAt.AddDate(0, 0, -90)
		if err := r.receivables(ctx, b, tenantID, w.Current, positionAt, since, &raw); err != nil {
			return err
		}
		if raw.DelinquencyBase, err = b.q.DashboardDelinquencyBase(ctx, dbgen.DashboardDelinquencyBaseParams{TenantID: tenantID,
			PositionAt: &positionAt, Since: &since}); err != nil {
			return err
		}
		counts, err := b.q.DashboardSubscriptionCounts(ctx, dbgen.DashboardSubscriptionCountsParams{TenantID: tenantID, FromTs: from, ToTs: &to,
			PositionAt: &positionAt})
		if err != nil {
			return err
		}
		raw.ActiveAtPosition, raw.StartedInPeriod = int(counts.ActiveAtPosition), int(counts.StartedInPeriod)
		raw.EndedInPeriod, raw.ActiveAtStart, raw.ChurnedInPeriod = int(counts.EndedInPeriod), int(counts.ActiveAtStart), int(counts.ChurnedInPeriod)
		contra, err := b.q.DashboardContraRevenue(ctx, dbgen.DashboardContraRevenueParams{TenantID: tenantID, FromTs: from, ToTs: to})
		if err != nil {
			return err
		}
		raw.Refunds, raw.Chargebacks = contra.Refunds, contra.Chargebacks
		if raw.OpenRecoverables, err = b.q.DashboardRecoverables(ctx, dbgen.DashboardRecoverablesParams{TenantID: tenantID,
			PositionAt: positionAt}); err != nil {
			return err
		}
		unknown, err := b.q.DashboardUnknownOperations(ctx, tenantID)
		if err != nil {
			return err
		}
		raw.UnknownOperations = int(unknown)
		discrepancies, err := b.q.DashboardOpenDiscrepancies(ctx, tenantID)
		raw.OpenDiscrepancies = int(discrepancies)
		return err
	})
	return raw, err
}

// receivables lê os recebíveis na posição: no mês corrente, pelo saldo das
// faturas em aberto; num mês encerrado, pelo ledger, restrito às faturas que
// podiam ter saldo na posição. No instante atual as duas formas coincidem.
func (r *Reporting) receivables(ctx context.Context, b base, tenantID uuid.UUID, current bool, positionAt, since time.Time, raw *reporting.Raw) error {
	if current {
		rec, err := b.q.DashboardReceivablesCurrent(ctx, dbgen.DashboardReceivablesCurrentParams{TenantID: tenantID, PositionAt: &positionAt,
			Since: &since})
		raw.OpenReceivables, raw.OverdueReceivables, raw.DelinquencyOpen = rec.OpenTotal, rec.OverdueTotal, rec.DelinquencyOpen
		return err
	}
	rec, err := b.q.DashboardReceivablesAt(ctx, dbgen.DashboardReceivablesAtParams{TenantID: tenantID, PositionAt: &positionAt, Since: &since})
	raw.OpenReceivables, raw.OverdueReceivables, raw.DelinquencyOpen = rec.OpenTotal, rec.OverdueTotal, rec.DelinquencyOpen
	return err
}
