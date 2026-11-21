package platformbilling

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

func TestParseAddonStatus(t *testing.T) {
	for _, s := range []string{"requested", "scheduled", "completed", "canceled"} {
		if got, err := ParseAddonStatus(s); err != nil || string(got) != s {
			t.Errorf("%s: %v %v", s, got, err)
		}
	}
	_, err := ParseAddonStatus("paid")
	assertAppErr(t, err, apperr.KindInvalid, "invalid_filter")
}

func TestAddonOrderTransitions(t *testing.T) {
	now := utc(2026, time.November, 20, 12, 0)
	staff := uuid.New()
	open := func(st AddonStatus) AddonOrder {
		return AddonOrder{ID: uuid.New(), Status: st, Price: money.New(250000, money.BRL), Version: 1}
	}

	t.Run("agendar e reagendar", func(t *testing.T) {
		o := open(AddonRequested)
		at := now.Add(72 * time.Hour).In(time.FixedZone("BRT", -3*3600))
		if err := o.Schedule(at, now, staff); err != nil {
			t.Fatal(err)
		}
		if o.Status != AddonScheduled || o.ScheduledFor == nil || !o.ScheduledFor.Equal(at) || o.ScheduledFor.Location() != time.UTC ||
			o.HandledBy == nil || *o.HandledBy != staff {
			t.Fatalf("agendamento inesperado: %+v", o)
		}
		later := now.Add(96 * time.Hour)
		if err := o.Schedule(later, now, staff); err != nil || !o.ScheduledFor.Equal(later) {
			t.Fatalf("reagendamento: %v %v", err, o.ScheduledFor)
		}
	})
	t.Run("data de atendimento inválida", func(t *testing.T) {
		for name, at := range map[string]time.Time{
			"ausente": {}, "no passado": now.Add(-time.Minute), "depois de um ano": now.Add(MaxScheduleAhead + time.Minute),
		} {
			o := open(AddonRequested)
			err := o.Schedule(at, now, staff)
			assertAppErr(t, err, apperr.KindInvalid, "invalid_addon_schedule")
			if o.Status != AddonRequested || o.ScheduledFor != nil {
				t.Fatalf("%s: o pedido não pode mudar: %+v", name, o)
			}
		}
	})
	t.Run("concluir com ou sem agendamento", func(t *testing.T) {
		for _, st := range []AddonStatus{AddonRequested, AddonScheduled} {
			o := open(st)
			if err := o.Complete(now, staff); err != nil {
				t.Fatal(err)
			}
			if o.Status != AddonCompleted || o.CompletedAt == nil || !o.CompletedAt.Equal(now) || *o.HandledBy != staff {
				t.Fatalf("conclusão inesperada: %+v", o)
			}
		}
	})
	t.Run("cancelar exige justificativa", func(t *testing.T) {
		o := open(AddonScheduled)
		for _, reason := range []string{"", "  ok  ", strings.Repeat("a", 501)} {
			err := o.Cancel(reason, now, staff)
			assertAppErr(t, err, apperr.KindInvalid, "invalid_addon_cancel")
		}
		if err := o.Cancel("  Cliente desistiu da implantação.  ", now, staff); err != nil {
			t.Fatal(err)
		}
		if o.Status != AddonCanceled || o.CanceledAt == nil || o.CancelReason != "Cliente desistiu da implantação." {
			t.Fatalf("cancelamento inesperado: %+v", o)
		}
	})
	t.Run("pedidos encerrados não mudam", func(t *testing.T) {
		for _, st := range []AddonStatus{AddonCompleted, AddonCanceled} {
			o := open(st)
			if o.Open() {
				t.Fatalf("%s não está em aberto", st)
			}
			for name, err := range map[string]error{
				"agendar":  o.Schedule(now.Add(time.Hour), now, staff),
				"concluir": o.Complete(now, staff),
				"cancelar": o.Cancel("motivo válido", now, staff),
			} {
				assertAppErr(t, err, apperr.KindConflict, "addon_order_closed")
				if o.Status != st {
					t.Fatalf("%s/%s alterou o pedido: %+v", st, name, o)
				}
			}
		}
	})
}

