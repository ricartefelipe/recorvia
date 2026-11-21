package platformbilling

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Addon é um serviço adicional ofertado.
type Addon struct {
	Code        string
	Name        string
	Description string
	Price       money.Money
	Kind        string
	Active      bool
}

// Tx são as operações de persistência dentro da transação do tenant.
type Tx interface {
	Subscription(ctx context.Context) (Subscription, error)
	UpdateSubscription(ctx context.Context, s Subscription) error
	Plan(ctx context.Context, code string) (Plan, error)
	Addon(ctx context.Context, code string) (Addon, error)
	Usage(ctx context.Context) (Usage, error)
	PendingInvitations(ctx context.Context, now time.Time) (int, error)
	InsertAddonOrder(ctx context.Context, o AddonOrder) error
	// AddonOrder lê o pedido com bloqueio.
	AddonOrder(ctx context.Context, id uuid.UUID) (AddonOrder, error)
	UpdateAddonOrder(ctx context.Context, o AddonOrder) error
	// UnbilledAddonsTotal soma os pedidos concluídos antes de before que
	// ainda não entraram em demonstrativo.
	UnbilledAddonsTotal(ctx context.Context, before time.Time) (money.Money, error)
	// MarkAddonsBilled vincula esses pedidos ao demonstrativo gravado.
	MarkAddonsBilled(ctx context.Context, statementID uuid.UUID, before time.Time) error
	InsertStatement(ctx context.Context, s Statement) (bool, error)
	TenantLocation(ctx context.Context) (*time.Location, error)
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações de tenant e lê o catálogo global de planos.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	Plans(ctx context.Context) ([]Plan, error)
	Addons(ctx context.Context) ([]Addon, error)
	TenantsDueStatement(ctx context.Context, now time.Time) ([]uuid.UUID, error)
	// StaffAddonOrders lista pedidos de todas as organizações (equipe da
	// plataforma) na ordem do filtro, depois do cursor.
	StaffAddonOrders(ctx context.Context, q StaffAddonQuery) ([]StaffAddonOrder, error)
}

// Service expõe os casos de uso do faturamento da plataforma.
type Service struct {
	store Store
	clock clock.Clock
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// Overview reúne plano, assinatura e consumo de limites do tenant.
type Overview struct {
	Plan         Plan
	Subscription Subscription
	Usage        Usage
}

// Overview retorna a situação do tenant na plataforma.
func (s *Service) Overview(ctx context.Context, tenantID uuid.UUID) (Overview, error) {
	var out Overview
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx)
		if err != nil {
			return err
		}
		plan, err := tx.Plan(ctx, sub.PlanCode)
		if err != nil {
			return err
		}
		usage, err := s.usage(ctx, tx)
		if err != nil {
			return err
		}
		out = Overview{Plan: plan, Subscription: sub, Usage: usage}
		return nil
	})
	return out, err
}

// usage lê o consumo, incluindo os convites pendentes que reservam vagas.
func (s *Service) usage(ctx context.Context, tx Tx) (Usage, error) {
	usage, err := tx.Usage(ctx)
	if err != nil {
		return Usage{}, err
	}
	usage.PendingInvitations, err = tx.PendingInvitations(ctx, s.clock.Now())
	return usage, err
}

// Plans lista os planos disponíveis.
func (s *Service) Plans(ctx context.Context) ([]Plan, error) { return s.store.Plans(ctx) }

// Addons lista os serviços adicionais.
func (s *Service) Addons(ctx context.Context) ([]Addon, error) { return s.store.Addons(ctx) }

// errSubscriptionVersion recusa a troca de plano com versão da assinatura
// diferente da informada no If-Match.
var errSubscriptionVersion = apperr.PreconditionFailed("version_mismatch",
	"A assinatura da plataforma foi alterada por outra pessoa. Recarregue e tente novamente.")

// ChangePlan troca o plano da plataforma na versão esperada da assinatura
// (If-Match), validando o consumo atual (membros ativos, convites pendentes e
// contratos ativos) contra os limites do plano de destino. A mudança vale a
// partir do período corrente. A leitura da assinatura bloqueia a linha:
// convites, aceites e criações de assinatura leem o plano com o mesmo
// bloqueio, de modo que nenhum deles conta o consumo durante a troca. Com a
// versão divergente nada é gravado nem auditado (412), inclusive na troca
// para o plano atual, que não tem efeito.
func (s *Service) ChangePlan(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, planCode string, expectedVersion int) (Subscription, error) {
	var out Subscription
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx)
		if err != nil {
			return err
		}
		if sub.Version != expectedVersion {
			return errSubscriptionVersion
		}
		if sub.PlanCode == planCode {
			out = sub
			return nil
		}
		target, err := tx.Plan(ctx, planCode)
		if err != nil {
			return err
		}
		usage, err := s.usage(ctx, tx)
		if err != nil {
			return err
		}
		if err := CanChangeTo(target, usage); err != nil {
			return err
		}
		previous := sub.PlanCode
		sub.PlanCode, sub.Version = target.Code, expectedVersion
		if err := tx.UpdateSubscription(ctx, sub); err != nil {
			return err
		}
		sub.Version++
		out = sub
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actor, "platform.plan_changed", "platform_subscription", tenantID.String(),
			map[string]any{"from": previous, "to": target.Code}))
	})
	return out, err
}

