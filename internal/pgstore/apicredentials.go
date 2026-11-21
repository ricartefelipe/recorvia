package pgstore

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// APICredentials implementa apicredentials.Store.
type APICredentials struct{ s *Store }

// APICredentials retorna o adaptador de credenciais de API.
func (s *Store) APICredentials() *APICredentials { return &APICredentials{s: s} }

type apiCredentialsTx struct{ base }

// InTenant abre a transação do tenant.
func (a *APICredentials) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(apicredentials.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(apiCredentialsTx{b}) })
}

// FindCredential localiza o tenant e a credencial pelo identificador público.
func (a *APICredentials) FindCredential(ctx context.Context, keyID string) (uuid.UUID, uuid.UUID, error) {
	var row dbgen.FindAPICredentialRow
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		row, err = q.FindAPICredential(ctx, keyID)
		return notFound(err, "api_credential_not_found", "Credencial não encontrada.")
	})
	return row.TenantID, row.CredentialID, err
}

func apiCredentialFromRow(r dbgen.ApiCredential) apicredentials.Credential {
	scopes := make([]tenancy.Permission, len(r.Scopes))
	for i, s := range r.Scopes {
		scopes[i] = tenancy.Permission(s)
	}
	return apicredentials.Credential{ID: r.ID, TenantID: r.TenantID, Name: r.Name, KeyID: r.KeyID, SecretHash: r.SecretHash,
		PreviousSecretHash: r.PreviousSecretHash, PreviousSecretExpiresAt: r.PreviousSecretExpiresAt, Scopes: scopes,
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, RotatedBy: r.RotatedBy, RotatedAt: r.RotatedAt,
		LastUsedAt: r.LastUsedAt, RevokedBy: r.RevokedBy, RevokedAt: r.RevokedAt, Version: int(r.Version)}
}

