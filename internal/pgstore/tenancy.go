package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Tenancy implementa tenancy.Store.
type Tenancy struct{ s *Store }

// Tenancy retorna o adaptador de organizações.
func (s *Store) Tenancy() *Tenancy { return &Tenancy{s: s} }

type tenancyTx struct{ base }

// InTenant abre a transação do tenant.
func (t *Tenancy) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(tenancy.Tx) error) error {
	return t.s.inTenant(ctx, tenantID, func(b base) error { return fn(tenancyTx{b}) })
}

// SlugAvailable consulta a disponibilidade do slug sem expor organizações.
func (t *Tenancy) SlugAvailable(ctx context.Context, slug string) (bool, error) {
	var ok bool
	err := t.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		ok, err = q.TenantSlugAvailable(ctx, slug)
		return err
	})
	return ok, err
}

// FindInvitation localiza o tenant de um convite pelo hash do token.
func (t *Tenancy) FindInvitation(ctx context.Context, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	var row dbgen.FindInvitationRow
	err := t.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		row, err = q.FindInvitation(ctx, tokenHash)
		return notFound(err, "invitation_not_found", "Convite não encontrado.")
	})
	return row.TenantID, row.InvitationID, err
}

// UserMemberships lista as organizações ativas do usuário.
func (t *Tenancy) UserMemberships(ctx context.Context, userID uuid.UUID) ([]tenancy.UserMembership, error) {
	var out []tenancy.UserMembership
	err := t.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.UserMemberships(ctx, userID)
		for _, r := range rows {
			out = append(out, tenancy.UserMembership{TenantID: r.TenantID, TenantName: r.TenantName, TenantSlug: r.TenantSlug,
				Role: tenancy.Role(r.Role), TenantStatus: tenancy.TenantStatus(r.TenantStatus)})
		}
		return err
	})
	return out, err
}

// PlatformCatalog lista organizações para a equipe da plataforma.
func (t *Tenancy) PlatformCatalog(ctx context.Context) ([]dbgen.PlatformTenantCatalogRow, error) {
	var out []dbgen.PlatformTenantCatalogRow
	err := t.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.PlatformTenantCatalog(ctx)
		return err
	})
	return out, err
}

func tenantFromRow(r dbgen.Tenant) tenancy.Tenant {
	return tenancy.Tenant{ID: r.ID, Slug: r.Slug, Name: r.Name, LegalName: deref(r.LegalName), DocumentNumber: deref(r.DocumentNumber),
		Timezone: r.Timezone, Currency: r.Currency, InvoicePrefix: r.InvoicePrefix, PaymentTermsDays: int(r.PaymentTermsDays),
		Status: tenancy.TenantStatus(r.Status), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}
}

func (tx tenancyTx) Tenant(ctx context.Context) (tenancy.Tenant, error) {
	r, err := tx.q.GetTenant(ctx)
	if err != nil {
		return tenancy.Tenant{}, notFound(err, "organization_not_found", "Organização não encontrada.")
	}
	return tenantFromRow(r), nil
}

func (tx tenancyTx) InsertTenant(ctx context.Context, t tenancy.Tenant) error {
	err := tx.q.InsertTenant(ctx, dbgen.InsertTenantParams{ID: t.ID, Slug: t.Slug, Name: t.Name, LegalName: strPtr(t.LegalName),
		DocumentNumber: strPtr(t.DocumentNumber), Timezone: t.Timezone, Currency: t.Currency, InvoicePrefix: t.InvoicePrefix,
		PaymentTermsDays: int32(t.PaymentTermsDays), Status: string(t.Status), CreatedAt: t.CreatedAt})
	if postgres.IsUniqueViolation(err, "tenants_slug_key") {
		return apperr.Conflict("slug_taken", "Este identificador de organização já está em uso.")
	}
	return err
}