// RequestAddon registra o pedido de serviço adicional.
func (s *Service) RequestAddon(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, requester uuid.UUID, code, notes string) (AddonOrder, error) {
	if len([]rune(notes)) > 2000 {
		return AddonOrder{}, apperr.Invalid("addon_notes_too_long", "As observações devem ter no máximo 2.000 caracteres.")
	}
	var out AddonOrder
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		addon, err := tx.Addon(ctx, code)
		if err != nil {
			return err
		}
		if !addon.Active {
			return apperr.Invalid("addon_inactive", "Serviço adicional indisponível.")
		}
		out = AddonOrder{
			ID: ids.New(), TenantID: tenantID, AddonCode: addon.Code, Status: AddonRequested, Notes: notes,
			Price: addon.Price, RequestedBy: requester, CreatedAt: s.clock.Now(), Version: 1,
		}
		if err := tx.InsertAddonOrder(ctx, out); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, out.CreatedAt, actor, "platform.addon_requested", "platform_addon_order", out.ID.String(),
			map[string]any{"addon": addon.Code}))
	})
	return out, err
}

// CloseDueStatements fecha os demonstrativos de períodos encerrados e avança
// as assinaturas. É idempotente: um demonstrativo por tenant e período.
func (s *Service) CloseDueStatements(ctx context.Context) (int, error) {
	now := s.clock.Now()
	tenants, err := s.store.TenantsDueStatement(ctx, now)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, tenantID := range tenants {
		if err := ctx.Err(); err != nil {
			return closed, err
		}
		inTenant := 0
		err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
			inTenant = 0
			loc, err := tx.TenantLocation(ctx)
			if err != nil {
				return err
			}
			sub, err := tx.Subscription(ctx)
			if err != nil {
				return err
			}
			// Recupera períodos atrasados (agendador indisponível), limitando
			// o trabalho por transação.
			for i := 0; i < 24 && !sub.PeriodEnd.After(now) && sub.Status != StatusCanceled; i++ {
				plan, err := tx.Plan(ctx, sub.PlanCode)
				if err != nil {
					return err
				}
				// Pedidos concluídos antes do fim do período e ainda não
				// cobrados (inclusive os concluídos em período já fechado,
				// por atraso entre a conclusão e o fechamento).
				addons, err := tx.UnbilledAddonsTotal(ctx, sub.PeriodEnd)
				if err != nil {
					return err
				}
				st, err := CloseStatement(ids.New(), sub, plan, addons, now)
				if err != nil {
					return err
				}
				inserted, err := tx.InsertStatement(ctx, st)
				if err != nil {
					return err
				}
				if inserted {
					inTenant++
					if err := tx.MarkAddonsBilled(ctx, st.ID, sub.PeriodEnd); err != nil {
						return err
					}
				}
				if err := sub.Advance(loc); err != nil {
					return err
				}
			}
			return tx.UpdateSubscription(ctx, sub)
		})
		if err != nil {
			return closed, err
		}
		closed += inTenant // contados somente após o commit
	}
	return closed, nil
}

// StaffAddonOrders lista os pedidos de serviços adicionais de todas as
// organizações para a equipe da plataforma, paginados por cursor. Sem
// filtro, lista os pedidos em aberto (solicitados e agendados), do mais
// antigo para o mais recente; agendados, pela data do atendimento;
// concluídos e cancelados, do encerramento mais recente para o mais antigo.
func (s *Service) StaffAddonOrders(ctx context.Context, status, cursor string, limit int) (StaffAddonPage, error) {
	var st AddonStatus
	if status != "" {
		var err error
		if st, err = ParseAddonStatus(status); err != nil {
			return StaffAddonPage{}, err
		}
	}
	after, err := ParseAddonCursor(st, cursor)
	if err != nil {
		return StaffAddonPage{}, err
	}
	if limit <= 0 {
		limit = DefaultStaffPageSize
	}
	limit = min(limit, MaxStaffPageSize)
	items, err := s.store.StaffAddonOrders(ctx, StaffAddonQuery{Status: st, After: after, Limit: limit + 1})
	if err != nil {
		return StaffAddonPage{}, err
	}
	page := StaffAddonPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = EncodeAddonCursor(st, AddonCursor{At: last.SortKey(st), ID: last.ID})
	}
	return page, nil
}