func (tx apiCredentialsTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

func (tx apiCredentialsTx) LockIssuance(ctx context.Context) error {
	_, err := tx.q.LockTenantForAPICredentialIssuance(ctx, tx.tenantID)
	return notFound(err, "organization_not_found", "Organização não encontrada.")
}

func (tx apiCredentialsTx) CountActiveCredentials(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.q.CountActiveAPICredentials(ctx, dbgen.CountActiveAPICredentialsParams{TenantID: tx.tenantID, Now: &now})
	return int(n), err
}

func (tx apiCredentialsTx) InsertCredential(ctx context.Context, c apicredentials.Credential) error {
	scopes := make([]string, len(c.Scopes))
	for i, s := range c.Scopes {
		scopes[i] = string(s)
	}
	err := tx.q.InsertAPICredential(ctx, dbgen.InsertAPICredentialParams{ID: c.ID, TenantID: tx.tenantID, Name: c.Name, KeyID: c.KeyID,
		SecretHash: c.SecretHash, Scopes: scopes, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt})
	if postgres.IsUniqueViolation(err, "api_credentials_key_id_key") {
		return apperr.Conflict("api_credential_key_collision", "Não foi possível gerar um identificador único. Tente novamente.")
	}
	return err
}

func (tx apiCredentialsTx) Credential(ctx context.Context, id uuid.UUID, forUpdate bool) (apicredentials.Credential, error) {
	var r dbgen.ApiCredential
	var err error
	if forUpdate {
		r, err = tx.q.GetAPICredentialForUpdate(ctx, dbgen.GetAPICredentialForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetAPICredential(ctx, dbgen.GetAPICredentialParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return apicredentials.Credential{}, notFound(err, "api_credential_not_found", "Credencial não encontrada.")
	}
	return apiCredentialFromRow(r), nil
}

// credentialCursor é a posição na listagem de credenciais: o instante em que
// a situação foi avaliada (o da primeira página) e a última credencial da
// página (ativa, criação, id).
type credentialCursor struct {
	asOf      time.Time
	active    bool
	createdAt time.Time
	id        uuid.UUID
}

const credentialCursorKind = "cred"

func (c credentialCursor) String() string {
	active := "0"
	if c.active {
		active = "1"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join([]string{credentialCursorKind, c.asOf.UTC().Format(time.RFC3339Nano), active,
		c.createdAt.UTC().Format(time.RFC3339Nano), c.id.String()}, "|")))
}

// parseCredentialCursor aceita somente a forma canônica gerada pelo
// servidor; fora dela, 422 invalid_cursor.
func parseCredentialCursor(s string) (*credentialCursor, error) {
	if s == "" {
		return nil, nil
	}
	invalid := apperr.Invalid("invalid_cursor", "Cursor de paginação inválido.")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, invalid
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 5 || parts[0] != credentialCursorKind || (parts[2] != "0" && parts[2] != "1") {
		return nil, invalid
	}
	c := credentialCursor{active: parts[2] == "1"}
	if c.asOf, err = time.Parse(time.RFC3339Nano, parts[1]); err != nil {
		return nil, invalid
	}
	if c.createdAt, err = time.Parse(time.RFC3339Nano, parts[3]); err != nil {
		return nil, invalid
	}
	if c.id, err = uuid.Parse(parts[4]); err != nil {
		return nil, invalid
	}
	if c.String() != s {
		return nil, invalid
	}
	return &c, nil
}

func (tx apiCredentialsTx) Credentials(ctx context.Context, now time.Time, limit int, cursor string) (apicredentials.Page, error) {
	after, err := parseCredentialCursor(cursor)
	if err != nil {
		return apicredentials.Page{}, err
	}
	limit = pageLimit(limit)
	// Resolução do banco, para que a situação calculada aqui para o cursor
	// coincida com a da ordenação.
	now = now.Truncate(time.Microsecond)
	args := dbgen.ListAPICredentialsParams{TenantID: tx.tenantID, Now: &now, RowLimit: int32(limit + 1)}
	if after != nil {
		active := int32(0)
		if after.active {
			active = 1
		}
		args.Now, args.CursorActive, args.CursorTs, args.CursorID = &after.asOf, &active, &after.createdAt, &after.id
	}
	rows, err := tx.q.ListAPICredentials(ctx, args)
	if err != nil {
		return apicredentials.Page{}, err
	}
	p := apicredentials.Page{Items: make([]apicredentials.Credential, 0, min(len(rows), limit))}
	for i, r := range rows {
		if i == limit {
			last := p.Items[limit-1]
			p.NextCursor = credentialCursor{asOf: *args.Now, active: last.State(*args.Now) == apicredentials.StateActive,
				createdAt: last.CreatedAt, id: last.ID}.String()
			break
		}
		p.Items = append(p.Items, apiCredentialFromRow(r))
	}
	return p, nil
}

func (tx apiCredentialsTx) RotateCredential(ctx context.Context, c apicredentials.Credential) error {
	n, err := tx.q.RotateAPICredential(ctx, dbgen.RotateAPICredentialParams{TenantID: tx.tenantID, ID: c.ID, Version: int32(c.Version),
		SecretHash: c.SecretHash, PreviousSecretHash: c.PreviousSecretHash, PreviousSecretExpiresAt: c.PreviousSecretExpiresAt,
		RotatedBy: c.RotatedBy, RotatedAt: c.RotatedAt})
	if err != nil {
		return err
	}
	return versionConflict(n, "A credencial foi alterada por outra pessoa.")
}

func (tx apiCredentialsTx) RevokeCredential(ctx context.Context, c apicredentials.Credential) error {
	n, err := tx.q.RevokeAPICredential(ctx, dbgen.RevokeAPICredentialParams{TenantID: tx.tenantID, ID: c.ID, RevokedAt: c.RevokedAt,
		RevokedBy: c.RevokedBy, Version: int32(c.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "A credencial foi alterada por outra pessoa.")
}

func (tx apiCredentialsTx) AuthContext(ctx context.Context, id uuid.UUID) (apicredentials.AuthContext, error) {
	r, err := tx.q.GetAPICredentialAuthContext(ctx, dbgen.GetAPICredentialAuthContextParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return apicredentials.AuthContext{}, notFound(err, "api_credential_not_found", "Credencial não encontrada.")
	}
	features := make([]platformbilling.Feature, len(r.PlanFeatures))
	for i, f := range r.PlanFeatures {
		features[i] = platformbilling.Feature(f)
	}
	return apicredentials.AuthContext{Credential: apiCredentialFromRow(r.ApiCredential), TenantStatus: tenancy.TenantStatus(r.TenantStatus),
		IssuerRole: tenancy.Role(r.IssuerRole), IssuerActive: r.IssuerStatus == string(tenancy.MembershipActive), PlanFeatures: features}, nil
}

func (tx apiCredentialsTx) TouchCredential(ctx context.Context, id uuid.UUID, at, staleBefore time.Time) error {
	return tx.q.TouchAPICredential(ctx, dbgen.TouchAPICredentialParams{TenantID: tx.tenantID, ID: id, UsedAt: &at, StaleBefore: &staleBefore})
}
