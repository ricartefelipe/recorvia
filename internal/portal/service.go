package portal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/customers"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	Tenant(ctx context.Context) (tenancy.Tenant, error)
	// Customer lê o cliente; lock serializa a geração de links e as
	// alterações de preferências do cliente.
	Customer(ctx context.Context, id uuid.UUID, lock bool) (customers.Customer, error)
	Contacts(ctx context.Context, customerID uuid.UUID) ([]customers.Contact, error)
	InsertLink(ctx context.Context, l Link) error
	// RevokeActiveLinks revoga os links não revogados do cliente e devolve
	// os identificadores afetados.
	RevokeActiveLinks(ctx context.Context, customerID uuid.UUID, at time.Time, reason string) ([]uuid.UUID, error)
	Link(ctx context.Context, id uuid.UUID, forUpdate bool) (Link, error)
	MarkLinkUsed(ctx context.Context, id uuid.UUID, at time.Time) error
	// Invoices lista as faturas emitidas do cliente, com itens, das mais
	// recentes para as mais antigas (no máximo limit).
	Invoices(ctx context.Context, customerID uuid.UUID, limit int) ([]billing.Invoice, error)
	// Invoice lê uma fatura emitida do cliente, com itens; fatura de outro
	// cliente ou em rascunho resulta em NotFound.
	Invoice(ctx context.Context, customerID, id uuid.UUID) (billing.Invoice, error)
	// CheckoutURLs devolve, por fatura, o link do pagamento hospedado da
	// cobrança mais recente que aguarda pagamento.
	CheckoutURLs(ctx context.Context, invoiceIDs []uuid.UUID) (map[uuid.UUID]string, error)
	// SetContactPreference altera a preferência de e-mail de cobrança de um
	// contato do cliente; contato de outro cliente resulta em NotFound.
	SetContactPreference(ctx context.Context, customerID, contactID uuid.UUID, receivesBillingEmail bool) error
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações do tenant e localiza links pelo hash do token, sem
// contexto de tenant.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	FindLink(ctx context.Context, tokenHash []byte) (tenantID, linkID uuid.UUID, err error)
}

// SessionStore guarda as sessões do portal (fora do banco).
type SessionStore interface {
	Save(ctx context.Context, s Session, ttl time.Duration) error
	// Get retorna ErrSessionNotFound para sessão inexistente ou expirada.
	Get(ctx context.Context, id string) (Session, error)
	Delete(ctx context.Context, id string) error
}

// FailureCounter conta tentativas de acesso inválidas por origem em janela
// fixa.
type FailureCounter interface {
	Failures(ctx context.Context, key string) (int, error)
	RecordFailure(ctx context.Context, key string, window time.Duration) error
}

// Policy define validades e limites do portal.
type Policy struct {
	// LinkTTL é a validade do link a partir da geração.
	LinkTTL time.Duration
	// SessionTTL é a duração máxima da sessão desde a troca do link.
	SessionTTL time.Duration
	// SessionIdle encerra a sessão após inatividade.
	SessionIdle time.Duration
	// TouchInterval evita regravar a sessão a cada requisição.
	TouchInterval time.Duration
	// MaxFailures bloqueia a origem após este número de tentativas
	// inválidas na janela FailureWindow.
	MaxFailures   int
	FailureWindow time.Duration
}

// Limites aceitos para a validade do link (configuração).
const (
	MinLinkTTL = 15 * time.Minute
	MaxLinkTTL = 30 * 24 * time.Hour
)

// DefaultPolicy é a política padrão: link válido por 72 horas, sessão de até
// 2 horas encerrada após 30 minutos sem uso e bloqueio da origem após 10
// tentativas inválidas em 15 minutos.
func DefaultPolicy() Policy {
	return Policy{LinkTTL: 72 * time.Hour, SessionTTL: 2 * time.Hour, SessionIdle: 30 * time.Minute, TouchInterval: time.Minute,
		MaxFailures: 10, FailureWindow: 15 * time.Minute}
}

// Validate verifica a coerência da política.
func (p Policy) Validate() error {
	switch {
	case p.LinkTTL < MinLinkTTL || p.LinkTTL > MaxLinkTTL:
		return fmt.Errorf("portal: validade do link deve ficar entre %s e %s", MinLinkTTL, MaxLinkTTL)
	case p.SessionTTL <= 0 || p.SessionIdle <= 0 || p.SessionIdle > p.SessionTTL:
		return errors.New("portal: inatividade da sessão deve ser positiva e não exceder a duração máxima")
	case p.TouchInterval <= 0 || p.TouchInterval >= p.SessionIdle:
		return errors.New("portal: intervalo de renovação deve ser positivo e menor que a inatividade")
	case p.MaxFailures < 0 || (p.MaxFailures > 0 && p.FailureWindow <= 0):
		return errors.New("portal: limite de tentativas inválido")
	}
	return nil
}

// Service implementa os casos de uso do portal do cliente.
type Service struct {
	store    Store
	sessions SessionStore
	failures FailureCounter
	clock    clock.Clock
	policy   Policy
}