func staffFixture(t *testing.T, status AddonStatus) (*fixture, AddonOrder, Staff) {
	t.Helper()
	f := newFixture(t)
	o := AddonOrder{ID: uuid.New(), TenantID: f.tenantID, AddonCode: "implantacao_assistida", Status: status,
		Price: money.New(250000, money.BRL), RequestedBy: uuid.New(), CreatedAt: f.clock.Now().Add(-48 * time.Hour), Version: 1}
	f.state().orders = []AddonOrder{o}
	return f, o, Staff{UserID: uuid.New(), DisplayName: "Equipe Recorvia"}
}

func TestStaffAddonOrders(t *testing.T) {
	f := newFixture(t)
	base := utc(2026, time.November, 1, 12, 0)
	for i := range 3 {
		done := base.Add(time.Duration(i) * time.Hour)
		f.store.staff = append(f.store.staff, StaffAddonOrder{AddonOrder: AddonOrder{ID: uuid.New(), CreatedAt: base.Add(-time.Hour),
			CompletedAt: &done}, TenantName: "Acme"})
	}
	pg, err := f.svc.StaffAddonOrders(context.Background(), "", "", 0)
	if err != nil || len(pg.Items) != 3 || pg.NextCursor != "" || f.store.staffQuery.Status != "" ||
		f.store.staffQuery.Limit != DefaultStaffPageSize+1 || f.store.staffQuery.After != nil {
		t.Fatalf("sem filtro lista os pedidos em aberto: %+v %v %+v", pg, err, f.store.staffQuery)
	}
	// Página menor que o total: o cursor aponta para o último item, pelo
	// instante de ordenação do filtro.
	pg, err = f.svc.StaffAddonOrders(context.Background(), "completed", "", 2)
	if err != nil || len(pg.Items) != 2 || pg.NextCursor == "" {
		t.Fatalf("página: %+v %v", pg, err)
	}
	cur, err := ParseAddonCursor(AddonCompleted, pg.NextCursor)
	if err != nil || cur.ID != pg.Items[1].ID || !cur.At.Equal(*pg.Items[1].CompletedAt) {
		t.Fatalf("cursor: %+v %v", cur, err)
	}
	if _, err := f.svc.StaffAddonOrders(context.Background(), "completed", pg.NextCursor, 500); err != nil ||
		f.store.staffQuery.After == nil || f.store.staffQuery.After.ID != cur.ID || f.store.staffQuery.Limit != MaxStaffPageSize+1 {
		t.Fatalf("página seguinte: %v %+v", err, f.store.staffQuery)
	}
	// O cursor de um filtro não vale em outro.
	_, err = f.svc.StaffAddonOrders(context.Background(), "canceled", pg.NextCursor, 2)
	assertAppErr(t, err, apperr.KindInvalid, "invalid_cursor")
	_, err = f.svc.StaffAddonOrders(context.Background(), "pago", "", 0)
	assertAppErr(t, err, apperr.KindInvalid, "invalid_filter")
	f.store.fail["StaffAddonOrders"] = errBoom
	if _, err := f.svc.StaffAddonOrders(context.Background(), "", "", 0); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
}

