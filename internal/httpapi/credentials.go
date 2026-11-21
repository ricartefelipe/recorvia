package httpapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

type apiCredentialDTO struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Version    int        `json:"version"`
}

type apiCredentialWithSecretDTO struct {
	Credential apiCredentialDTO `json:"credential"`
	Secret     string           `json:"secret"`
}

func credentialDTO(c apicredentials.Credential) apiCredentialDTO {
	scopes := make([]string, len(c.Scopes))
	for i, s := range c.Scopes {
		scopes[i] = string(s)
	}
	return apiCredentialDTO{ID: c.ID, Name: c.Name, Prefix: c.Prefix(), Scopes: scopes, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt,
		LastUsedAt: c.LastUsedAt, RevokedAt: c.RevokedAt, Version: c.Version}
}

// grantor descreve o membro que gerencia credenciais. Credenciais de API não
// têm o escopo api_credentials.manage (não delegável); a verificação aqui é
// uma segunda barreira.
func grantor(p Principal) (apicredentials.Grantor, error) {
	if p.Credential != nil {
		return apicredentials.Grantor{}, apperr.Forbidden("user_session_required",
			"Credenciais de API são gerenciadas apenas por membros com sessão de usuário.")
	}
	return apicredentials.Grantor{UserID: p.User.ID, Role: p.Role, Actor: p.Actor()}, nil
}

// listAPICredentials lista as credenciais em páginas por cursor: ativas
// primeiro, depois revogadas e expiradas, das mais recentes para as mais
// antigas.
func (a *API) listAPICredentials(w http.ResponseWriter, r *http.Request) {
	limit, cursor, err := pageParams(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	pg, err := a.Credentials.List(r.Context(), mustPrincipal(r).TenantID, limit, cursor)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	out := make([]apiCredentialDTO, 0, len(pg.Items))
	for _, c := range pg.Items {
		out = append(out, credentialDTO(c))
	}
	a.writeJSON(w, r, http.StatusOK, newPage(out, pg.NextCursor))
}

func (a *API) createAPICredential(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string   `json:"name"`
		Scopes        []string `json:"scopes"`
		ExpiresInDays *int     `json:"expires_in_days"`
	}
	if err := decodeJSON(r, &in); err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	g, err := grantor(p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	issued, err := a.Credentials.Create(r.Context(), p.TenantID, g, apicredentials.CreateInput{Name: in.Name, Scopes: in.Scopes,
		ExpiresInDays: in.ExpiresInDays})
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeJSON(w, r, http.StatusCreated, apiCredentialWithSecretDTO{Credential: credentialDTO(issued.Credential), Secret: issued.Token})
}

func (a *API) rotateAPICredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	g, err := grantor(p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	issued, err := a.Credentials.Rotate(r.Context(), p.TenantID, g, id, version)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	a.writeVersioned(w, r, http.StatusCreated, issued.Credential.Version,
		apiCredentialWithSecretDTO{Credential: credentialDTO(issued.Credential), Secret: issued.Token})
}

func (a *API) revokeAPICredential(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	version, err := ifMatch(r)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	p := mustPrincipal(r)
	g, err := grantor(p)
	if err != nil {
		a.writeError(w, r, err)
		return
	}
	if err := a.Credentials.Revoke(r.Context(), p.TenantID, g, id, version); err != nil {
		a.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
