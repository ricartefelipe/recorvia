package apicredentials

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// AuthContext reúne o que a autenticação verifica além da própria credencial.
type AuthContext struct {
	Credential   Credential
	TenantStatus tenancy.TenantStatus
	// IssuerRole e IssuerActive descrevem a associação atual de quem emitiu
	// o segredo vigente (Credential.Issuer).
	IssuerRole   tenancy.Role
	IssuerActive bool
	PlanFeatures []platformbilling.Feature
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)
	// LockIssuance serializa as emissões de credenciais da organização até o
	// fim da transação, para que a contagem de ativas valha até o commit.
	LockIssuance(ctx context.Context) error
	CountActiveCredentials(ctx context.Context, now time.Time) (int, error)
	InsertCredential(ctx context.Context, c Credential) error
	Credential(ctx context.Context, id uuid.UUID, forUpdate bool) (Credential, error)
	// Credentials lista as credenciais com as ativas em now sempre primeiro.
	// Credentials devolve uma página da listagem. A situação das credenciais
	// que define a ordem é a do instante da primeira página (now), levado no
	// cursor das seguintes.
	Credentials(ctx context.Context, now time.Time, limit int, cursor string) (Page, error)
	// RotateCredential e RevokeCredential gravam condicionados à versão de c
	// (412 version_mismatch se a linha tiver outra versão).
	RotateCredential(ctx context.Context, c Credential) error
	RevokeCredential(ctx context.Context, c Credential) error
	AuthContext(ctx context.Context, id uuid.UUID) (AuthContext, error)
	// TouchCredential registra o último uso se o registro anterior não for
	// posterior a staleBefore.
	TouchCredential(ctx context.Context, id uuid.UUID, at, staleBefore time.Time) error
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações do tenant e localiza credenciais pelo identificador
// público, sem contexto de tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	FindCredential(ctx context.Context, keyID string) (tenantID, credentialID uuid.UUID, err error)
}

// FailureCounter conta falhas de autenticação por origem em janela fixa.
type FailureCounter interface {
	Failures(ctx context.Context, key string) (int, error)
	RecordFailure(ctx context.Context, key string, window time.Duration) error
}

// Policy define os limites de tentativas de autenticação.
type Policy struct {
	// MaxFailures bloqueia a origem após este número de falhas na janela.
	MaxFailures   int
	FailureWindow time.Duration
}

// DefaultPolicy é a política padrão: 30 falhas em 10 minutos por origem.
func DefaultPolicy() Policy { return Policy{MaxFailures: 30, FailureWindow: 10 * time.Minute} }

// Grantor é o membro que cria, rotaciona ou revoga uma credencial.
type Grantor struct {
	UserID uuid.UUID
	Role   tenancy.Role
	Actor  audit.Actor
}

// Service implementa a gestão e a autenticação de credenciais.
type Service struct {
	store    Store
	failures FailureCounter
	clock    clock.Clock
	policy   Policy
}

// NewService cria o serviço. failures pode ser nil (sem limitação de
// tentativas, por exemplo em ferramentas internas).
func NewService(store Store, failures FailureCounter, c clock.Clock, p Policy) *Service {
	return &Service{store: store, failures: failures, clock: c, policy: p}
}

// CreateInput são os dados de criação.
type CreateInput struct {
	Name          string
	Scopes        []string
	ExpiresInDays *int
}

// Issued é uma credencial com o token completo, exibido uma única vez.
type Issued struct {
	Credential Credential
	Token      string
}

func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if n := len([]rune(name)); n < 3 || n > 100 {
		return name, false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return name, false
		}
	}
	return name, true
}