// Staff identifica o integrante da equipe da plataforma que atende o pedido.
type Staff struct {
	UserID      uuid.UUID
	DisplayName string
}

func (st Staff) actor() audit.Actor { return audit.User(st.UserID, st.DisplayName) }

// ScheduleAddonOrder agenda o atendimento do pedido na versão esperada
// (If-Match).
func (s *Service) ScheduleAddonOrder(ctx context.Context, staff Staff, tenantID, orderID uuid.UUID, at time.Time,
	expectedVersion int) (AddonOrder, error) {
	return s.handleAddonOrder(ctx, staff, tenantID, orderID, expectedVersion, "platform.addon_scheduled", func(o *AddonOrder, _ Subscription, now time.Time) error {
		return o.Schedule(at, now, staff.UserID)
	})
}

// CompleteAddonOrder conclui o atendimento na versão esperada (If-Match): o
// valor do pedido entra no demonstrativo do período em que a conclusão
// ocorreu (ou no seguinte, se esse já estiver fechado). Com a assinatura da
// plataforma cancelada não há demonstrativo a fechar: a conclusão é recusada
// (o pedido é cancelado), para que nenhum pedido fique concluído sem
// cobrança.
func (s *Service) CompleteAddonOrder(ctx context.Context, staff Staff, tenantID, orderID uuid.UUID, expectedVersion int) (AddonOrder, error) {
	return s.handleAddonOrder(ctx, staff, tenantID, orderID, expectedVersion, "platform.addon_completed", func(o *AddonOrder, sub Subscription, now time.Time) error {
		if sub.Status == StatusCanceled {
			return apperr.Conflict("platform_subscription_canceled",
				"A assinatura da plataforma desta organização está cancelada: o serviço não pode ser concluído nem cobrado. Cancele o pedido.")
		}
		return o.Complete(now, staff.UserID)
	})
}

// CancelAddonOrder cancela o pedido em aberto, sem cobrança, na versão
// esperada (If-Match).
func (s *Service) CancelAddonOrder(ctx context.Context, staff Staff, tenantID, orderID uuid.UUID, reason string,
	expectedVersion int) (AddonOrder, error) {
	return s.handleAddonOrder(ctx, staff, tenantID, orderID, expectedVersion, "platform.addon_canceled", func(o *AddonOrder, _ Subscription, now time.Time) error {
		return o.Cancel(reason, now, staff.UserID)
	})
}

// errAddonOrderVersion recusa a transição do pedido com versão diferente da
// informada no If-Match.
var errAddonOrderVersion = apperr.PreconditionFailed("version_mismatch",
	"O pedido foi alterado por outra pessoa. Recarregue e tente novamente.")

// handleAddonOrder aplica a transição na transação do tenant do pedido, com
// auditoria na trilha do tenant. A assinatura da plataforma é bloqueada
// antes do pedido, na mesma ordem do fechamento do demonstrativo: a
// conclusão e o fechamento não se cruzam, e o pedido concluído é cobrado
// exatamente uma vez. O pedido bloqueado precisa estar na versão esperada
// (If-Match), que também condiciona a gravação: divergente, nada é gravado
// nem auditado (412).
func (s *Service) handleAddonOrder(ctx context.Context, staff Staff, tenantID, orderID uuid.UUID, expectedVersion int, action string,
	apply func(o *AddonOrder, sub Subscription, now time.Time) error) (AddonOrder, error) {
	var out AddonOrder
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		sub, err := tx.Subscription(ctx)
		if err != nil {
			return err
		}
		o, err := tx.AddonOrder(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Version != expectedVersion {
			return errAddonOrderVersion
		}
		now := s.clock.Now()
		from := o.Status
		if err := apply(&o, sub, now); err != nil {
			return err
		}
		if err := tx.UpdateAddonOrder(ctx, o); err != nil {
			return err
		}
		o.Version++
		out = o
		details := map[string]any{"addon": o.AddonCode, "from": from, "to": o.Status, "platform_staff": true}
		if o.ScheduledFor != nil && o.Status == AddonScheduled {
			details["scheduled_for"] = *o.ScheduledFor
		}
		if o.Status == AddonCanceled {
			details["reason"] = o.CancelReason
		}
		return tx.Audit(ctx, audit.New(tenantID, now, staff.actor(), action, "platform_addon_order", o.ID.String(), details))
	})
	return out, err
}
