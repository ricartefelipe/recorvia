package tenancy

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// Tx são as operações dentro da transação de um tenant.
type Tx interface {
	// Tenant devolve a organização com o endereço (nil sem cadastro).
	Tenant(ctx context.Context) (Tenant, error)
	InsertTenant(ctx context.Context, t Tenant) error
	// UpdateTenant grava os dados cadastrais, sem o endereço.
	UpdateTenant(ctx context.Context, t Tenant) error
	// UpsertTenantAddress grava o endereço da organização.
	UpsertTenantAddress(ctx context.Context, a address.Address, at time.Time) error
	InitTenantResources(ctx context.Context) error

	Membership(ctx context.Context, userID uuid.UUID, forUpdate bool) (Membership, error)
	InsertMembership(ctx context.Context, m Membership) error
	UpdateMembership(ctx context.Context, m Membership) error
	Members(ctx context.Context) ([]Member, error)
	CountActiveOwners(ctx context.Context) (int, error)

	Invitation(ctx context.Context, id uuid.UUID, forUpdate bool) (Invitation, error)
	InsertInvitation(ctx context.Context, inv Invitation) error
	UpdateInvitation(ctx context.Context, inv Invitation) error
	Invitations(ctx context.Context, limit int, cursor string) (InvitationPage, error)
	PendingInvitations(ctx context.Context, now time.Time) (int, error)

	DefaultPlatformPlan(ctx context.Context) (platformbilling.Plan, error)
	// LockPlatformPlan lê o plano da plataforma bloqueando a assinatura do
	// tenant na plataforma até o fim da transação: convites, aceites, troca de
	// plano e criação de assinaturas contam o consumo um de cada vez.
	LockPlatformPlan(ctx context.Context) (platformbilling.Plan, error)
	PlatformUsage(ctx context.Context) (platformbilling.Usage, error)
	InsertPlatformSubscription(ctx context.Context, s platformbilling.Subscription) error

	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações e consulta o catálogo global de forma restrita.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	SlugAvailable(ctx context.Context, slug string) (bool, error)
	FindInvitation(ctx context.Context, tokenHash []byte) (tenantID, invitationID uuid.UUID, err error)
	UserMemberships(ctx context.Context, userID uuid.UUID) ([]UserMembership, error)
}

// UserRef identifica o usuário autenticado que executa a ação.
type UserRef struct {
	ID          uuid.UUID
	Email       string
	DisplayName string
}

func (u UserRef) actor() audit.Actor { return audit.User(u.ID, u.DisplayName) }

// TokenSealer cifra o token do convite incluído no evento member.invited,
// para que o worker envie o e-mail sem que o token fique em claro no banco.
type TokenSealer interface {
	Encrypt(plaintext, aad []byte) ([]byte, string, error)
}

// Service implementa os casos de uso de organizações e membros.
type Service struct {
	store  Store
	clock  clock.Clock
	sealer TokenSealer
}

// NewService cria o serviço.
func NewService(store Store, c clock.Clock) *Service { return &Service{store: store, clock: c} }

// WithTokenSealer habilita o envio do token cifrado no evento do convite
// (usado pelo e-mail de convite). Sem ele, o evento não carrega o token.
func (s *Service) WithTokenSealer(ts TokenSealer) *Service {
	s.sealer = ts
	return s
}

// Onboard cria a organização, torna o usuário proprietário e inicia o plano
// padrão da plataforma em avaliação.
func (s *Service) Onboard(ctx context.Context, user UserRef, in TenantInput) (Tenant, error) {
	now := s.clock.Now()
	t, err := NewTenant(ids.New(), in, now)
	if err != nil {
		return Tenant{}, err
	}
	free, err := s.store.SlugAvailable(ctx, t.Slug)
	if err != nil {
		return Tenant{}, err
	}
	if !free {
		return Tenant{}, apperr.Conflict("slug_taken", "Este identificador de organização já está em uso.")
	}
	loc, err := t.Location()
	if err != nil {
		return Tenant{}, err
	}
	err = s.store.InTenant(ctx, t.ID, func(tx Tx) error {
		if err := tx.InsertTenant(ctx, t); err != nil {
			return err
		}
		if err := tx.InsertMembership(ctx, Membership{
			TenantID: t.ID, UserID: user.ID, Role: RoleOwner, Status: MembershipActive, CreatedAt: now, Version: 1,
		}); err != nil {
			return err
		}
		plan, err := tx.DefaultPlatformPlan(ctx)
		if err != nil {
			return err
		}
		sub, err := platformbilling.NewSubscription(t.ID, plan, now, loc)
		if err != nil {
			return err
		}
		if err := tx.InsertPlatformSubscription(ctx, sub); err != nil {
			return err
		}
		if err := tx.InitTenantResources(ctx); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(t.ID, now, user.actor(), "organization.created", "organization", t.ID.String(),
			map[string]any{"slug": t.Slug, "platform_plan": plan.Code}))
	})
	if err != nil {
		return Tenant{}, err
	}
	return t, nil
}

