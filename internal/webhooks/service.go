package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/tenantscan"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// ConsumerFanout identifica o consumidor que cria as entregas de cada evento.
const ConsumerFanout = "webhooks.fanout"

// Limites de configuração.
const (
	DefaultMaxEndpoints   = 20
	DefaultRotationWindow = 24 * time.Hour
	MaxRotationWindow     = 168 * time.Hour
)

// Padrões do isolamento entre tenants nas entregas.
const (
	DefaultTenantBudget     = 20 * time.Second
	DefaultRunBudget        = time.Minute
	DefaultBreakerThreshold = 3
	DefaultBreakerPause     = 5 * time.Minute
)

// recordTimeout limita a gravação do resultado de uma tentativa, feita mesmo
// com o job cancelado (desligamento do worker ou tempo limite da tarefa).
const recordTimeout = 10 * time.Second

// DeliveryPage é uma página de entregas.
type DeliveryPage struct {
	Items      []Delivery
	NextCursor string
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)

	Endpoints(ctx context.Context, now time.Time) ([]Endpoint, error)
	Endpoint(ctx context.Context, id uuid.UUID, forUpdate bool, now time.Time) (Endpoint, error)
	// LockEndpoints serializa, por tenant, a criação de endpoints (contagem
	// do limite); o bloqueio é liberado no fim da transação.
	LockEndpoints(ctx context.Context) error
	CountEndpoints(ctx context.Context) (int, error)
	InsertEndpoint(ctx context.Context, e Endpoint, createdBy *uuid.UUID) error
	UpdateEndpoint(ctx context.Context, e Endpoint, expectedVersion int) error
	// InsertSecret cifra e grava o segredo.
	InsertSecret(ctx context.Context, endpointID uuid.UUID, s Secret) error
	// ExpireSecrets define a validade at para os segredos válidos depois de at.
	ExpireSecrets(ctx context.Context, endpointID uuid.UUID, at time.Time) error
	// RetainSecrets encerra em now os segredos válidos além dos keep mais
	// recentes (o corrente primeiro e depois do mais recente ao mais antigo).
	RetainSecrets(ctx context.Context, endpointID uuid.UUID, keep int, now time.Time) error
	// Secrets devolve os segredos válidos em now, decifrados: o corrente
	// primeiro e depois do mais recente ao mais antigo. Um segredo que não
	// decifra vem com Unavailable, sem interromper a leitura dos demais.
	Secrets(ctx context.Context, endpointID uuid.UUID, now time.Time) ([]Secret, error)
	SubscribedEndpoints(ctx context.Context, eventType string) ([]uuid.UUID, error)

	InsertEvent(ctx context.Context, e Event) (bool, error)
	Event(ctx context.Context, id uuid.UUID) (Event, error)
	InsertDelivery(ctx context.Context, d Delivery) (bool, error)
	Deliveries(ctx context.Context, endpointID uuid.UUID, limit int, cursor string) (DeliveryPage, error)
	Delivery(ctx context.Context, id uuid.UUID, forUpdate bool) (Delivery, error)
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	// UpdateDelivery grava a entrega se o número de tentativas ainda for
	// expectedAttempts; false indica que outra execução a reivindicou.
	UpdateDelivery(ctx context.Context, d Delivery, expectedAttempts int) (bool, error)
	InsertAttempt(ctx context.Context, a Attempt) error

	MarkInbox(ctx context.Context, consumer string, messageID uuid.UUID) (bool, error)
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações e descobre entregas pendentes.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	// TenantsWithDueDeliveries devolve até limit tenants com entregas a
	// tentar e ID maior que after (nil: desde o início), em ordem de ID.
	TenantsWithDueDeliveries(ctx context.Context, now time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Request é a requisição HTTP de uma entrega.
type Request struct {
	URL    string
	Header map[string]string
	Body   []byte
}

// Transport envia a requisição com as proteções de rede (SSRF, limites de
// tempo e de resposta, sem redirecionamentos) e devolve o status HTTP.
type Transport interface {
	Post(ctx context.Context, r Request) (int, error)
}

// DestinationValidator resolve o destino e rejeita endereços não permitidos.
type DestinationValidator interface {
	Validate(ctx context.Context, rawURL string) error
}

// Config parametriza o serviço.
type Config struct {
	RequireHTTPS bool
	MaxEndpoints int
	// Lease reservado a uma tentativa; deve exceder Timeout.
	Lease     time.Duration
	Timeout   time.Duration
	BatchSize int
	// IsPermanent classifica erros do transporte que não devem ser repetidos.
	IsPermanent func(error) bool
	Jitter      func() float64
	UserAgent   string
	// TenantBudget limita o tempo de uma execução dedicado a um tenant:
	// esgotado, as entregas reivindicadas e ainda não tentadas voltam à fila
	// sem consumir tentativa.
	TenantBudget time.Duration
	// RunBudget limita o tempo de uma execução: esgotado, os tenants
	// restantes ficam para a execução seguinte, que começa por eles.
	RunBudget time.Duration
	// BreakerThreshold é o número de tentativas seguidas sem resposta do
	// destino (tempo esgotado, conexão recusada) que suspende as tentativas
	// ao endpoint na execução; as entregas restantes dele são adiadas por
	// BreakerPause, sem consumir tentativa.
	BreakerThreshold int
	BreakerPause     time.Duration
	// TenantPageSize é o tamanho das páginas de descoberta de tenants.
	TenantPageSize int
	Log            *slog.Logger
}

// Service implementa endpoints, fan-out de eventos e entregas.
type Service struct {
	store     Store
	transport Transport
	validator DestinationValidator
	clock     clock.Clock
	cfg       Config
	rotation  tenantscan.Rotation
}

// NewService cria o serviço. transport pode ser nulo em executáveis que não
// entregam webhooks (API).
func NewService(store Store, transport Transport, validator DestinationValidator, c clock.Clock, cfg Config) *Service {
	if cfg.MaxEndpoints <= 0 {
		cfg.MaxEndpoints = DefaultMaxEndpoints
	}
	if cfg.Lease <= 0 {
		cfg.Lease = time.Minute
	}
	if cfg.Timeout <= 0 || cfg.Timeout >= cfg.Lease {
		cfg.Timeout = cfg.Lease / 2
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 50
	}
	if cfg.IsPermanent == nil {
		cfg.IsPermanent = func(error) bool { return false }
	}
	if cfg.Jitter == nil {
		cfg.Jitter = rand.Float64 //nolint:gosec // jitter não exige aleatoriedade criptográfica
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "Recorvia-Webhooks/1.0"
	}
	if cfg.TenantBudget <= 0 {
		cfg.TenantBudget = DefaultTenantBudget
	}
	if cfg.RunBudget <= 0 {
		cfg.RunBudget = DefaultRunBudget
	}
	if cfg.BreakerThreshold <= 0 {
		cfg.BreakerThreshold = DefaultBreakerThreshold
	}
	if cfg.BreakerPause <= 0 {
		cfg.BreakerPause = DefaultBreakerPause
	}
	if cfg.TenantPageSize <= 0 {
		cfg.TenantPageSize = tenantscan.PageSize
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, transport: transport, validator: validator, clock: c, cfg: cfg}
}

func (s *Service) rules() URLRules { return URLRules{RequireHTTPS: s.cfg.RequireHTTPS} }

func (s *Service) checkDestination(ctx context.Context, rawURL string) error {
	if s.validator == nil {
		return nil
	}
	if err := s.validator.Validate(ctx, rawURL); err != nil {
		s.cfg.Log.InfoContext(ctx, "destino de webhook recusado", slog.String("error", err.Error()))
		return apperr.Invalid("webhook_destination_not_allowed", "O destino do webhook não é permitido ou não pôde ser resolvido.",
			apperr.FieldError{Field: "url", Message: "Use um endereço público acessível pela internet."})
	}
	return nil
}

// liveEndpoint carrega um endpoint não removido (removido: 404).
func liveEndpoint(ctx context.Context, tx Tx, id uuid.UUID, forUpdate bool, now time.Time) (Endpoint, error) {
	e, err := tx.Endpoint(ctx, id, forUpdate, now)
	if err != nil {
		return Endpoint{}, err
	}
	if e.DeletedAt != nil {
		return Endpoint{}, errEndpointNotFound()
	}
	return e, nil
}

func errEndpointNotFound() error {
	return apperr.NotFound("webhook_endpoint_not_found", "Endpoint de webhook não encontrado.")
}

func requireFeature(ctx context.Context, tx Tx) error {
	plan, err := tx.PlatformPlan(ctx)
	if err != nil {
		return err
	}
	return platformbilling.RequireFeature(plan, platformbilling.FeatureOutboundWebhooks)
}

// Endpoints lista os endpoints não removidos com os segredos válidos.
func (s *Service) Endpoints(ctx context.Context, tenantID uuid.UUID) ([]Endpoint, error) {
	var out []Endpoint
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = tx.Endpoints(ctx, s.clock.Now())
		return err
	})
	return out, err
}