// ValidateScopes normaliza os escopos pedidos (sem repetição, em ordem
// estável) e verifica que cada um é uma permissão existente, delegável a
// credenciais e concedida ao papel de quem emite.
func ValidateScopes(raw []string, grantor tenancy.Role) ([]tenancy.Permission, error) {
	if len(raw) == 0 {
		return nil, apperr.Invalid("invalid_api_credential", "Escolha pelo menos um escopo.",
			apperr.FieldError{Field: "scopes", Message: "Escolha pelo menos um escopo."})
	}
	if len(raw) > MaxScopes {
		return nil, apperr.Invalid("invalid_api_credential", "Escopos inválidos.",
			apperr.FieldError{Field: "scopes", Message: fmt.Sprintf("Informe no máximo %d escopos.", MaxScopes)})
	}
	var fields []apperr.FieldError
	out := make([]tenancy.Permission, 0, len(raw))
	for _, s := range raw {
		p, err := tenancy.ParsePermission(s)
		switch {
		case err != nil:
			fields = append(fields, apperr.FieldError{Field: "scopes", Message: "Escopo desconhecido: " + truncate(s, 64) + "."})
		case !Delegable(p):
			fields = append(fields, apperr.FieldError{Field: "scopes", Message: "O escopo " + s + " não pode ser concedido a credenciais de API."})
		default:
			out = append(out, p)
		}
	}
	if len(fields) > 0 {
		return nil, apperr.Invalid("invalid_api_credential", "Escopos inválidos.", fields...)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	for _, p := range out {
		if !grantor.Can(p) {
			return nil, apperr.Forbidden("scope_exceeds_permissions",
				"A credencial não pode ter escopos além das permissões de quem a emite: "+string(p)+".")
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func requireAPIAccess(ctx context.Context, tx Tx) error {
	plan, err := tx.PlatformPlan(ctx)
	if err != nil {
		return err
	}
	return platformbilling.RequireFeature(plan, platformbilling.FeatureAPIAccess)
}

// Create emite uma credencial. O token completo só existe no retorno.
func (s *Service) Create(ctx context.Context, tenantID uuid.UUID, g Grantor, in CreateInput) (Issued, error) {
	name, ok := validName(in.Name)
	if !ok {
		return Issued{}, apperr.Invalid("invalid_api_credential", "Nome inválido.",
			apperr.FieldError{Field: "name", Message: "Informe um nome entre 3 e 100 caracteres."})
	}
	scopes, err := ValidateScopes(in.Scopes, g.Role)
	if err != nil {
		return Issued{}, err
	}
	now := s.clock.Now()
	var expiresAt *time.Time
	if in.ExpiresInDays != nil {
		days := *in.ExpiresInDays
		if days < 1 || days > MaxExpiresInDays {
			return Issued{}, apperr.Invalid("invalid_api_credential", "Validade inválida.",
				apperr.FieldError{Field: "expires_in_days", Message: fmt.Sprintf("Informe de 1 a %d dias.", MaxExpiresInDays)})
		}
		at := now.Add(time.Duration(days) * 24 * time.Hour)
		expiresAt = &at
	}
	keyID, secret := newKeyID(), newSecret()
	c := Credential{ID: ids.New(), TenantID: tenantID, Name: name, KeyID: keyID, SecretHash: HashSecret(secret), Scopes: scopes,
		CreatedBy: g.UserID, CreatedAt: now, ExpiresAt: expiresAt, Version: 1}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := requireAPIAccess(ctx, tx); err != nil {
			return err
		}
		// Sem o bloqueio, criações simultâneas leriam a mesma contagem e
		// ultrapassariam MaxActivePerTenant.
		if err := tx.LockIssuance(ctx); err != nil {
			return err
		}
		active, err := tx.CountActiveCredentials(ctx, now)
		if err != nil {
			return err
		}
		if active >= MaxActivePerTenant {
			return apperr.Conflict("api_credential_limit",
				fmt.Sprintf("A organização já tem %d credenciais ativas. Revogue as que não são usadas.", MaxActivePerTenant))
		}
		if err := tx.InsertCredential(ctx, c); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, g.Actor, "api_credential.created", "api_credential", c.ID.String(),
			map[string]any{"prefix": c.Prefix(), "scopes": scopes, "expires_at": expiresAt}))
	})
	if err != nil {
		return Issued{}, err
	}
	return Issued{Credential: c, Token: FormatToken(keyID, secret)}, nil
}