func (tx tenancyTx) UpdateTenant(ctx context.Context, t tenancy.Tenant) error {
	n, err := tx.q.UpdateTenant(ctx, dbgen.UpdateTenantParams{ID: t.ID, Name: t.Name, LegalName: strPtr(t.LegalName),
		DocumentNumber: strPtr(t.DocumentNumber), Timezone: t.Timezone, InvoicePrefix: t.InvoicePrefix,
		PaymentTermsDays: int32(t.PaymentTermsDays), Version: int32(t.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "A organização foi alterada por outra pessoa.")
}

func (tx tenancyTx) InitTenantResources(ctx context.Context) error {
	if err := tx.q.InitInvoiceSequence(ctx, tx.tenantID); err != nil {
		return err
	}
	return tx.ensureLedgerAccounts(ctx)
}

func membershipFromRow(r dbgen.Membership) tenancy.Membership {
	return tenancy.Membership{TenantID: r.TenantID, UserID: r.UserID, Role: tenancy.Role(r.Role), Status: tenancy.MembershipStatus(r.Status),
		InvitedBy: r.InvitedBy, CreatedAt: r.CreatedAt, Version: int(r.Version)}
}

func (tx tenancyTx) Membership(ctx context.Context, userID uuid.UUID, forUpdate bool) (tenancy.Membership, error) {
	var r dbgen.Membership
	var err error
	args := dbgen.GetMembershipParams{TenantID: tx.tenantID, UserID: userID}
	if forUpdate {
		r, err = tx.q.GetMembershipForUpdate(ctx, dbgen.GetMembershipForUpdateParams(args))
	} else {
		r, err = tx.q.GetMembership(ctx, args)
	}
	if err != nil {
		return tenancy.Membership{}, notFound(err, "member_not_found", "Membro não encontrado.")
	}
	return membershipFromRow(r), nil
}

func (tx tenancyTx) InsertMembership(ctx context.Context, m tenancy.Membership) error {
	return tx.q.InsertMembership(ctx, dbgen.InsertMembershipParams{TenantID: tx.tenantID, UserID: m.UserID, Role: string(m.Role),
		Status: string(m.Status), InvitedBy: m.InvitedBy, CreatedAt: m.CreatedAt})
}

func (tx tenancyTx) UpdateMembership(ctx context.Context, m tenancy.Membership) error {
	n, err := tx.q.UpdateMembership(ctx, dbgen.UpdateMembershipParams{TenantID: tx.tenantID, UserID: m.UserID, Role: string(m.Role),
		Status: string(m.Status), Version: int32(m.Version)})
	if err != nil {
		return err
	}
	return versionConflict(n, "O membro foi alterado por outra pessoa.")
}

func (tx tenancyTx) Members(ctx context.Context) ([]tenancy.Member, error) {
	rows, err := tx.q.ListMembers(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]tenancy.Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, tenancy.Member{Membership: tenancy.Membership{TenantID: r.TenantID, UserID: r.UserID, Role: tenancy.Role(r.Role),
			Status: tenancy.MembershipStatus(r.Status), InvitedBy: r.InvitedBy, CreatedAt: r.CreatedAt, Version: int(r.Version)},
			Email: r.Email, DisplayName: r.DisplayName})
	}
	return out, nil
}

func (tx tenancyTx) CountActiveOwners(ctx context.Context) (int, error) {
	ids, err := tx.q.LockActiveOwners(ctx, tx.tenantID)
	return len(ids), err
}

func invitationFromRow(r dbgen.Invitation) tenancy.Invitation {
	return tenancy.Invitation{ID: r.ID, TenantID: r.TenantID, Email: r.Email, Role: tenancy.Role(r.Role), TokenHash: r.TokenHash,
		InvitedBy: r.InvitedBy, ExpiresAt: r.ExpiresAt, AcceptedAt: r.AcceptedAt, AcceptedBy: r.AcceptedBy, RevokedAt: r.RevokedAt,
		RevokedBy: r.RevokedBy, CreatedAt: r.CreatedAt}
}