// Organization retorna a organização.
func (s *Service) Organization(ctx context.Context, tenantID uuid.UUID) (Tenant, error) {
	var t Tenant
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		t, err = tx.Tenant(ctx)
		return err
	})
	return t, err
}

// UpdateOrganization altera os dados com controle de concorrência otimista.
// O endereço informado substitui o atual na mesma transação; quando muda, a
// auditoria registra organization.address_updated com os nomes dos campos
// alterados e se o endereço foi cadastrado agora, como no endereço de
// cobrança do cliente.
func (s *Service) UpdateOrganization(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, in TenantInput, expectedVersion int) (Tenant, error) {
	var t Tenant
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if t, err = tx.Tenant(ctx); err != nil {
			return err
		}
		if expectedVersion != t.Version {
			return apperr.PreconditionFailed("version_mismatch", "A organização foi alterada por outra pessoa. Recarregue e tente novamente.")
		}
		prev := t.Address
		if err := t.ApplyUpdate(in); err != nil {
			return err
		}
		if err := tx.UpdateTenant(ctx, t); err != nil {
			return err
		}
		t.Version++
		now := s.clock.Now()
		if err := tx.Audit(ctx, audit.New(tenantID, now, actor, "organization.updated", "organization", tenantID.String(), nil)); err != nil {
			return err
		}
		if t.Address == nil {
			return nil
		}
		changed := t.Address.ChangedFields(prev)
		if len(changed) == 0 {
			return nil
		}
		if err := tx.UpsertTenantAddress(ctx, *t.Address, now); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "organization.address_updated", "organization", tenantID.String(),
			map[string]any{"fields": changed, "created": prev == nil}))
	})
	return t, err
}

// Access é o resultado da validação de acesso de um usuário a um tenant.
type Access struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	Role     Role
}

// ResolveAccess valida que o usuário tem associação ativa a uma organização
// ativa. É chamado em toda requisição autenticada: o identificador do
// tenant recebido nunca é aceito isoladamente.
func (s *Service) ResolveAccess(ctx context.Context, tenantID, userID uuid.UUID) (Access, error) {
	var a Access
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		t, err := tx.Tenant(ctx)
		if apperr.IsKind(err, apperr.KindNotFound) {
			return apperr.Forbidden("no_membership", "Você não tem acesso a esta organização.")
		}
		if err != nil {
			return err
		}
		if t.Status != TenantActive {
			return apperr.Forbidden("organization_inactive", "Esta organização não está ativa.")
		}
		m, err := tx.Membership(ctx, userID, false)
		if err != nil && !apperr.IsKind(err, apperr.KindNotFound) {
			return err
		}
		if err != nil || m.Status != MembershipActive {
			return apperr.Forbidden("no_membership", "Você não tem acesso a esta organização.")
		}
		a = Access{TenantID: tenantID, UserID: userID, Role: m.Role}
		return nil
	})
	return a, err
}

// UserOrganizations lista as organizações do usuário.
func (s *Service) UserOrganizations(ctx context.Context, userID uuid.UUID) ([]UserMembership, error) {
	return s.store.UserMemberships(ctx, userID)
}

// Members lista os membros.
func (s *Service) Members(ctx context.Context, tenantID uuid.UUID) ([]Member, error) {
	var out []Member
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Members(ctx)
		return err
	})
	return out, err
}

// Invitations lista os convites, dos mais recentes para os mais antigos, em
// páginas por cursor.
func (s *Service) Invitations(ctx context.Context, tenantID uuid.UUID, limit int, cursor string) (InvitationPage, error) {
	var out InvitationPage
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Invitations(ctx, limit, cursor)
		return err
	})
	return out, err
}