// List lista as credenciais da organização (sem segredos). As ativas vêm
// sempre primeiro, para que nenhuma fique fora do limite da listagem.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, limit int, cursor string) (Page, error) {
	now := s.clock.Now()
	var out Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Credentials(ctx, now, limit, cursor)
		return err
	})
	return out, err
}

// Rotate gera um novo segredo para a credencial, mantendo identificador,
// nome, escopos e validade. O segredo anterior continua aceito por
// RotationGrace (sem ultrapassar a validade da credencial); uma segunda
// rotação descarta imediatamente o segredo de antes da primeira. Quem
// rotaciona passa a ser o responsável pelo segredo e precisa ter todas as
// permissões dos escopos. A gravação é condicionada à versão esperada
// (If-Match): divergente, nada é alterado (412).
func (s *Service) Rotate(ctx context.Context, tenantID uuid.UUID, g Grantor, id uuid.UUID, expectedVersion int) (Issued, error) {
	now := s.clock.Now()
	var out Issued
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := requireAPIAccess(ctx, tx); err != nil {
			return err
		}
		c, err := tx.Credential(ctx, id, true)
		if err != nil {
			return err
		}
		if c.State(now) != StateActive {
			return apperr.Conflict("api_credential_inactive", "Credencial revogada ou expirada não pode ser rotacionada.")
		}
		for _, p := range c.Scopes {
			if !g.Role.Can(p) {
				return apperr.Forbidden("scope_exceeds_permissions",
					"Somente quem tem todas as permissões dos escopos pode rotacionar esta credencial: "+string(p)+".")
			}
		}
		secret := newSecret()
		graceEnd := now.Add(RotationGrace)
		if c.ExpiresAt != nil && c.ExpiresAt.Before(graceEnd) {
			graceEnd = *c.ExpiresAt
		}
		c.PreviousSecretHash, c.PreviousSecretExpiresAt = c.SecretHash, &graceEnd
		c.SecretHash = HashSecret(secret)
		c.RotatedAt, c.RotatedBy = &now, &g.UserID
		c.Version = expectedVersion
		if err := tx.RotateCredential(ctx, c); err != nil {
			return err
		}
		c.Version++
		out = Issued{Credential: c, Token: FormatToken(c.KeyID, secret)}
		return tx.Audit(ctx, audit.New(tenantID, now, g.Actor, "api_credential.rotated", "api_credential", c.ID.String(),
			map[string]any{"prefix": c.Prefix(), "previous_secret_valid_until": graceEnd}))
	})
	if err != nil {
		return Issued{}, err
	}
	return out, nil
}

// Revoke invalida a credencial imediatamente (segredo vigente e anterior),
// condicionada à versão esperada (If-Match). Revogar uma credencial já
// revogada, na versão informada, não tem efeito.
func (s *Service) Revoke(ctx context.Context, tenantID uuid.UUID, g Grantor, id uuid.UUID, expectedVersion int) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		c, err := tx.Credential(ctx, id, true)
		if err != nil {
			return err
		}
		if c.RevokedAt != nil {
			if c.Version != expectedVersion {
				return errVersionMismatch
			}
			return nil
		}
		c.RevokedAt, c.RevokedBy, c.Version = &now, &g.UserID, expectedVersion
		if err := tx.RevokeCredential(ctx, c); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, g.Actor, "api_credential.revoked", "api_credential", c.ID.String(),
			map[string]any{"prefix": c.Prefix()}))
	})
}

// errVersionMismatch recusa a alteração de uma credencial cuja versão difere
// da informada no If-Match.
var errVersionMismatch = apperr.PreconditionFailed("version_mismatch", "A credencial foi alterada por outra pessoa. Recarregue e tente novamente.")

