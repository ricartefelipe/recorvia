package billing

import (
	"context"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/subscriptions"
)

// isCents informa se o valor exato r é igual a cents centavos.
func isCents(r *big.Rat, cents int64) bool { return r != nil && r.Cmp(big.NewRat(cents, 1)) == 0 }

func (s *cycleFake) PendingChange(_ context.Context, subID uuid.UUID) (*subscriptions.ScheduledChange, error) {
	if err := s.fail("PendingChange"); err != nil {
		return nil, err
	}
	if c := s.changes[subID]; c != nil && c.Status == subscriptions.ChangePending {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (s *cycleFake) ResolveChange(_ context.Context, id uuid.UUID, status subscriptions.ChangeStatus, at time.Time) error {
	if err := s.fail("ResolveChange"); err != nil {
		return err
	}
	s.resolved[id] = status
	for _, c := range s.changes {
		if c.ID == id {
			c.Status, c.ResolvedAt = status, &at
		}
	}
	return nil
}

func (s *cycleFake) DueAdjustments(_ context.Context, subID uuid.UUID, onDate calendar.Date) ([]subscriptions.PriceAdjustment, error) {
	if err := s.fail("DueAdjustments"); err != nil {
		return nil, err
	}
	var out []subscriptions.PriceAdjustment
	for _, a := range s.adjustments[subID] {
		if a.Status == subscriptions.AdjustmentScheduled && !onDate.Before(a.EffectiveDate) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *cycleFake) MarkAdjustmentApplied(_ context.Context, a subscriptions.PriceAdjustment) error {
	if err := s.fail("MarkAdjustmentApplied"); err != nil {
		return err
	}
	list := s.adjustments[a.SubscriptionID]
	for i := range list {
		if list[i].ID == a.ID {
			list[i] = a
		}
	}
	return nil
}

func (s *cycleFake) CancelAdjustment(_ context.Context, a subscriptions.PriceAdjustment) error {
	if err := s.fail("CancelAdjustment"); err != nil {
		return err
	}
	list := s.adjustments[a.SubscriptionID]
	for i := range list {
		if list[i].ID == a.ID && list[i].Status == subscriptions.AdjustmentScheduled {
			list[i] = a
		}
	}
	return nil
}

func (s *cycleFake) CancelScheduledAdjustments(_ context.Context, subID uuid.UUID, reason subscriptions.AdjustmentCancelReason,
	at time.Time) ([]uuid.UUID, error) {
	if err := s.fail("CancelScheduledAdjustments"); err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	list := s.adjustments[subID]
	for i := range list {
		if list[i].Status == subscriptions.AdjustmentScheduled {
			list[i].Status, list[i].CanceledAt, list[i].CancelReason = subscriptions.AdjustmentCanceled, &at, reason
			ids = append(ids, list[i].ID)
		}
	}
	return ids, nil
}

func (s *cycleFake) CancelPendingTerms(_ context.Context, subID uuid.UUID, _ time.Time) error {
	if err := s.fail("CancelPendingTerms"); err != nil {
		return err
	}
	s.termsCanceled = append(s.termsCanceled, subID)
	return nil
}

func (s *cycleFake) EndActiveDiscount(_ context.Context, subID uuid.UUID, _ time.Time) (bool, error) {
	if err := s.fail("EndActiveDiscount"); err != nil {
		return false, err
	}
	s.discountEnded = append(s.discountEnded, subID)
	ended := s.discount != nil
	s.discount = nil
	return ended, nil
}