// NewService cria o serviço. failures pode ser nil (sem limitação).
func NewService(store Store, sessions SessionStore, failures FailureCounter, c clock.Clock, p Policy) *Service {
	return &Service{store: store, sessions: sessions, failures: failures, clock: c, policy: p}
}

// Policy devolve a política em uso.
func (s *Service) Policy() Policy { return s.policy }

var (
	errLinkInvalid = apperr.Unauthenticated("portal_link_invalid",
		"Link de acesso inválido, expirado ou já utilizado. Solicite um novo link à empresa.")
	errSessionInvalid = apperr.Unauthenticated("portal_session_invalid",
		"Sua sessão do portal expirou ou foi encerrada. Solicite um novo link de acesso à empresa.")
	errSessionStore = apperr.Unavailable("session_store_unavailable", "Serviço de sessão indisponível. Tente novamente em instantes.")
)

// CreateLink gera um link de uso único para o cliente. Links anteriores do
// mesmo cliente são revogados (os pendentes deixam de funcionar e a sessão
// aberta com um link já usado é encerrada na próxima requisição). O token
// em claro só existe no retorno.
func (s *Service) CreateLink(ctx context.Context, tenantID uuid.UUID, creator Creator, customerID uuid.UUID) (Link, string, error) {
	if (creator.UserID == nil) == (creator.CredentialID == nil) {
		return Link{}, "", errors.New("portal: informe exatamente um autor do link")
	}
	token, hash := tenancy.NewToken()
	now := s.clock.Now()
	l := Link{ID: ids.New(), TenantID: tenantID, CustomerID: customerID, TokenHash: hash, CreatedByUser: creator.UserID,
		CreatedByCredential: creator.CredentialID, CreatedAt: now, ExpiresAt: now.Add(s.policy.LinkTTL)}
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		c, err := tx.Customer(ctx, customerID, true)
		if err != nil {
			return err
		}
		if c.Status != customers.Active {
			return apperr.Conflict("customer_archived", "Cliente arquivado não tem acesso ao portal.")
		}
		superseded, err := tx.RevokeActiveLinks(ctx, customerID, now, RevokeSuperseded)
		if err != nil {
			return err
		}
		if err := tx.InsertLink(ctx, l); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, creator.Actor, "customer.portal_link_created", "customer", customerID.String(),
			map[string]any{"link_id": l.ID, "expires_at": l.ExpiresAt, "superseded_links": len(superseded)}))
	})
	if err != nil {
		return Link{}, "", err
	}
	return l, token, nil
}

// RevokeLinks revoga todos os links não revogados do cliente, encerrando
// também as sessões abertas com eles.
func (s *Service) RevokeLinks(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, customerID uuid.UUID) (int, error) {
	now := s.clock.Now()
	var n int
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.Customer(ctx, customerID, true); err != nil {
			return err
		}
		revoked, err := tx.RevokeActiveLinks(ctx, customerID, now, RevokeRequested)
		if err != nil {
			return err
		}
		n = len(revoked)
		if n == 0 {
			return nil
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "customer.portal_links_revoked", "customer", customerID.String(),
			map[string]any{"links": n}))
	})
	return n, err
}

func (s *Service) failureKey(clientKey string) string { return "portal_access:" + clientKey }

// blocked informa se a origem excedeu as tentativas inválidas. Falha do
// contador libera a tentativa: o limite é proteção auxiliar e o link
// continua sendo verificado.
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

// Exchange troca o token do link por uma sessão do portal. O link é
// consumido na mesma transação que confirma a sessão: a sessão é gravada
// antes do commit e descartada se ele falhar, de modo que uma falha do
// armazenamento de sessões não inutiliza o link. Tentativas inválidas
// (token desconhecido, expirado, revogado ou já usado) contam para o limite
// da origem identificada por clientKey.
func (s *Service) Exchange(ctx context.Context, token, clientKey string) (Session, error) {
	if s.blocked(ctx, clientKey) {
		return Session{}, apperr.RateLimited("portal_access_attempts_exceeded",
			"Muitas tentativas de acesso. Aguarde alguns minutos e tente novamente.")
	}
	sess, err := s.exchange(ctx, token)
	if errors.Is(err, errLinkInvalid) {
		s.recordFailure(ctx, clientKey)
	}
	return sess, err
}