func (tx tenancyTx) Invitation(ctx context.Context, id uuid.UUID, forUpdate bool) (tenancy.Invitation, error) {
	var r dbgen.Invitation
	var err error
	if forUpdate {
		r, err = tx.q.GetInvitationForUpdate(ctx, dbgen.GetInvitationForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetInvitation(ctx, dbgen.GetInvitationParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return tenancy.Invitation{}, notFound(err, "invitation_not_found", "Convite não encontrado.")
	}
	return invitationFromRow(r), nil
}

func (tx tenancyTx) InsertInvitation(ctx context.Context, inv tenancy.Invitation) error {
	return tx.q.InsertInvitation(ctx, dbgen.InsertInvitationParams{ID: inv.ID, TenantID: tx.tenantID, Email: inv.Email, Role: string(inv.Role),
		TokenHash: inv.TokenHash, InvitedBy: inv.InvitedBy, ExpiresAt: inv.ExpiresAt, CreatedAt: inv.CreatedAt})
}

func (tx tenancyTx) UpdateInvitation(ctx context.Context, inv tenancy.Invitation) error {
	return tx.q.UpdateInvitation(ctx, dbgen.UpdateInvitationParams{TenantID: tx.tenantID, ID: inv.ID, AcceptedAt: inv.AcceptedAt,
		AcceptedBy: inv.AcceptedBy, RevokedAt: inv.RevokedAt, RevokedBy: inv.RevokedBy})
}

func (tx tenancyTx) Invitations(ctx context.Context) ([]tenancy.Invitation, error) {
	rows, err := tx.q.ListInvitations(ctx, tx.tenantID)
	out := make([]tenancy.Invitation, 0, len(rows))
	for _, r := range rows {
		out = append(out, invitationFromRow(r))
	}
	return out, err
}

func (tx tenancyTx) PendingInvitations(ctx context.Context, now time.Time) (int, error) {
	n, err := tx.q.CountPendingInvitations(ctx, dbgen.CountPendingInvitationsParams{TenantID: tx.tenantID, Now: now})
	return int(n), err
}

func (tx tenancyTx) DefaultPlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	r, err := tx.q.GetDefaultPlatformPlan(ctx)
	if err != nil {
		return platformbilling.Plan{}, notFound(err, "platform_plan_not_found", "Plano padrão da plataforma não configurado.")
	}
	return platformPlanFromRow(r), nil
}

func (tx tenancyTx) LockPlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.lockedPlatformPlan(ctx)
}

func (tx tenancyTx) PlatformUsage(ctx context.Context) (platformbilling.Usage, error) {
	return tx.platformUsage(ctx)
}

func (tx tenancyTx) InsertPlatformSubscription(ctx context.Context, s platformbilling.Subscription) error {
	return tx.q.InsertTenantPlatformSubscription(ctx, dbgen.InsertTenantPlatformSubscriptionParams{TenantID: tx.tenantID, PlanCode: s.PlanCode,
		Status: string(s.Status), TrialEndsAt: s.TrialEndsAt, AnchorDate: dateToPg(s.Anchor), CurrentPeriodStart: s.PeriodStart,
		CurrentPeriodEnd: s.PeriodEnd, CreatedAt: tx.store.clock.Now()})
}

// --- plano da plataforma (compartilhado por vários módulos) ---

func platformPlanFromRow(r dbgen.PlatformPlan) platformbilling.Plan {
	features := make([]platformbilling.Feature, len(r.Features))
	for i, f := range r.Features {
		features[i] = platformbilling.Feature(f)
	}
	return platformbilling.Plan{Code: r.Code, Name: r.Name, Description: r.Description, MonthlyPrice: brl(r.MonthlyPriceAmount),
		MaxUsers: intFromPtr32(r.MaxUsers), MaxActiveSubscriptions: intFromPtr32(r.MaxActiveSubscriptions), Features: features,
		TrialDays: int(r.TrialDays), IsDefault: r.IsDefault, Active: r.Active}
}