// Endpoint devolve um endpoint não removido com os segredos válidos.
func (s *Service) Endpoint(ctx context.Context, tenantID, id uuid.UUID) (Endpoint, error) {
	var out Endpoint
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		out, err = liveEndpoint(ctx, tx, id, false, s.clock.Now())
		return err
	})
	return out, err
}

// CreateEndpoint cria o endpoint com o primeiro segredo, exibido uma única vez.
func (s *Service) CreateEndpoint(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in Input) (Endpoint, string, error) {
	in, err := ValidateInput(in, s.rules())
	if err != nil {
		return Endpoint{}, "", err
	}
	if err := s.checkDestination(ctx, in.URL); err != nil {
		return Endpoint{}, "", err
	}
	now := s.clock.Now()
	e := Endpoint{ID: ids.New(), TenantID: tenantID, URL: in.URL, Description: in.Description, EventTypes: in.EventTypes,
		Status: Active, CreatedAt: now, UpdatedAt: now, Version: 1}
	secret := Secret{SecretInfo: SecretInfo{ID: ids.New(), CreatedAt: now}, Value: NewSecretValue()}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if err := requireFeature(ctx, tx); err != nil {
			return err
		}
		// Sem o bloqueio, criações simultâneas contariam o mesmo total e
		// ultrapassariam o limite.
		if err := tx.LockEndpoints(ctx); err != nil {
			return err
		}
		n, err := tx.CountEndpoints(ctx)
		if err != nil {
			return err
		}
		if n >= s.cfg.MaxEndpoints {
			return apperr.LimitExceeded("webhook_endpoint_limit", fmt.Sprintf("A organização atingiu o limite de %d endpoints de webhook.", s.cfg.MaxEndpoints))
		}
		if err := tx.InsertEndpoint(ctx, e, userID); err != nil {
			return err
		}
		if err := tx.InsertSecret(ctx, e.ID, secret); err != nil {
			return err
		}
		if e, err = tx.Endpoint(ctx, e.ID, false, now); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "webhook_endpoint.created", "webhook_endpoint", e.ID.String(),
			map[string]any{"event_types": e.EventTypes}))
	})
	if err != nil {
		return Endpoint{}, "", err
	}
	return e, secret.Value, nil
}