// Invite cria um convite. Somente proprietários convidam proprietários. O
// limite de usuários do plano considera membros ativos e convites pendentes,
// contados com a assinatura da plataforma bloqueada (convites simultâneos não
// passam do limite).
func (s *Service) Invite(ctx context.Context, tenantID uuid.UUID, inviter Access, inviterRef UserRef, email string, role Role) (Invitation, string, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return Invitation{}, "", err
	}
	if _, err := ParseRole(string(role)); err != nil {
		return Invitation{}, "", apperr.Invalid("invalid_role", "Papel inválido.", apperr.FieldError{Field: "role", Message: "Papel inválido."})
	}
	if role == RoleOwner && inviter.Role != RoleOwner {
		return Invitation{}, "", apperr.Forbidden("owner_only", "Somente proprietários podem convidar proprietários.")
	}
	now := s.clock.Now()
	token, hash := NewToken()
	inv := Invitation{
		ID: ids.New(), TenantID: tenantID, Email: email, Role: role, TokenHash: hash,
		InvitedBy: inviter.UserID, ExpiresAt: now.Add(InvitationTTL), CreatedAt: now,
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		plan, err := tx.LockPlatformPlan(ctx)
		if err != nil {
			return err
		}
		usage, err := tx.PlatformUsage(ctx)
		if err != nil {
			return err
		}
		if usage.PendingInvitations, err = tx.PendingInvitations(ctx, now); err != nil {
			return err
		}
		if err := platformbilling.CheckUserLimit(plan, usage); err != nil {
			return err
		}
		if err := tx.InsertInvitation(ctx, inv); err != nil {
			return err
		}
		payload := map[string]any{"invitation_id": inv.ID, "role": role, "expires_at": inv.ExpiresAt}
		if s.sealer != nil {
			ct, keyID, err := s.sealer.Encrypt([]byte(token), InvitationTokenAAD(tenantID, inv.ID))
			if err != nil {
				return err
			}
			payload["token_ciphertext"], payload["token_key_id"] = base64.StdEncoding.EncodeToString(ct), keyID
		}
		if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventMemberInvited, "invitation", inv.ID, payload, now)); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, inviterRef.actor(), "member.invited", "invitation", inv.ID.String(),
			map[string]any{"role": role}))
	})
	if err != nil {
		return Invitation{}, "", err
	}
	return inv, token, nil
}

// RevokeInvitation revoga um convite pendente.
func (s *Service) RevokeInvitation(ctx context.Context, tenantID uuid.UUID, actor UserRef, invitationID uuid.UUID) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.Invitation(ctx, invitationID, true)
		if err != nil {
			return err
		}
		if inv.State(now) != InvitationPending {
			return apperr.Conflict("invitation_not_pending", "O convite não está pendente.")
		}
		inv.RevokedAt, inv.RevokedBy = &now, &actor.ID
		if err := tx.UpdateInvitation(ctx, inv); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor.actor(), "member.invitation_revoked", "invitation", inv.ID.String(), nil))
	})
}

