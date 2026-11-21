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

// Raw lê as agregações do período.
func (r *Reporting) Raw(ctx context.Context, tenantID uuid.UUID, from, to, now time.Time) (reporting.Raw, error) {
	var raw reporting.Raw
	err := r.s.inTenant(ctx, tenantID, func(b base) error {
		comps, err := b.q.DashboardMRRComponents(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, c := range comps {
			pu, err := ratFromNumeric(c.PerUnitTotal)
			if err != nil {
				return err
			}
			raw.MRR = append(raw.MRR, reporting.MRRComponent{Interval: calendar.Interval(c.BillingInterval), Model: c.PricingModel,
				FlatTotal: c.FlatTotal, PerUnitTotal: pu})
		}
		billed, err := b.q.DashboardBilled(ctx, dbgen.DashboardBilledParams{TenantID: tenantID, FromTs: &from, ToTs: &to})
		if err != nil {
			return err
		}
		raw.Billed, raw.BilledRecurring, raw.BilledUsage = billed.Billed, billed.BilledRecurring, billed.BilledUsage
		if raw.Received, err = b.q.DashboardReceived(ctx, dbgen.DashboardReceivedParams{TenantID: tenantID, FromTs: from, ToTs: to}); err != nil {
			return err
		}
		rec, err := b.q.DashboardReceivables(ctx, dbgen.DashboardReceivablesParams{TenantID: tenantID, Now: &now})
		if err != nil {
			return err
		}
		raw.OpenReceivables, raw.OverdueReceivables = rec.OpenTotal, rec.OverdueTotal
		since := now.AddDate(0, 0, -90)
		if raw.DelinquencyBase, err = b.q.DashboardDelinquencyBase(ctx, dbgen.DashboardDelinquencyBaseParams{TenantID: tenantID, Since: &since, Now: &now}); err != nil {
			return err
		}
		if raw.DelinquencyOpen, err = b.q.DashboardDelinquencyOpen(ctx, dbgen.DashboardDelinquencyOpenParams{TenantID: tenantID, Since: &since, Now: &now}); err != nil {
			return err
		}
		counts, err := b.q.DashboardSubscriptionCounts(ctx, dbgen.DashboardSubscriptionCountsParams{TenantID: tenantID, FromTs: from, ToTs: to})
		if err != nil {
			return err
		}
		raw.ActiveNow, raw.CreatedInPeriod = int(counts.ActiveNow), int(counts.CreatedInPeriod)
		raw.EndedInPeriod, raw.ActiveAtStart = int(counts.EndedInPeriod), int(counts.ActiveAtStart)
		contra, err := b.q.DashboardContraRevenue(ctx, dbgen.DashboardContraRevenueParams{TenantID: tenantID, FromTs: from, ToTs: to})
		if err != nil {
			return err
		}
		raw.Refunds, raw.Chargebacks = contra.Refunds, contra.Chargebacks
		if raw.OpenRecoverables, err = b.q.DashboardRecoverables(ctx, tenantID); err != nil {
			return err
		}
		unknown, err := b.q.DashboardUnknownOperations(ctx, tenantID)
		raw.UnknownOperations = int(unknown)
		return err
	})
	return raw, err
}