// UpdateEndpoint altera URL, descrição, eventos e situação (If-Match).
func (s *Service) UpdateEndpoint(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, in Input, status string, expectedVersion int) (Endpoint, error) {
	st, err := ParseStatus(status)
	if err != nil {
		return Endpoint{}, err
	}
	if in, err = ValidateInput(in, s.rules()); err != nil {
		return Endpoint{}, err
	}
	now := s.clock.Now()
	var e Endpoint
	checked := false
	for {
		retry := false
		err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
			var err error
			if e, err = liveEndpoint(ctx, tx, id, true, now); err != nil {
				return err
			}
			if expectedVersion != 0 && e.Version != expectedVersion {
				return apperr.PreconditionFailed("version_mismatch", "O endpoint foi alterado por outra pessoa. Recarregue e tente novamente.")
			}
			// Somente um endpoint que fica ativo exige o recurso no plano:
			// desativar é sempre permitido (por exemplo, após um downgrade).
			if st == Active {
				if err := requireFeature(ctx, tx); err != nil {
					return err
				}
			}
			if (in.URL != e.URL || (st == Active && e.Status != Active)) && !checked {
				retry = true // valida o destino fora da transação e repete
				return nil
			}
			changed := map[string]any{"url_changed": in.URL != e.URL, "status": st}
			e.URL, e.Description, e.EventTypes, e.Status, e.UpdatedAt = in.URL, in.Description, in.EventTypes, st, now
			if err := tx.UpdateEndpoint(ctx, e, e.Version); err != nil {
				return err
			}
			e.Version++
			return tx.Audit(ctx, audit.New(tenantID, now, actor, "webhook_endpoint.updated", "webhook_endpoint", e.ID.String(), changed))
		})
		if err != nil || !retry {
			return e, err
		}
		if err := s.checkDestination(ctx, in.URL); err != nil {
			return Endpoint{}, err
		}
		checked = true
	}
}