func (s *Service) exchange(ctx context.Context, token string) (Session, error) {
	if !validLinkToken(token) {
		return Session{}, errLinkInvalid
	}
	tenantID, linkID, err := s.store.FindLink(ctx, tenancy.HashToken(token))
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return Session{}, errLinkInvalid
		}
		return Session{}, err
	}
	now := s.clock.Now()
	var sess Session
	saved := false
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		l, err := tx.Link(ctx, linkID, true)
		if err != nil {
			if apperr.IsKind(err, apperr.KindNotFound) {
				return errLinkInvalid
			}
			return err
		}
		if l.State(now) != LinkPending {
			return errLinkInvalid
		}
		if err := s.checkAccess(ctx, tx, l.CustomerID, errLinkInvalid); err != nil {
			return err
		}
		if err := tx.MarkLinkUsed(ctx, l.ID, now); err != nil {
			return err
		}
		sess = Session{ID: randomToken(32), TenantID: tenantID, CustomerID: l.CustomerID, LinkID: l.ID, CSRFToken: randomToken(32),
			CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(s.policy.SessionTTL)}
		if err := tx.Audit(ctx, audit.New(tenantID, now, sess.Actor(), "customer_portal.session_started", "customer", l.CustomerID.String(),
			map[string]any{"link_id": l.ID})); err != nil {
			return err
		}
		if err := s.sessions.Save(ctx, sess, s.ttl(sess, now)); err != nil {
			return errSessionStore.Wrap(err)
		}
		saved = true
		return nil
	})
	if err != nil {
		if saved {
			_ = s.sessions.Delete(context.WithoutCancel(ctx), sess.ID) // melhor esforço: sem commit, a sessão não vale (link não usado)
		}
		return Session{}, err
	}
	return sess, nil
}

// checkAccess confirma que a organização está ativa e que o cliente existe
// e está ativo; caso contrário devolve denied.
func (s *Service) checkAccess(ctx context.Context, tx Tx, customerID uuid.UUID, denied error) error {
	t, err := tx.Tenant(ctx)
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return denied
		}
		return err
	}
	if t.Status != tenancy.TenantActive {
		return denied
	}
	c, err := tx.Customer(ctx, customerID, false)
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return denied
		}
		return err
	}
	if c.Status != customers.Active {
		return denied
	}
	return nil
}

func (s *Service) ttl(sess Session, now time.Time) time.Duration {
	return min(s.policy.SessionIdle, sess.ExpiresAt.Sub(now))
}

// Authenticate valida a sessão do portal: expiração absoluta e por
// inatividade, link de origem ainda não revogado, cliente e organização
// ativos. Sessões inválidas são removidas.
func (s *Service) Authenticate(ctx context.Context, sessionID string) (Session, error) {
	if sessionID == "" {
		return Session{}, errSessionInvalid
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return Session{}, errSessionInvalid
		}
		return Session{}, errSessionStore.Wrap(err)
	}
	now := s.clock.Now()
	if !now.Before(sess.ExpiresAt) || now.Sub(sess.LastSeenAt) >= s.policy.SessionIdle {
		_ = s.sessions.Delete(ctx, sessionID) // melhor esforço; a TTL do armazenamento também expira
		return Session{}, errSessionInvalid
	}
	err = s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		l, err := tx.Link(ctx, sess.LinkID, false)
		if err != nil {
			if apperr.IsKind(err, apperr.KindNotFound) {
				return errSessionInvalid
			}
			return err
		}
		if l.State(now) != LinkUsed || l.CustomerID != sess.CustomerID {
			return errSessionInvalid
		}
		return s.checkAccess(ctx, tx, sess.CustomerID, errSessionInvalid)
	})
	if err != nil {
		if errors.Is(err, errSessionInvalid) {
			_ = s.sessions.Delete(ctx, sessionID) // melhor esforço
		}
		return Session{}, err
	}
	if now.Sub(sess.LastSeenAt) >= s.policy.TouchInterval {
		sess.LastSeenAt = now
		if err := s.sessions.Save(ctx, sess, s.ttl(sess, now)); err != nil {
			return Session{}, errSessionStore.Wrap(err)
		}
	}
	return sess, nil
}

// View é o resumo da sessão exibido ao cliente.
type View struct {
	Session          Session
	CustomerName     string
	OrganizationName string
	Contacts         []customers.Contact
}

// View monta o resumo da sessão (cliente, organização e contatos).
func (s *Service) View(ctx context.Context, sess Session) (View, error) {
	v := View{Session: sess}
	err := s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		t, err := tx.Tenant(ctx)
		if err != nil {
			return err
		}
		c, err := tx.Customer(ctx, sess.CustomerID, false)
		if err != nil {
			return err
		}
		contacts, err := tx.Contacts(ctx, sess.CustomerID)
		if err != nil {
			return err
		}
		v.CustomerName, v.OrganizationName, v.Contacts = c.Name, t.Name, contacts
		return nil
	})
	return v, err
}

// Logout encerra a sessão do portal e registra o encerramento na auditoria.
// A sessão é removida dentro da transação da auditoria: se a remoção falhar,
// nada é registrado e o erro é devolvido.
func (s *Service) Logout(ctx context.Context, sess Session) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, sess.TenantID, func(tx Tx) error {
		if err := tx.Audit(ctx, audit.New(sess.TenantID, now, sess.Actor(), "customer_portal.session_ended", "customer", sess.CustomerID.String(),
			map[string]any{"link_id": sess.LinkID})); err != nil {
			return err
		}
		if err := s.sessions.Delete(ctx, sess.ID); err != nil {
			return errSessionStore.Wrap(err)
		}
		return nil
	})
}