// AcceptInvitation associa o usuário autenticado à organização do convite.
// O convite é de uso único, expira e exige o mesmo e-mail do usuário. O
// limite de usuários do plano é conferido de novo (o plano pode ter mudado
// depois do convite), com a assinatura da plataforma bloqueada antes do
// convite: aceites simultâneos contam os membros um de cada vez.
func (s *Service) AcceptInvitation(ctx context.Context, user UserRef, token string) (Membership, error) {
	invalid := apperr.Invalid("invitation_invalid", "Convite inválido, expirado ou já utilizado.")
	if len(token) < 20 || len(token) > 200 {
		return Membership{}, invalid
	}
	tenantID, invID, err := s.store.FindInvitation(ctx, HashToken(token))
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return Membership{}, invalid
		}
		return Membership{}, err
	}
	now := s.clock.Now()
	var m Membership
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		plan, err := tx.LockPlatformPlan(ctx)
		if err != nil {
			return err
		}
		inv, err := tx.Invitation(ctx, invID, true)
		if err != nil {
			return err
		}
		if inv.State(now) != InvitationPending {
			return invalid
		}
		if !strings.EqualFold(inv.Email, user.Email) {
			return apperr.Forbidden("invitation_email_mismatch", "Este convite foi enviado para outro endereço de e-mail.")
		}
		existing, err := tx.Membership(ctx, user.ID, true)
		if err == nil && existing.Status == MembershipActive {
			return apperr.Conflict("already_member", "Você já participa desta organização.")
		}
		if err == nil || apperr.IsKind(err, apperr.KindNotFound) {
			usage, uerr := tx.PlatformUsage(ctx)
			if uerr != nil {
				return uerr
			}
			if lerr := platformbilling.CheckJoinLimit(plan, usage); lerr != nil {
				return lerr
			}
		}
		switch {
		case err == nil:
			existing.Role, existing.Status = inv.Role, MembershipActive
			if err := tx.UpdateMembership(ctx, existing); err != nil {
				return err
			}
			existing.Version++
			m = existing
		case apperr.IsKind(err, apperr.KindNotFound):
			m = Membership{TenantID: tenantID, UserID: user.ID, Role: inv.Role, Status: MembershipActive,
				InvitedBy: &inv.InvitedBy, CreatedAt: now, Version: 1}
			if err := tx.InsertMembership(ctx, m); err != nil {
				return err
			}
		default:
			return err
		}
		inv.AcceptedAt, inv.AcceptedBy = &now, &user.ID
		if err := tx.UpdateInvitation(ctx, inv); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, user.actor(), "member.joined", "membership", user.ID.String(),
			map[string]any{"role": inv.Role, "invitation_id": inv.ID}))
	})
	return m, err
}

// ChangeRole altera o papel de um membro. Regras: somente proprietários
// concedem ou retiram o papel de proprietário; a organização mantém ao menos
// um proprietário ativo. A gravação é condicionada à versão esperada da
// associação (If-Match): divergente, nada é alterado (412).
func (s *Service) ChangeRole(ctx context.Context, tenantID uuid.UUID, actor Access, actorRef UserRef, target uuid.UUID, role Role,
	expectedVersion int) (Membership, error) {
	if _, err := ParseRole(string(role)); err != nil {
		return Membership{}, apperr.Invalid("invalid_role", "Papel inválido.", apperr.FieldError{Field: "role", Message: "Papel inválido."})
	}
	var m Membership
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if m, err = tx.Membership(ctx, target, true); err != nil {
			return err
		}
		if m.Status != MembershipActive {
			return apperr.NotFound("member_not_found", "Membro não encontrado.")
		}
		if (m.Role == RoleOwner || role == RoleOwner) && actor.Role != RoleOwner {
			return apperr.Forbidden("owner_only", "Somente proprietários alteram o papel de proprietário.")
		}
		if m.Role == RoleOwner && role != RoleOwner {
			if err := s.ensureAnotherOwner(ctx, tx); err != nil {
				return err
			}
		}
		previous := m.Role
		m.Role, m.Version = role, expectedVersion
		if err := tx.UpdateMembership(ctx, m); err != nil {
			return err
		}
		m.Version++
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actorRef.actor(), "member.role_changed", "membership", target.String(),
			map[string]any{"from": previous, "to": role}))
	})
	return m, err
}

// RemoveMember desativa a associação, condicionada à versão esperada
// (If-Match). As sessões do usuário perdem o acesso a esta organização na
// próxima requisição (ResolveAccess).
func (s *Service) RemoveMember(ctx context.Context, tenantID uuid.UUID, actor Access, actorRef UserRef, target uuid.UUID, expectedVersion int) error {
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		m, err := tx.Membership(ctx, target, true)
		if err != nil {
			return err
		}
		if m.Status != MembershipActive {
			return apperr.NotFound("member_not_found", "Membro não encontrado.")
		}
		if m.Role == RoleOwner {
			if actor.Role != RoleOwner {
				return apperr.Forbidden("owner_only", "Somente proprietários removem proprietários.")
			}
			if err := s.ensureAnotherOwner(ctx, tx); err != nil {
				return err
			}
		}
		m.Status, m.Version = MembershipRemoved, expectedVersion
		if err := tx.UpdateMembership(ctx, m); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), actorRef.actor(), "member.removed", "membership", target.String(), nil))
	})
}

func (s *Service) ensureAnotherOwner(ctx context.Context, tx Tx) error {
	owners, err := tx.CountActiveOwners(ctx)
	if err != nil {
		return err
	}
	if owners <= 1 {
		return apperr.Conflict("last_owner", "A organização precisa manter ao menos um proprietário.")
	}
	return nil
}