// DeleteEndpoint desativa o endpoint e o retira da listagem. Entregas
// pendentes deixam de ser tentadas.
func (s *Service) DeleteEndpoint(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		e, err := liveEndpoint(ctx, tx, id, true, now)
		if err != nil {
			return err
		}
		e.Status, e.DeletedAt, e.UpdatedAt = Disabled, &now, now
		if err := tx.UpdateEndpoint(ctx, e, e.Version); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "webhook_endpoint.deleted", "webhook_endpoint", e.ID.String(), nil))
	})
}

// RotateSecret gera um novo segredo. Os segredos anteriores continuam
// válidos por previousValidHours (padrão: 24 h; 0 encerra imediatamente),
// período em que as entregas levam as duas assinaturas. Valem no máximo
// MaxSignatures segredos ao mesmo tempo: os anteriores mais antigos além
// desse limite são encerrados na rotação, de modo que a listagem coincide
// com os segredos que assinam as entregas.
func (s *Service) RotateSecret(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID, previousValidHours *int) (Endpoint, string, error) {
	window := DefaultRotationWindow
	if previousValidHours != nil {
		// O inteiro é validado antes da conversão: a multiplicação por
		// time.Hour estouraria para valores grandes.
		h := *previousValidHours
		if h < 0 || h > int(MaxRotationWindow/time.Hour) {
			return Endpoint{}, "", apperr.Invalid("invalid_rotation_window", "Janela de rotação inválida.",
				apperr.FieldError{Field: "previous_secret_valid_hours", Message: "Informe entre 0 e 168 horas."})
		}
		window = time.Duration(h) * time.Hour
	}
	now := s.clock.Now()
	secret := Secret{SecretInfo: SecretInfo{ID: ids.New(), CreatedAt: now}, Value: NewSecretValue()}
	var e Endpoint
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if e, err = liveEndpoint(ctx, tx, id, true, now); err != nil {
			return err
		}
		if err := requireFeature(ctx, tx); err != nil {
			return err
		}
		if err := tx.ExpireSecrets(ctx, e.ID, now.Add(window)); err != nil {
			return err
		}
		if err := tx.RetainSecrets(ctx, e.ID, MaxSignatures-1, now); err != nil {
			return err
		}
		if err := tx.InsertSecret(ctx, e.ID, secret); err != nil {
			return err
		}
		if e, err = tx.Endpoint(ctx, e.ID, false, now); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "webhook_endpoint.secret_rotated", "webhook_endpoint", e.ID.String(),
			map[string]any{"secret_id": secret.ID, "previous_valid_hours": int(window / time.Hour)}))
	})
	if err != nil {
		return Endpoint{}, "", err
	}
	return e, secret.Value, nil
}

// Deliveries lista as entregas de um endpoint.
func (s *Service) Deliveries(ctx context.Context, tenantID, endpointID uuid.UUID, limit int, cursor string) (DeliveryPage, error) {
	var p DeliveryPage
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := liveEndpoint(ctx, tx, endpointID, false, s.clock.Now()); err != nil {
			return err
		}
		var err error
		p, err = tx.Deliveries(ctx, endpointID, limit, cursor)
		return err
	})
	return p, err
}