func TestAddonCursorAndSortKey(t *testing.T) {
	at := utc(2026, time.November, 3, 10, 0).Add(1500 * time.Nanosecond)
	o := AddonOrder{ID: uuid.New(), CreatedAt: utc(2026, time.November, 1, 0, 0), ScheduledFor: &at, CompletedAt: &at, CanceledAt: &at}
	for _, st := range []AddonStatus{"", AddonRequested} {
		if !o.SortKey(st).Equal(o.CreatedAt) {
			t.Fatalf("%q ordena pela solicitação", st)
		}
	}
	for _, st := range []AddonStatus{AddonScheduled, AddonCompleted, AddonCanceled} {
		if !o.SortKey(st).Equal(at) {
			t.Fatalf("%s ordena pelo próprio instante", st)
		}
	}
	if (AddonOrder{CreatedAt: o.CreatedAt}).SortKey(AddonCompleted) != o.CreatedAt {
		t.Fatal("sem o instante, usa a solicitação")
	}
	c := EncodeAddonCursor(AddonScheduled, AddonCursor{At: at, ID: o.ID})
	got, err := ParseAddonCursor(AddonScheduled, c)
	if err != nil || got.ID != o.ID || !got.At.Equal(at.Truncate(time.Microsecond)) {
		t.Fatalf("cursor: %+v %v", got, err)
	}
	if got, err := ParseAddonCursor("", ""); got != nil || err != nil {
		t.Fatal("cursor vazio é a primeira página")
	}
	for _, bad := range []string{"!!", strings.Repeat("a", 161), "YWJj", c + "x",
		base64.RawURLEncoding.EncodeToString([]byte("ao-scheduled|ontem|" + o.ID.String())),
		base64.RawURLEncoding.EncodeToString([]byte("ao-scheduled|2026-11-03T10:00:00Z|nao-e-uuid")),
		base64.RawURLEncoding.EncodeToString([]byte("ao-scheduled|2026-11-03T10:00:00.000000Z|" + o.ID.String()))} {
		if _, err := ParseAddonCursor(AddonScheduled, bad); err == nil {
			t.Fatalf("cursor inválido aceito: %q", bad)
		}
	}
}

// Sem assinatura da plataforma ativa não há demonstrativo a fechar: a
// conclusão é recusada, e o pedido continua em aberto para ser cancelado.
func TestCompleteAddonOrderRefusedWithCanceledPlatformSubscription(t *testing.T) {
	f, o, staff := staffFixture(t, AddonScheduled)
	f.state().sub.Status = StatusCanceled
	_, err := f.svc.CompleteAddonOrder(context.Background(), staff, f.tenantID, o.ID)
	assertAppErr(t, err, apperr.KindConflict, "platform_subscription_canceled")
	if st := f.state(); st.orders[0].Status != AddonScheduled || st.orderUpdates != 0 {
		t.Fatal("o pedido não pode ser concluído")
	}
	if _, err := f.svc.CancelAddonOrder(context.Background(), staff, f.tenantID, o.ID, "Organização encerrou o contrato."); err != nil {
		t.Fatalf("o cancelamento continua possível: %v", err)
	}
}

func TestStaffHandlesAddonOrder(t *testing.T) {
	at := utc(2026, time.November, 25, 13, 0)
	cases := []struct {
		name   string
		run    func(f *fixture, o AddonOrder, st Staff) (AddonOrder, error)
		action string
		status AddonStatus
		detail string
	}{
		{"agendar", func(f *fixture, o AddonOrder, st Staff) (AddonOrder, error) {
			return f.svc.ScheduleAddonOrder(context.Background(), st, f.tenantID, o.ID, at)
		}, "platform.addon_scheduled", AddonScheduled, "scheduled_for"},
		{"concluir", func(f *fixture, o AddonOrder, st Staff) (AddonOrder, error) {
			return f.svc.CompleteAddonOrder(context.Background(), st, f.tenantID, o.ID)
		}, "platform.addon_completed", AddonCompleted, ""},
		{"cancelar", func(f *fixture, o AddonOrder, st Staff) (AddonOrder, error) {
			return f.svc.CancelAddonOrder(context.Background(), st, f.tenantID, o.ID, "Cliente desistiu.")
		}, "platform.addon_canceled", AddonCanceled, "reason"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, o, staff := staffFixture(t, AddonRequested)
			got, err := tc.run(f, o, staff)
			if err != nil {
				t.Fatal(err)
			}
			st := f.state()
			if got.Status != tc.status || got.Version != 2 || st.orders[0].Status != tc.status || st.orderUpdates != 1 {
				t.Fatalf("pedido inesperado: %+v", got)
			}
			if len(st.audits) != 1 {
				t.Fatalf("esperada uma auditoria, obtidas %d", len(st.audits))
			}
			e := st.audits[0]
			if e.Action != tc.action || e.Actor != audit.User(staff.UserID, staff.DisplayName) || e.TenantID != f.tenantID ||
				e.ResourceType != "platform_addon_order" || e.ResourceID != o.ID.String() || e.Details["platform_staff"] != true ||
				e.Details["from"] != AddonRequested || e.Details["to"] != tc.status || !e.OccurredAt.Equal(f.clock.Now()) {
				t.Fatalf("auditoria inesperada: %+v", e)
			}
			if tc.detail != "" && e.Details[tc.detail] == nil {
				t.Fatalf("auditoria sem %s: %+v", tc.detail, e.Details)
			}
		})
	}
}