var errInvalidCredential = apperr.Unauthenticated("api_credential_invalid", "Credencial de API inválida, expirada ou revogada.")

func (s *Service) failureKey(clientKey string) string { return "api_credential:" + clientKey }

// blocked informa se a origem excedeu as falhas da janela. Falha do contador
// libera a tentativa: o limite é proteção auxiliar e a credencial continua
// sendo verificada.
func (s *Service) blocked(ctx context.Context, clientKey string) bool {
	if s.failures == nil || clientKey == "" || s.policy.MaxFailures <= 0 {
		return false
	}
	n, err := s.failures.Failures(ctx, s.failureKey(clientKey))
	return err == nil && n >= s.policy.MaxFailures
}

func (s *Service) recordFailure(ctx context.Context, clientKey string) {
	if s.failures == nil || clientKey == "" || s.policy.MaxFailures <= 0 {
		return
	}
	_ = s.failures.RecordFailure(ctx, s.failureKey(clientKey), s.policy.FailureWindow) // melhor esforço (ver blocked)
}

// Authenticate valida o token apresentado em Authorization: Bearer. A
// credencial precisa estar ativa, o segredo precisa coincidir (vigente ou
// anterior dentro da janela de rotação), a organização precisa estar ativa,
// o plano da plataforma precisa incluir acesso à API e quem emitiu o
// segredo vigente precisa continuar membro ativo. clientKey identifica a
// origem para a limitação de tentativas inválidas.
func (s *Service) Authenticate(ctx context.Context, token, clientKey string) (Principal, error) {
	if s.blocked(ctx, clientKey) {
		return Principal{}, apperr.RateLimited("authentication_attempts_exceeded",
			"Muitas tentativas de autenticação inválidas. Aguarde alguns minutos e tente novamente.")
	}
	p, err := s.authenticate(ctx, token)
	if errors.Is(err, errInvalidCredential) {
		s.recordFailure(ctx, clientKey)
	}
	return p, err
}

func (s *Service) authenticate(ctx context.Context, token string) (Principal, error) {
	keyID, secret, ok := ParseToken(token)
	if !ok {
		return Principal{}, errInvalidCredential
	}
	tenantID, credentialID, err := s.store.FindCredential(ctx, keyID)
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return Principal{}, errInvalidCredential
		}
		return Principal{}, err
	}
	now := s.clock.Now()
	var p Principal
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		ac, err := tx.AuthContext(ctx, credentialID)
		if err != nil {
			if apperr.IsKind(err, apperr.KindNotFound) {
				return errInvalidCredential
			}
			return err
		}
		c := ac.Credential
		if !c.MatchSecret(secret, now) || c.State(now) != StateActive {
			return errInvalidCredential
		}
		if ac.TenantStatus != tenancy.TenantActive {
			return apperr.Forbidden("organization_inactive", "Esta organização não está ativa.")
		}
		if !slices.Contains(ac.PlanFeatures, platformbilling.FeatureAPIAccess) {
			return apperr.Forbidden("platform_feature_unavailable", "O plano atual da plataforma não inclui acesso à API.")
		}
		if !ac.IssuerActive {
			return apperr.Forbidden("api_credential_issuer_inactive",
				"Quem emitiu esta credencial não participa mais da organização. Rotacione a credencial com um membro ativo.")
		}
		if c.LastUsedAt == nil || !now.Before(c.LastUsedAt.Add(LastUsedResolution)) {
			if err := tx.TouchCredential(ctx, c.ID, now, now.Add(-LastUsedResolution)); err != nil {
				return err
			}
		}
		p = Principal{CredentialID: c.ID, TenantID: tenantID, Name: c.Name, KeyID: c.KeyID, Scopes: EffectiveScopes(c.Scopes, ac.IssuerRole)}
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}