func (b base) platformPlan(ctx context.Context) (platformbilling.Plan, error) {
	sub, err := b.q.GetTenantPlatformSubscription(ctx, b.tenantID)
	if err != nil {
		return platformbilling.Plan{}, notFound(err, "platform_subscription_not_found", "Assinatura da plataforma não encontrada.")
	}
	r, err := b.q.GetPlatformPlan(ctx, sub.PlanCode)
	if err != nil {
		return platformbilling.Plan{}, err
	}
	return platformPlanFromRow(r), nil
}

// lockedPlatformPlan lê o plano bloqueando a linha da assinatura do tenant na
// plataforma (FOR UPDATE) até o fim da transação. As checagens de limite
// (convites, aceites, criação e reativação de assinaturas) e a troca de
// plano passam por esse bloqueio, o que serializa a contagem do consumo em
// READ COMMITTED (ADR-077).
func (b base) lockedPlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	sub, err := b.q.GetTenantPlatformSubscriptionForUpdate(ctx, b.tenantID)
	if err != nil {
		return platformbilling.Plan{}, notFound(err, "platform_subscription_not_found", "Assinatura da plataforma não encontrada.")
	}
	r, err := b.q.GetPlatformPlan(ctx, sub.PlanCode)
	if err != nil {
		return platformbilling.Plan{}, err
	}
	return platformPlanFromRow(r), nil
}

func (b base) platformUsage(ctx context.Context) (platformbilling.Usage, error) {
	users, err := b.q.CountActiveMembers(ctx, b.tenantID)
	if err != nil {
		return platformbilling.Usage{}, err
	}
	subs, err := b.q.CountActiveSubscriptions(ctx, b.tenantID)
	return platformbilling.Usage{ActiveUsers: int(users), ActiveSubscriptions: int(subs)}, err
}

// --- usuários (identidade global) ---

// Users implementa identity.UserStore.
type Users struct{ s *Store }

// Users retorna o adaptador de usuários.
func (s *Store) Users() *Users { return &Users{s: s} }

func userFromRow(r dbgen.User) identity.User {
	return identity.User{ID: r.ID, Issuer: r.OidcIssuer, Subject: r.OidcSubject, Email: r.Email, DisplayName: r.DisplayName,
		Status: r.Status, CreatedAt: r.CreatedAt, LastLoginAt: r.LastLoginAt}
}

// UpsertFromClaims cria ou atualiza o usuário pelo par emissor/sujeito.
func (u *Users) UpsertFromClaims(ctx context.Context, id uuid.UUID, c identity.Claims, now time.Time) (identity.User, error) {
	var out identity.User
	name := c.Name
	if name == "" {
		name = c.Email
	}
	err := u.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		r, err := q.UpsertUserFromClaims(ctx, dbgen.UpsertUserFromClaimsParams{ID: id, OidcIssuer: c.Issuer, OidcSubject: c.Subject,
			Email: c.Email, DisplayName: name, Now: now})
		out = userFromRow(r)
		return err
	})
	return out, err
}

// User retorna um usuário.
func (u *Users) User(ctx context.Context, id uuid.UUID) (identity.User, error) {
	var out identity.User
	err := u.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		r, err := q.GetUser(ctx, id)
		if err != nil {
			return notFound(err, "user_not_found", "Usuário não encontrado.")
		}
		out = userFromRow(r)
		return nil
	})
	return out, err
}

// IsPlatformStaff informa se o usuário integra a equipe da plataforma.
func (u *Users) IsPlatformStaff(ctx context.Context, id uuid.UUID) (bool, error) {
	var ok bool
	err := u.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		ok, err = q.IsPlatformStaff(ctx, id)
		return err
	})
	return ok, err
}