func TestStaffHandleAddonOrderRejected(t *testing.T) {
	f, o, staff := staffFixture(t, AddonCompleted)
	_, err := f.svc.CancelAddonOrder(context.Background(), staff, f.tenantID, o.ID, "Cliente desistiu.")
	assertAppErr(t, err, apperr.KindConflict, "addon_order_closed")
	_, err = f.svc.CompleteAddonOrder(context.Background(), staff, f.tenantID, uuid.New())
	assertAppErr(t, err, apperr.KindNotFound, "addon_order_not_found")
	if st := f.state(); st.orderUpdates != 0 || len(st.audits) != 0 {
		t.Fatal("nada deveria ser gravado")
	}

	for _, op := range []string{"InTenant", "Subscription", "AddonOrder", "UpdateAddonOrder", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			f, o, staff := staffFixture(t, AddonScheduled)
			f.store.fail[op] = errBoom
			if _, err := f.svc.CompleteAddonOrder(context.Background(), staff, f.tenantID, o.ID); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
		})
	}
}

// O pedido concluído é cobrado uma única vez: no demonstrativo do período da
// conclusão ou, se a conclusão ocorreu depois do fechamento desse período
// (por exemplo, com o agendador atrasado), no seguinte.
func TestCompletedAddonIsBilledOnce(t *testing.T) {
	f := dueFixture(t) // três períodos encerrados: agosto, setembro e outubro
	staff := Staff{UserID: uuid.New(), DisplayName: "Equipe"}
	o := AddonOrder{ID: uuid.New(), TenantID: f.tenantID, AddonCode: "implantacao_assistida", Status: AddonScheduled,
		Price: money.New(250000, money.BRL), Version: 1}
	f.state().orders = []AddonOrder{o}
	if _, err := f.svc.CompleteAddonOrder(context.Background(), staff, f.tenantID, o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.CloseDueStatements(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := f.state()
	var charged int64
	for _, s := range st.statements {
		charged += s.AddonsAmount.Amount()
	}
	// Concluído agora (depois do fim do último período fechado): fica para o
	// demonstrativo do período corrente.
	if charged != 0 || st.orders[0].StatementID != nil {
		t.Fatalf("conclusão posterior aos períodos fechados não entra neles: %d", charged)
	}
	f.clock.Set(st.sub.PeriodEnd.Add(time.Minute))
	f.store.due = []uuid.UUID{f.tenantID}
	if _, err := f.svc.CloseDueStatements(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := st.statements[len(st.statements)-1]
	if last.AddonsAmount.Amount() != 250000 || last.Total.Amount() != 19900+250000 || st.orders[0].StatementID == nil ||
		*st.orders[0].StatementID != last.ID {
		t.Fatalf("o pedido deve entrar no demonstrativo seguinte à conclusão: %+v", last)
	}
	f.clock.Set(st.sub.PeriodEnd.Add(time.Minute))
	if _, err := f.svc.CloseDueStatements(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again := st.statements[len(st.statements)-1]; again.ID == last.ID || again.AddonsAmount.Amount() != 0 {
		t.Fatalf("o pedido não pode ser cobrado duas vezes: %+v", again)
	}
}
