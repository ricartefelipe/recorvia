package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// --- atendimento dos serviços adicionais pela equipe da plataforma ---

func staffAddonOrderDTO(o platformbilling.StaffAddonOrder) map[string]any {
	out := addonOrderDTO(o.AddonOrder)
	out["tenant_id"], out["tenant_name"], out["tenant_slug"] = o.TenantID, o.TenantName, o.TenantSlug
	name := o.AddonName
	if name == "" {
		name = o.AddonCode
	}
	out["addon_name"] = name
	return out
}

func (a *API) staff(r *http.Request) platformbilling.Staff {
	p := mustPrincipal(r)
	return platformbilling.Staff{UserID: p.User.ID, DisplayName: p.User.DisplayName}
}

func (a *API) adminAddonOrders(w http.ResponseWriter, r *http.Request) {
	limit, err := queryLimit(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	pg, err := a.Platform.StaffAddonOrders(r.Context(), q.Get("status"), q.Get("cursor"), limit)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(pg.Items))
	for _, o := range pg.Items {
		out = append(out, staffAddonOrderDTO(o))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, pg.NextCursor))
}

// adminAddonOrderTarget lê o tenant e o pedido do caminho.
func adminAddonOrderTarget(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	tenantID, err := pathUUID(r, "tenant_id")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id, err := pathUUID(r, "id")
	return tenantID, id, err
}

func (a *API) adminScheduleAddonOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, id, err := adminAddonOrderTarget(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		ScheduledFor time.Time `json:"scheduled_for"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	o, err := a.Platform.ScheduleAddonOrder(r.Context(), a.staff(r), tenantID, id, in.ScheduledFor)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, addonOrderDTO(o))
}

func (a *API) adminCompleteAddonOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, id, err := adminAddonOrderTarget(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	o, err := a.Platform.CompleteAddonOrder(r.Context(), a.staff(r), tenantID, id)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, addonOrderDTO(o))
}

func (a *API) adminCancelAddonOrder(w http.ResponseWriter, r *http.Request) {
	tenantID, id, err := adminAddonOrderTarget(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	o, err := a.Platform.CancelAddonOrder(r.Context(), a.staff(r), tenantID, id, in.Reason)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusOK, addonOrderDTO(o))
}