// RetryDelivery agenda o reenvio manual de uma entrega (auditado).
func (s *Service) RetryDelivery(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, id uuid.UUID) (Delivery, error) {
	now := s.clock.Now()
	var d Delivery
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if d, err = tx.Delivery(ctx, id, true); err != nil {
			return err
		}
		e, err := tx.Endpoint(ctx, d.EndpointID, false, now)
		if err != nil {
			return err
		}
		if !e.Deliverable() {
			return apperr.Conflict("webhook_endpoint_disabled", "O endpoint está desativado.")
		}
		if err := requireFeature(ctx, tx); err != nil {
			return err
		}
		previous := d.Status
		if err := d.ScheduleManualRetry(now); err != nil {
			return err
		}
		ok, err := tx.UpdateDelivery(ctx, d, d.Attempts)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.Conflict("delivery_in_progress", "A entrega está em andamento.")
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "webhook_delivery.retried", "webhook_delivery", d.ID.String(),
			map[string]any{"endpoint_id": d.EndpointID, "event_id": d.EventID, "previous_status": previous}))
	})
	return d, err
}

// HandleEvent cria as entregas de um evento de domínio para os endpoints
// ativos que o assinam (consumidor com inbox). Eventos não assináveis, tenants
// sem o recurso no plano ou sem endpoints interessados não têm efeito (nem
// registro no inbox).
func (s *Service) HandleEvent(ctx context.Context, tenantID, messageID uuid.UUID, eventType string, occurredAt time.Time, payload []byte) error {
	if !Subscribable(eventType) {
		return nil
	}
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		plan, err := tx.PlatformPlan(ctx)
		if err != nil || !plan.HasFeature(platformbilling.FeatureOutboundWebhooks) {
			return err
		}
		endpoints, err := tx.SubscribedEndpoints(ctx, eventType)
		if err != nil || len(endpoints) == 0 {
			return err
		}
		first, err := tx.MarkInbox(ctx, ConsumerFanout, messageID)
		if err != nil || !first {
			return err
		}
		if !json.Valid(payload) {
			payload = []byte("{}")
		}
		if occurredAt.IsZero() {
			occurredAt = now
		}
		if _, err := tx.InsertEvent(ctx, Event{ID: messageID, Type: eventType, Payload: payload, OccurredAt: occurredAt.UTC()}); err != nil {
			return err
		}
		for _, id := range endpoints {
			if _, err := tx.InsertDelivery(ctx, NewDelivery(ids.New(), id, messageID, eventType, now)); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeliverResult resume uma execução das entregas.
type DeliverResult struct {
	Succeeded int
	Retrying  int
	Exhausted int
	LostLease int
	// Deferred conta as entregas reivindicadas e devolvidas à fila sem
	// tentativa (orçamento esgotado, cancelamento ou endpoint sem resposta).
	Deferred int
}

func (r *DeliverResult) add(o DeliverResult) {
	r.Succeeded += o.Succeeded
	r.Retrying += o.Retrying
	r.Exhausted += o.Exhausted
	r.LostLease += o.LostLease
	r.Deferred += o.Deferred
}

// Deliver executa as entregas vencidas de todos os tenants. Cada entrega é
// reivindicada com lease (SKIP LOCKED) numa transação curta; a requisição
// ocorre fora de transação e o resultado só é gravado por quem detém a
// tentativa corrente. Um tenant com destinos lentos não retém os demais:
// cada tenant tem um orçamento de tempo por execução, a execução tem o seu
// (RunBudget) e a descoberta começa depois do último tenant da execução
// anterior (rodízio).
func (s *Service) Deliver(ctx context.Context) (DeliverResult, error) {
	var res DeliverResult
	if s.transport == nil {
		return res, errors.New("webhooks: transporte não configurado")
	}
	now := s.clock.Now()
	end := now.Add(s.cfg.RunBudget)
	page := func(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
		return s.store.TenantsWithDueDeliveries(ctx, now, after, limit)
	}
	var errs []error
	err := s.rotation.Each(ctx, s.cfg.TenantPageSize, page, func() bool { return s.clock.Now().Before(end) }, func(t uuid.UUID) {
		r, err := s.DeliverTenant(ctx, t)
		res.add(r)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", t, err))
		}
	})
	if err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

type job struct {
	d       Delivery
	before  int // tentativas antes da reivindicação
	url     string
	body    []byte
	eventID string
	secrets []string
}

// signingSecrets são os segredos utilizáveis de um endpoint.
type signingSecrets struct {
	values      []string
	unavailable int
}

// DeliverTenant executa as entregas vencidas de um tenant: reivindica um lote
// numa transação curta e faz as tentativas em sequência dentro do orçamento
// do tenant (TenantBudget). Entregas não tentadas (orçamento esgotado, job
// cancelado ou endpoint com tentativas seguidas sem resposta) voltam à fila
// sem consumir tentativa.
func (s *Service) DeliverTenant(ctx context.Context, tenantID uuid.UUID) (DeliverResult, error) {
	var res DeliverResult
	var jobs []job
	now := s.clock.Now()
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		jobs, res = nil, DeliverResult{}
		plan, err := tx.PlatformPlan(ctx)
		if err != nil {
			return err
		}
		due, err := tx.DueDeliveries(ctx, now, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		endpoints := map[uuid.UUID]Endpoint{}
		secretsByEndpoint := map[uuid.UUID]signingSecrets{}
		for _, d := range due {
			before := d.Attempts
			e, ok := endpoints[d.EndpointID]
			if !ok {
				if e, err = tx.Endpoint(ctx, d.EndpointID, false, now); err != nil && !apperr.IsKind(err, apperr.KindNotFound) {
					return err
				}
				endpoints[d.EndpointID] = e
			}
			var j job
			switch {
			case !e.Deliverable():
				d.Exhaust(now, ReasonEndpointDisabled)
			case !plan.HasFeature(platformbilling.FeatureOutboundWebhooks):
				d.Exhaust(now, ReasonFeatureUnavailable)
			default:
				secrets, ok := secretsByEndpoint[e.ID]
				if !ok {
					if secrets, err = s.signingSecrets(ctx, tx, e.ID, now); err != nil {
						return err
					}
					secretsByEndpoint[e.ID] = secrets
				}
				if len(secrets.values) == 0 {
					reason := ReasonNoSecret
					if secrets.unavailable > 0 {
						reason = ReasonSecretUnavailable
					}
					d.Exhaust(now, reason)
					break
				}
				ev, err := tx.Event(ctx, d.EventID)
				if err != nil {
					return err
				}
				body, err := ev.Body()
				if err != nil {
					return err
				}
				if err := d.Claim(now, now.Add(s.cfg.Lease)); err != nil {
					return err
				}
				j = job{d: d, before: before, url: e.URL, body: body, eventID: ev.ID.String(), secrets: secrets.values}
			}
			updated, err := tx.UpdateDelivery(ctx, d, before)
			if err != nil {
				return err
			}
			switch {
			case !updated:
				res.LostLease++
			case d.Status == Exhausted:
				res.Exhausted++
			default:
				jobs = append(jobs, j)
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	end := now.Add(s.cfg.TenantBudget)
	noResponse := map[uuid.UUID]int{}
	var released []release
	var errs []error
	for _, j := range jobs {
		at := s.clock.Now()
		switch {
		case ctx.Err() != nil || !at.Before(end):
			released = append(released, release{j: j, next: at}) // volta à fila para a próxima execução
			continue
		case noResponse[j.d.EndpointID] >= s.cfg.BreakerThreshold:
			released = append(released, release{j: j, next: at.Add(s.cfg.BreakerPause)})
			continue
		}
		r, answered, err := s.attempt(ctx, tenantID, j)
		res.add(r)
		if err != nil {
			errs = append(errs, err)
		}
		if answered {
			noResponse[j.d.EndpointID] = 0
		} else {
			noResponse[j.d.EndpointID]++
		}
	}
	if len(released) > 0 {
		r, err := s.release(ctx, tenantID, released)
		res.add(r)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// signingSecrets lê os segredos válidos do endpoint. Segredos que não
// decifram (chave retirada do keyring ou dado corrompido) são ignorados,
// sem interromper as entregas do tenant.
func (s *Service) signingSecrets(ctx context.Context, tx Tx, endpointID uuid.UUID, now time.Time) (signingSecrets, error) {
	list, err := tx.Secrets(ctx, endpointID, now)
	if err != nil {
		return signingSecrets{}, err
	}
	var out signingSecrets
	for _, sec := range list {
		if sec.Unavailable {
			out.unavailable++
			continue
		}
		out.values = append(out.values, sec.Value)
	}
	if out.unavailable > 0 {
		s.cfg.Log.WarnContext(ctx, "segredos de assinatura indisponíveis ignorados", slog.String("endpoint_id", endpointID.String()),
			slog.Int("count", out.unavailable))
	}
	return out, nil
}

// release é uma entrega reivindicada que volta à fila sem tentativa.
type release struct {
	j    job
	next time.Time
}

// release devolve à fila as entregas reivindicadas e não tentadas, sem
// consumir a tentativa (gravação feita mesmo com o job cancelado).
func (s *Service) release(ctx context.Context, tenantID uuid.UUID, items []release) (DeliverResult, error) {
	var res DeliverResult
	now := s.clock.Now()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err := s.store.InTenant(wctx, tenantID, func(tx Tx) error {
		res = DeliverResult{}
		for _, it := range items {
			d := it.j.d
			if err := d.Release(it.j.before, now, it.next); err != nil {
				return err
			}
			ok, err := tx.UpdateDelivery(wctx, d, it.j.d.Attempts)
			if err != nil {
				return err
			}
			if ok {
				res.Deferred++
			} else {
				res.LostLease++
			}
		}
		return nil
	})
	return res, err
}

// attempt faz uma tentativa e grava o resultado. answered informa se o
// destino respondeu (ou recusou de forma permanente); falhas sem resposta
// alimentam o disjuntor do endpoint.
func (s *Service) attempt(ctx context.Context, tenantID uuid.UUID, j job) (res DeliverResult, answered bool, err error) {
	started := s.clock.Now()
	sig, count, err := SignatureHeader(j.secrets, j.eventID, started, j.body)
	if err != nil {
		return res, true, err
	}
	req := Request{URL: j.url, Body: j.body, Header: map[string]string{
		"Content-Type": "application/json", "User-Agent": s.cfg.UserAgent,
		HeaderID: j.eventID, HeaderTimestamp: strconv.FormatInt(started.Unix(), 10), HeaderSignature: sig, HeaderEventType: j.d.EventType,
	}}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	code, callErr := s.transport.Post(callCtx, req)
	cancel()
	finished := s.clock.Now()
	o := Outcome{StatusCode: code}
	if callErr != nil {
		o.Err, o.Permanent = callErr.Error(), s.cfg.IsPermanent(callErr)
	}
	answered = callErr == nil || o.Permanent
	// O resultado é gravado mesmo com o job cancelado: sem o registro, uma
	// entrega aceita pelo destino seria reenviada após o lease.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	err = s.store.InTenant(wctx, tenantID, func(tx Tx) error {
		res = DeliverResult{}
		cur, err := tx.Delivery(wctx, j.d.ID, true)
		if err != nil {
			return err
		}
		if cur.Status != Delivering || cur.Attempts != j.d.Attempts {
			res.LostLease++
			return nil
		}
		if err := cur.Record(finished, o, s.cfg.Jitter()); err != nil {
			return err
		}
		ok, err := tx.UpdateDelivery(wctx, cur, j.d.Attempts)
		if err != nil {
			return err
		}
		if !ok {
			res.LostLease++
			return nil
		}
		switch cur.Status {
		case Succeeded:
			res.Succeeded++
		case Retrying:
			res.Retrying++
		default:
			res.Exhausted++
		}
		a := Attempt{ID: ids.New(), DeliveryID: cur.ID, Number: cur.Attempts, StartedAt: started, Duration: max(finished.Sub(started), 0),
			Error: cur.LastError, SignatureCount: count}
		if code > 0 {
			a.StatusCode = &code
		}
		return tx.InsertAttempt(wctx, a)
	})
	if callErr != nil || !o.Success() {
		s.cfg.Log.InfoContext(ctx, "entrega de webhook sem sucesso", slog.String("tenant_id", tenantID.String()),
			slog.String("delivery_id", j.d.ID.String()), slog.Int("attempt", j.d.Attempts), slog.Int("status_code", code),
			slog.Bool("permanent", o.Permanent))
	}
	return res, answered, err
}
