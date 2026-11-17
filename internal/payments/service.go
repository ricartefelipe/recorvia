package payments

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// Payment é um recebimento aplicado a uma fatura.
type Payment struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	InvoiceID  uuid.UUID
	ChargeID   *uuid.UUID
	CustomerID uuid.UUID
	Amount     money.Money
	Fee        money.Money
	Method     string
	Source     string
	PaidAt     time.Time
	Note       string
	RecordedBy *uuid.UUID
	RequestKey string
	CreatedAt  time.Time
}

// StoredWebhook é um webhook recebido e persistido.
type StoredWebhook struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	ProviderAccountID uuid.UUID
	Provider          string
	EventID           string
	EventType         string
	ProviderChargeID  string
	OccurredAt        *time.Time
	ReceivedAt        time.Time
	Payload           []byte
	Status            string
	Error             string
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	MarkInbox(ctx context.Context, consumer string, messageID uuid.UUID) (bool, error)
	Location(ctx context.Context) (*time.Location, error)
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)

	DefaultAccount(ctx context.Context) (ProviderAccount, error)
	Account(ctx context.Context, id uuid.UUID) (ProviderAccount, error)
	AccountByProvider(ctx context.Context, provider string) (ProviderAccount, error)
	SaveAccount(ctx context.Context, a ProviderAccount, endpointTokenHash []byte) error
	Accounts(ctx context.Context) ([]ProviderAccount, error)

	Invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (billing.Invoice, error)
	UpdateInvoiceProgress(ctx context.Context, inv *billing.Invoice) error
	InsertInvoiceHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []billing.StatusChange) error
	RefreshSubscriptionStanding(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error

	CustomerData(ctx context.Context, customerID uuid.UUID, provider string) (CustomerData, error)
	SaveProviderCustomer(ctx context.Context, customerID uuid.UUID, provider, providerCustomerID string) error

	LatestCharge(ctx context.Context, invoiceID uuid.UUID) (*Charge, error)
	InsertCharge(ctx context.Context, c Charge) error
	Charge(ctx context.Context, id uuid.UUID, forUpdate bool) (Charge, error)
	ChargeByProviderID(ctx context.Context, provider, providerChargeID string, forUpdate bool) (Charge, error)
	UpdateCharge(ctx context.Context, c *Charge) error
	OpenChargesForInvoice(ctx context.Context, invoiceID uuid.UUID) ([]Charge, error)

	InsertOperation(ctx context.Context, op Operation) error
	Operation(ctx context.Context, id uuid.UUID, forUpdate bool) (Operation, error)
	UpdateOperation(ctx context.Context, op Operation) error
	DueOperationIDs(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error)

	InsertPayment(ctx context.Context, p Payment) (bool, error)
	PaymentByRequestKey(ctx context.Context, key string) (Payment, error)
	GrantCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, kind string, invoiceID *uuid.UUID, reason string, at time.Time) error

	InsertWebhook(ctx context.Context, w StoredWebhook) (bool, error)
	Webhook(ctx context.Context, id uuid.UUID, forUpdate bool) (StoredWebhook, error)
	UpdateWebhookStatus(ctx context.Context, id uuid.UUID, status, errText string, at time.Time) error

	PostLedger(ctx context.Context, t ledger.Transaction) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error
}

// Store abre transações e resolve o vínculo de webhooks com tenants.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	ResolveWebhookEndpoint(ctx context.Context, provider string, tokenHash []byte) (tenantID, accountID uuid.UUID, err error)
	TenantsWithPendingOperations(ctx context.Context, now time.Time) ([]uuid.UUID, error)
}

// Config parametriza o serviço.
type Config struct {
	// WebhookBaseURL é a URL pública de recepção (ex.: https://api.exemplo/webhooks/providers).
	WebhookBaseURL string
	// CallTimeout limita cada chamada ao provedor.
	CallTimeout time.Duration
	// Lease deve exceder CallTimeout com folga, para que duas instâncias
	// nunca executem a mesma operação ao mesmo tempo.
	Lease time.Duration
	// Log registra falhas tratadas em melhor esforço (opcional).
	Log *slog.Logger
}

// Service implementa os casos de uso de pagamentos.
type Service struct {
	store    Store
	resolver GatewayResolver
	clock    clock.Clock
	cfg      Config
}

// NewService cria o serviço.
func NewService(store Store, resolver GatewayResolver, c clock.Clock, cfg Config) *Service {
	if cfg.CallTimeout <= 0 {
		cfg.CallTimeout = 20 * time.Second
	}
	if cfg.Lease <= cfg.CallTimeout {
		cfg.Lease = 3 * cfg.CallTimeout
	}
	return &Service{store: store, resolver: resolver, clock: c, cfg: cfg}
}

var (
	systemActor   = audit.System("payments")
	providerActor = audit.Actor{Type: audit.ActorProvider, ID: "provider", Label: "Provedor de pagamento"}
)

// errNothingToDo sinaliza que não há cobrança a criar.
var errNothingToDo = errors.New("payments: nada a fazer")

// RequestCharge cria a cobrança da fatura no provedor padrão do tenant. É
// idempotente: se já houver cobrança em andamento ou paga, retorna a
// existente. Sem provedor configurado, não cria cobrança.
func (s *Service) RequestCharge(ctx context.Context, tenantID, invoiceID uuid.UUID, actor audit.Actor) (*Charge, error) {
	var charge Charge
	var opID uuid.UUID
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.Invoice(ctx, invoiceID, true)
		if err != nil {
			return err
		}
		if (inv.Status != billing.Open && inv.Status != billing.PartiallyPaid) || !inv.AmountDue.IsPositive() {
			return errNothingToDo
		}
		last, err := tx.LatestCharge(ctx, invoiceID)
		if err != nil {
			return err
		}
		if last != nil && (last.Status == ChargePending || last.Status == ChargeAwaitingPayment) {
			charge = *last
			return errNothingToDo
		}
		charge, opID, err = s.insertChargeForBalance(ctx, tx, tenantID, inv, last, actor, s.clock.Now())
		return err
	})
	if errors.Is(err, errNothingToDo) {
		if charge.ID == uuid.Nil {
			return nil, nil
		}
		return &charge, nil
	}
	if err != nil {
		return nil, err
	}
	s.executeBestEffort(ctx, tenantID, charge.ID, opID)
	return s.chargeAfter(context.WithoutCancel(ctx), tenantID, charge.ID)
}

// insertChargeForBalance grava, na transação corrente, a cobrança pelo saldo
// atual da fatura e a operação persistida que a cria no provedor. A chamada
// ao provedor ocorre depois do commit (ExecuteOperation). Sem provedor padrão
// configurado, devolve errNothingToDo.
func (s *Service) insertChargeForBalance(ctx context.Context, tx Tx, tenantID uuid.UUID, inv billing.Invoice, last *Charge, actor audit.Actor, now time.Time) (Charge, uuid.UUID, error) {
	account, err := tx.DefaultAccount(ctx)
	if apperr.IsKind(err, apperr.KindNotFound) {
		return Charge{}, uuid.Nil, errNothingToDo
	}
	if err != nil {
		return Charge{}, uuid.Nil, err
	}
	loc, err := tx.Location(ctx)
	if err != nil {
		return Charge{}, uuid.Nil, err
	}
	due := billing.DueDate(*inv.DueAt, loc)
	if today := calendar.DateOf(now, loc); due.Before(today) {
		due = today // provedores não aceitam vencimento no passado
	}
	attempt := 1
	if last != nil {
		attempt = last.Attempt + 1
	}
	charge, err := NewCharge(ids.New(), tenantID, inv.ID, inv.CustomerID, account.ID, account.Provider, attempt, inv.AmountDue, due, now)
	if err != nil {
		return Charge{}, uuid.Nil, err
	}
	op := Operation{ID: ids.New(), TenantID: tenantID, ChargeID: charge.ID, Kind: OpCreateCharge, Status: OpPending,
		ExternalReference: charge.ID.String(), NextAttemptAt: now, CreatedAt: now, UpdatedAt: now}
	if err := tx.InsertCharge(ctx, charge); err != nil {
		return Charge{}, uuid.Nil, err
	}
	if err := tx.InsertOperation(ctx, op); err != nil {
		return Charge{}, uuid.Nil, err
	}
	err = tx.Audit(ctx, audit.New(tenantID, now, actor, "charge.requested", "charge", charge.ID.String(),
		map[string]any{"invoice_id": inv.ID, "attempt": attempt, "amount": charge.Amount.Amount(), "provider": account.Provider}))
	return charge, op.ID, err
}

// executeBestEffort executa a criação no provedor logo após o commit. Em caso
// de falha a operação permanece registrada e é retomada pela recuperação
// agendada.
func (s *Service) executeBestEffort(ctx context.Context, tenantID, chargeID, opID uuid.UUID) {
	if err := s.ExecuteOperation(ctx, tenantID, opID); err != nil && s.cfg.Log != nil {
		s.cfg.Log.WarnContext(ctx, "criação da cobrança adiada para a recuperação", slog.String("charge_id", chargeID.String()),
			slog.String("error", err.Error()))
	}
}

func (s *Service) chargeAfter(ctx context.Context, tenantID, id uuid.UUID) (*Charge, error) {
	var c Charge
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		c, err = tx.Charge(ctx, id, false)
		return err
	})
	return &c, err
}

type outcome int

const (
	outcomeSucceeded outcome = iota + 1
	outcomePermanent
	outcomeRetryable
	outcomeAmbiguous
)

// ExecuteOperation executa uma operação externa em três fases: reivindica
// com lease (transação curta), chama o provedor sem transação aberta e
// registra o resultado (transação curta). Em tentativas após falha
// ambígua, consulta o provedor pela referência externa antes de criar.
func (s *Service) ExecuteOperation(ctx context.Context, tenantID, opID uuid.UUID) error {
	var (
		op      Operation
		charge  Charge
		account ProviderAccount
		cust    CustomerData
		number  string
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if op, err = tx.Operation(ctx, opID, true); err != nil {
			return err
		}
		now := s.clock.Now()
		if !op.Claimable(now) {
			return errNothingToDo
		}
		if op.Kind != OpCreateCharge {
			// Não reivindica operações que esta execução não sabe realizar.
			return fmt.Errorf("payments: operação %s ainda não suportada pela execução genérica", op.Kind)
		}
		lease := now.Add(s.cfg.Lease)
		op.Status, op.LeaseUntil, op.UpdatedAt = OpInFlight, &lease, now
		op.Attempts++
		if err := tx.UpdateOperation(ctx, op); err != nil {
			return err
		}
		if charge, err = tx.Charge(ctx, op.ChargeID, false); err != nil {
			return err
		}
		if account, err = tx.Account(ctx, charge.ProviderAccountID); err != nil {
			return err
		}
		if cust, err = tx.CustomerData(ctx, charge.CustomerID, charge.Provider); err != nil {
			return err
		}
		inv, err := tx.Invoice(ctx, charge.InvoiceID, false)
		number = inv.Number
		return err
	})
	if errors.Is(err, errNothingToDo) {
		return nil
	}
	if err != nil {
		return err
	}
	gw, err := s.resolver.Gateway(account)
	if err != nil {
		_, err := s.recordOutcome(ctx, tenantID, opID, outcomePermanent, nil, "", err)
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	pc, providerCustomer, res, callErr := s.createAtProvider(callCtx, gw, op, charge, cust, number)
	cancel()
	next, err := s.recordOutcome(ctx, tenantID, opID, res, pc, providerCustomer, callErr)
	if err != nil {
		return err
	}
	if next != nil {
		// A cobrança criada divergia do saldo atual da fatura e foi
		// substituída na mesma transação; a nova segue o fluxo normal.
		s.executeBestEffort(ctx, tenantID, next.chargeID, next.opID)
	}
	return nil
}

func classify(err error) outcome {
	switch GatewayErrorKind(err) {
	case ErrKindPermanent:
		return outcomePermanent
	case ErrKindRetryable:
		return outcomeRetryable
	}
	return outcomeAmbiguous
}

func (s *Service) createAtProvider(ctx context.Context, gw Gateway, op Operation, c Charge, cust CustomerData, number string) (*ProviderCharge, string, outcome, error) {
	if op.Attempts > 1 {
		found, err := gw.FindByExternalReference(ctx, op.ExternalReference)
		if err != nil {
			if GatewayErrorKind(err) == ErrKindPermanent {
				return nil, "", outcomePermanent, err
			}
			return nil, "", outcomeAmbiguous, err
		}
		if found != nil {
			return found, found.ProviderCustomerID, outcomeSucceeded, nil
		}
	}
	providerCustomer := cust.ProviderCustomerID
	if providerCustomer == "" {
		id, err := gw.EnsureCustomer(ctx, cust)
		if err != nil {
			return nil, "", classify(err), err
		}
		providerCustomer = id
		cust.ProviderCustomerID = id
	}
	pc, err := gw.CreateCharge(ctx, CreateChargeRequest{
		ExternalReference: op.ExternalReference, Amount: c.Amount, DueDate: c.DueDate,
		Description: "Fatura " + number, Customer: cust,
	})
	if err != nil {
		return nil, providerCustomer, classify(err), err
	}
	return &pc, providerCustomer, outcomeSucceeded, nil
}

// replacement identifica a cobrança gravada para substituir outra cujo valor
// deixou de corresponder ao saldo da fatura.
type replacement struct {
	chargeID, opID uuid.UUID
}

// recordOutcome registra o resultado da chamada ao provedor. Quando a
// cobrança recém-criada já não corresponde ao saldo da fatura, devolve a
// cobrança substituta gravada na mesma transação, para execução após o
// commit.
func (s *Service) recordOutcome(ctx context.Context, tenantID, opID uuid.UUID, res outcome, pc *ProviderCharge, providerCustomer string, callErr error) (*replacement, error) {
	var next *replacement
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		next = nil
		op, err := tx.Operation(ctx, opID, true)
		if err != nil {
			return err
		}
		if op.Status == OpSucceeded || op.Status == OpFailed {
			return nil // outra instância concluiu
		}
		charge, err := tx.Charge(ctx, op.ChargeID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		op.UpdatedAt, op.LeaseUntil = now, nil
		if callErr != nil {
			op.LastError = truncate(callErr.Error(), 1000)
		}
		switch res {
		case outcomeSucceeded:
			if providerCustomer != "" {
				if err := tx.SaveProviderCustomer(ctx, charge.CustomerID, charge.Provider, providerCustomer); err != nil {
					return err
				}
			}
			if err := charge.MarkCreated(pc.ProviderChargeID, pc.CheckoutURL, now); err != nil {
				return err
			}
			charge.RecordProviderStatus(pc.RawStatus, pc.UpdatedAt)
			op.Status, op.CompletedAt, op.LastError = OpSucceeded, &now, ""
			op.ProviderResponse = map[string]any{"provider_charge_id": pc.ProviderChargeID, "status": pc.RawStatus}
			replaced := false
			if charge.Status == ChargeAwaitingPayment && !pc.Status.IsPaid() {
				if replaced, next, err = s.replaceIfBalanceChanged(ctx, tx, tenantID, &charge, now); err != nil {
					return err
				}
			}
			if err := tx.UpdateCharge(ctx, &charge); err != nil {
				return err
			}
			if !replaced {
				if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventChargeCreated, "charge", charge.ID,
					map[string]any{"charge_id": charge.ID, "invoice_id": charge.InvoiceID, "checkout_url": charge.CheckoutURL, "amount": charge.Amount.Amount()}, now)); err != nil {
					return err
				}
			}
			if pc.Status.IsPaid() {
				if err := s.applyProviderState(ctx, tx, &charge, *pc, now, providerActor); err != nil {
					return err
				}
			}
		case outcomePermanent:
			if err := charge.MarkFailed(op.LastError, now); err != nil {
				return err
			}
			op.Status, op.CompletedAt = OpFailed, &now
			if err := tx.UpdateCharge(ctx, &charge); err != nil {
				return err
			}
			if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventChargeFailed, "charge", charge.ID,
				map[string]any{"charge_id": charge.ID, "invoice_id": charge.InvoiceID}, now)); err != nil {
				return err
			}
		case outcomeRetryable:
			op.Status, op.NextAttemptAt = OpPending, now.Add(Backoff(op.Attempts))
		default:
			// Resultado desconhecido: nunca repetir às cegas. A próxima
			// tentativa consulta o provedor pela referência externa.
			op.Status, op.NextAttemptAt = OpUnknown, now.Add(Backoff(op.Attempts))
		}
		if err := tx.UpdateOperation(ctx, op); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, systemActor, "payment_operation."+string(op.Status), "payment_operation", op.ID.String(),
			map[string]any{"charge_id": op.ChargeID, "attempts": op.Attempts, "kind": op.Kind}))
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

// chargeMatchesBalance informa se a cobrança corresponde ao saldo atual da
// fatura.
func chargeMatchesBalance(c Charge, inv billing.Invoice) bool {
	return inv.AmountDue.IsPositive() && c.Amount == inv.AmountDue
}

// replaceIfBalanceChanged ajusta a cobrança cuja criação no provedor terminou
// depois de uma alteração do saldo da fatura (por exemplo, nota de crédito
// emitida enquanto a criação estava em andamento ou em retentativa). Se o
// valor diverge do saldo, a cobrança é cancelada localmente e, havendo saldo,
// uma nova cobrança é gravada pelo valor atualizado na mesma transação; a
// chamada ao provedor ocorre depois do commit e, se falhar, fica com a
// recuperação agendada. O ajuste não depende de reentrega de mensagens.
// Faturas canceladas ou baixadas seguem os fluxos próprios.
func (s *Service) replaceIfBalanceChanged(ctx context.Context, tx Tx, tenantID uuid.UUID, c *Charge, now time.Time) (bool, *replacement, error) {
	inv, err := tx.Invoice(ctx, c.InvoiceID, true)
	if err != nil {
		return false, nil, err
	}
	if inv.Status != billing.Open && inv.Status != billing.PartiallyPaid && inv.Status != billing.Paid {
		return false, nil, nil
	}
	if chargeMatchesBalance(*c, inv) {
		return false, nil, nil
	}
	if err := c.MarkCanceled(now); err != nil {
		return false, nil, err
	}
	details := map[string]any{"invoice_id": inv.ID, "charge_amount": c.Amount.Amount(), "amount_due": inv.AmountDue.Amount()}
	var next *replacement
	if inv.Status != billing.Paid && inv.AmountDue.IsPositive() {
		nc, opID, err := s.insertChargeForBalance(ctx, tx, tenantID, inv, c, systemActor, now)
		switch {
		case err == nil:
			next = &replacement{chargeID: nc.ID, opID: opID}
			details["replacement_charge_id"] = nc.ID
		case !errors.Is(err, errNothingToDo):
			return false, nil, err
		}
	}
	err = tx.Audit(ctx, audit.New(tenantID, now, systemActor, "charge.canceled_after_balance_change", "charge", c.ID.String(), details))
	if err != nil {
		return false, nil, err
	}
	return true, next, nil
}

// RecoverOperations retoma operações pendentes, com lease expirado ou com
// resultado desconhecido elegível a nova consulta.
func (s *Service) RecoverOperations(ctx context.Context) (int, error) {
	now := s.clock.Now()
	tenants, err := s.store.TenantsWithPendingOperations(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		var opIDs []uuid.UUID
		err := s.store.InTenant(ctx, t, func(tx Tx) error {
			var err error
			opIDs, err = tx.DueOperationIDs(ctx, now, 50)
			return err
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, id := range opIDs {
			if err := ctx.Err(); err != nil {
				return n, err
			}
			if err := s.ExecuteOperation(ctx, t, id); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// HashEndpointToken calcula o hash do token opaco do endpoint de webhook.
func HashEndpointToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// MaxWebhookBody limita o tamanho aceito de um webhook.
const MaxWebhookBody = 256 << 10

// ReceiveWebhook autentica o webhook conforme o contrato do provedor,
// vincula ao tenant pelo token opaco do endpoint e persiste o evento antes
// de confirmar o recebimento. O processamento ocorre de forma assíncrona.
// Retorna duplicate=true quando o evento já havia sido recebido.
func (s *Service) ReceiveWebhook(ctx context.Context, provider, endpointToken string, header http.Header, body []byte) (duplicate bool, err error) {
	notFound := apperr.NotFound("webhook_endpoint_not_found", "Endpoint de webhook desconhecido.")
	if !s.resolver.Supports(provider) || len(endpointToken) < 20 || len(endpointToken) > 200 {
		return false, notFound
	}
	if len(body) > MaxWebhookBody {
		return false, apperr.Invalid("webhook_too_large", "Corpo do webhook excede o limite.")
	}
	tenantID, accountID, err := s.store.ResolveWebhookEndpoint(ctx, provider, HashEndpointToken(endpointToken))
	if err != nil {
		if apperr.IsKind(err, apperr.KindNotFound) {
			return false, notFound
		}
		return false, err
	}
	verifier, err := s.resolver.Verifier(provider)
	if err != nil {
		return false, err
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		account, err := tx.Account(ctx, accountID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		if err := verifier.Verify(header, body, account.Secret, now); err != nil {
			return apperr.Unauthenticated("webhook_unauthenticated", "Assinatura ou token do webhook inválido.").Wrap(err)
		}
		ev, err := verifier.Parse(body)
		if err != nil {
			return apperr.Invalid("webhook_invalid", "Conteúdo do webhook inválido.").Wrap(err)
		}
		w := StoredWebhook{ID: ids.New(), TenantID: tenantID, ProviderAccountID: accountID, Provider: provider, EventID: ev.EventID,
			EventType: ev.Type, ProviderChargeID: ev.ProviderChargeID, OccurredAt: ev.OccurredAt, ReceivedAt: now, Payload: body, Status: "received"}
		inserted, err := tx.InsertWebhook(ctx, w)
		if err != nil {
			return err
		}
		if !inserted {
			duplicate = true
			return nil
		}
		return tx.Emit(ctx, outbox.New(tenantID, outbox.EventProviderWebhook, "provider_webhook", w.ID,
			map[string]any{"webhook_event_id": w.ID, "provider": provider, "event_type": ev.Type}, now))
	})
	return duplicate, err
}

// ConsumerWebhooks identifica o consumidor de webhooks no inbox.
const ConsumerWebhooks = "payments.provider_webhooks"

// ProcessWebhook aplica um webhook persistido. A ordem de chegada não
// determina o estado financeiro: eventos antigos não regridem a cobrança e,
// quando o evento não é aplicável ao estado local (fora de ordem) ou não traz
// dados suficientes, a situação autoritativa é consultada no provedor
// (fora de transação) antes de aplicar.
func (s *Service) ProcessWebhook(ctx context.Context, tenantID, webhookID, messageID uuid.UUID) error {
	var (
		w       StoredWebhook
		charge  Charge
		account ProviderAccount
		ev      WebhookEvent
		found   bool
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if w, err = tx.Webhook(ctx, webhookID, false); err != nil {
			return err
		}
		if w.Status != "received" {
			return errNothingToDo
		}
		verifier, err := s.resolver.Verifier(w.Provider)
		if err != nil {
			return err
		}
		if ev, err = verifier.Parse(w.Payload); err != nil {
			return err
		}
		if ev.ProviderChargeID == "" {
			return nil
		}
		charge, err = tx.ChargeByProviderID(ctx, w.Provider, ev.ProviderChargeID, false)
		if apperr.IsKind(err, apperr.KindNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		account, err = tx.Account(ctx, charge.ProviderAccountID)
		return err
	})
	if errors.Is(err, errNothingToDo) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot *ProviderCharge
	if found && needsAuthoritativeState(charge, ev) {
		gw, err := s.resolver.Gateway(account)
		if err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		pc, err := gw.GetCharge(callCtx, ev.ProviderChargeID)
		cancel()
		if err != nil {
			return fmt.Errorf("payments: consulta ao provedor: %w", err)
		}
		snapshot = &pc
	} else if found {
		snapshot = ev.Charge
	}
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerWebhooks, messageID)
		if err != nil || !first {
			return err
		}
		now := s.clock.Now()
		if !found {
			// Cobrança desconhecida: registrado para a conciliação.
			return tx.UpdateWebhookStatus(ctx, webhookID, "ignored", "cobrança não encontrada localmente", now)
		}
		c, err := tx.ChargeByProviderID(ctx, w.Provider, ev.ProviderChargeID, true)
		if err != nil {
			return err
		}
		if snapshot == nil {
			return tx.UpdateWebhookStatus(ctx, webhookID, "ignored", "evento sem efeito financeiro", now)
		}
		if err := s.applyProviderState(ctx, tx, &c, *snapshot, now, providerActor); err != nil {
			if apperr.IsKind(err, apperr.KindConflict) {
				return tx.UpdateWebhookStatus(ctx, webhookID, "ignored", err.Error(), now)
			}
			return err
		}
		return tx.UpdateWebhookStatus(ctx, webhookID, "processed", "", now)
	})
}

// statusRank ordena situações do provedor para impedir regressão por evento antigo.
func statusRank(s ProviderStatus) int {
	switch s {
	case ProviderPending, ProviderOverdue:
		return 0
	case ProviderConfirmed, ProviderReceived:
		return 2
	case ProviderRefundInProgress:
		return 3
	case ProviderPartiallyRefunded:
		return 4
	case ProviderRefunded, ProviderChargebackRequested, ProviderChargebackDispute, ProviderChargebackReversed, ProviderChargebackLost:
		return 5
	case ProviderFailed, ProviderCanceled:
		return 1
	}
	return -1
}

func localRank(c Charge) int {
	switch c.Status {
	case ChargePending, ChargeAwaitingPayment:
		return 0
	case ChargeFailed, ChargeCanceled:
		return 1
	case ChargePaid:
		return 2
	case ChargePartiallyRefunded:
		return 4
	default:
		return 5
	}
}

// needsAuthoritativeState decide quando consultar o provedor: evento sem
// snapshot de valores, evento que não é a próxima etapa natural do estado
// local (fora de ordem) ou status desconhecido.
func needsAuthoritativeState(c Charge, ev WebhookEvent) bool {
	if ev.Charge == nil || ev.Status == ProviderUnknownStatus {
		return true
	}
	lr, er := localRank(c), statusRank(ev.Status)
	if er < lr {
		return false // evento antigo: não regride, sem necessidade de consulta
	}
	// Saltos (ex.: reembolso chegando antes da confirmação) pedem a
	// situação autoritativa.
	return er-lr > 2 || (lr == 0 && er > 2)
}

// applyProviderState aplica a situação informada pelo provedor sem
// regredir estados e de forma idempotente.
func (s *Service) applyProviderState(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor) error {
	c.RecordProviderStatus(pc.RawStatus, pc.UpdatedAt)
	if statusRank(pc.Status) < localRank(*c) && pc.Status != ProviderFailed && pc.Status != ProviderCanceled {
		return tx.UpdateCharge(ctx, c) // evento antigo: apenas registra a situação recebida
	}
	switch {
	case pc.Status.IsPaid() && !IsSettledStatus(c.Status):
		if err := s.confirmPayment(ctx, tx, c, pc, now, actor); err != nil {
			return err
		}
	case (pc.Status == ProviderFailed || pc.Status == ProviderCanceled) && !IsSettledStatus(c.Status):
		if c.Status == ChargeFailed || c.Status == ChargeCanceled {
			return nil
		}
		if pc.Status == ProviderFailed {
			if err := c.MarkFailed("recusada pelo provedor", now); err != nil {
				return err
			}
		} else if err := c.MarkCanceled(now); err != nil {
			return err
		}
		if err := tx.Emit(ctx, outbox.New(c.TenantID, outbox.EventChargeFailed, "charge", c.ID,
			map[string]any{"charge_id": c.ID, "invoice_id": c.InvoiceID, "provider_status": pc.RawStatus}, now)); err != nil {
			return err
		}
	case (pc.Status == ProviderFailed || pc.Status == ProviderCanceled) && IsSettledStatus(c.Status):
		return apperr.Conflict("provider_state_conflict", "Provedor informou cancelamento de cobrança já paga; verificar na conciliação.")
	}
	return tx.UpdateCharge(ctx, c)
}

func (s *Service) confirmPayment(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor) error {
	paidAt := now
	if pc.PaidAt != nil {
		paidAt = *pc.PaidAt
	}
	gross := pc.Amount
	if gross.Currency() != c.Amount.Currency() || !gross.IsPositive() {
		gross = c.Amount
	}
	fee := pc.FeeOrDerived()
	// Cobrança cancelada apenas no Recorvia continua válida no provedor: a
	// confirmação prevalece e o valor segue para a fatura (excedente vira
	// crédito do cliente), nunca é descartado.
	wasCanceled := c.Status == ChargeCanceled
	if err := c.MarkPaid(gross, fee, paidAt, now); err != nil {
		return err
	}
	inv, err := tx.Invoice(ctx, c.InvoiceID, true)
	if err != nil {
		return err
	}
	chargeID := c.ID
	pay := Payment{ID: ids.New(), TenantID: c.TenantID, InvoiceID: inv.ID, ChargeID: &chargeID, CustomerID: c.CustomerID,
		Amount: gross, Fee: fee, Method: "provider", Source: "provider", PaidAt: paidAt, CreatedAt: now}
	inserted, err := tx.InsertPayment(ctx, pay)
	if err != nil || !inserted {
		return err
	}
	if wasCanceled {
		if err := tx.Audit(ctx, audit.New(c.TenantID, now, actor, "charge.paid_after_local_cancellation", "charge", c.ID.String(),
			map[string]any{"invoice_id": inv.ID, "payment_id": pay.ID, "amount": gross.Amount(), "invoice_status": string(inv.Status)})); err != nil {
			return err
		}
	}
	return s.applyToInvoice(ctx, tx, &inv, pay, true, actor, now)
}

func (s *Service) applyToInvoice(ctx context.Context, tx Tx, inv *billing.Invoice, pay Payment, viaProvider bool, actor audit.Actor, now time.Time) error {
	if inv.Status == billing.Void || inv.Status == billing.Paid {
		// Fatura já quitada ou cancelada: o valor bruto vira crédito do
		// cliente e a taxa do provedor é reconhecida como despesa.
		if err := s.creditOverpayment(ctx, tx, inv, pay, pay.Amount, viaProvider, actor, now); err != nil {
			return err
		}
		if !viaProvider || !pay.Fee.IsPositive() {
			return nil
		}
		t, ok, err := ledger.ProviderFeeCharged(ids.New(), pay.TenantID, pay.ID, pay.Fee, now)
		if err != nil || !ok {
			return err
		}
		return tx.PostLedger(ctx, t)
	}
	applied, excess, recovered, err := inv.ApplyPaymentWithRecovery(pay.Amount, now)
	if err != nil {
		return err
	}
	if err := tx.UpdateInvoiceProgress(ctx, inv); err != nil {
		return err
	}
	if err := tx.InsertInvoiceHistory(ctx, inv.ID, actor, inv.PopChanges()); err != nil {
		return err
	}
	var t ledger.Transaction
	var ok bool
	if viaProvider {
		t, ok, err = ledger.ProviderPaymentReceived(ids.New(), pay.TenantID, pay.ID, applied, pay.Fee, now)
	} else {
		t, ok, err = ledger.ManualPaymentReceived(ids.New(), pay.TenantID, pay.ID, applied, now)
	}
	if err != nil {
		return err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
	}
	if recovered.IsPositive() {
		t, ok, err := ledger.BadDebtRecovered(ids.New(), pay.TenantID, pay.ID, recovered, now)
		if err != nil {
			return err
		}
		if ok {
			if err := tx.PostLedger(ctx, t); err != nil {
				return err
			}
		}
	}
	if excess.IsPositive() {
		if err := s.creditOverpayment(ctx, tx, inv, pay, excess, viaProvider, actor, now); err != nil {
			return err
		}
	}
	event := outbox.EventInvoicePartiallyPaid
	if inv.Status == billing.Paid {
		event = outbox.EventInvoicePaid
		if inv.SubscriptionID != nil {
			if err := tx.RefreshSubscriptionStanding(ctx, *inv.SubscriptionID, actor, now); err != nil {
				return err
			}
		}
	}
	if err := tx.Emit(ctx, outbox.New(pay.TenantID, outbox.EventPaymentConfirmed, "payment", pay.ID,
		map[string]any{"payment_id": pay.ID, "invoice_id": inv.ID, "charge_id": pay.ChargeID, "amount": pay.Amount.Amount(), "fee": pay.Fee.Amount(), "source": pay.Source}, now)); err != nil {
		return err
	}
	if err := tx.Emit(ctx, outbox.New(pay.TenantID, event, "invoice", inv.ID,
		map[string]any{"invoice_id": inv.ID, "number": inv.Number, "amount_due": inv.AmountDue.Amount(), "amount_paid": inv.AmountPaid.Amount()}, now)); err != nil {
		return err
	}
	return tx.Audit(ctx, audit.New(pay.TenantID, now, actor, "payment.applied", "invoice", inv.ID.String(),
		map[string]any{"payment_id": pay.ID, "applied": applied.Amount(), "excess": excess.Amount(), "source": pay.Source}))
}

func (s *Service) creditOverpayment(ctx context.Context, tx Tx, inv *billing.Invoice, pay Payment, amount money.Money, viaProvider bool, actor audit.Actor, now time.Time) error {
	if !amount.IsPositive() {
		return nil
	}
	invID := inv.ID
	if err := tx.GrantCustomerCredit(ctx, pay.CustomerID, amount, "overpayment", &invID, "Pagamento excedente", now); err != nil {
		return err
	}
	t, ok, err := ledger.Overpayment(ids.New(), pay.TenantID, pay.ID, amount, viaProvider, now)
	if err != nil {
		return err
	}
	if ok {
		if err := tx.PostLedger(ctx, t); err != nil {
			return err
		}
	}
	return tx.Audit(ctx, audit.New(pay.TenantID, now, actor, "payment.overpayment_credited", "customer", pay.CustomerID.String(),
		map[string]any{"payment_id": pay.ID, "amount": amount.Amount()}))
}

// ManualPaymentInput registra um recebimento fora do provedor.
type ManualPaymentInput struct {
	InvoiceID uuid.UUID
	Amount    money.Money
	PaidAt    time.Time
	Method    string
	Note      string
	// RequestKey identifica o pedido (derivado do Idempotency-Key); um
	// segundo registro com a mesma chave devolve o pagamento existente.
	RequestKey string
}

var manualMethods = map[string]bool{"pix": true, "bank_transfer": true, "boleto": true, "cash": true, "other": true}

// RecordManualPayment registra um recebimento manual (por exemplo,
// transferência identificada no extrato).
func (s *Service) RecordManualPayment(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in ManualPaymentInput) (Payment, error) {
	var f []apperr.FieldError
	if !in.Amount.IsPositive() || in.Amount.CheckBusinessRange() != nil {
		f = append(f, apperr.FieldError{Field: "amount", Message: "Informe um valor positivo em centavos."})
	}
	if !manualMethods[in.Method] {
		f = append(f, apperr.FieldError{Field: "method", Message: "Use pix, bank_transfer, boleto, cash ou other."})
	}
	now := s.clock.Now()
	if in.PaidAt.IsZero() {
		in.PaidAt = now
	}
	if in.PaidAt.After(now) {
		f = append(f, apperr.FieldError{Field: "paid_at", Message: "A data do pagamento não pode estar no futuro."})
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 500 {
		f = append(f, apperr.FieldError{Field: "note", Message: "Observação com no máximo 500 caracteres."})
	}
	if len(f) > 0 {
		return Payment{}, apperr.Invalid("invalid_payment", "Pagamento inválido.", f...)
	}
	var pay Payment
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		inv, err := tx.Invoice(ctx, in.InvoiceID, true)
		if err != nil {
			return err
		}
		if in.RequestKey != "" {
			// Retentativa de um pedido já registrado devolve o pagamento
			// existente mesmo que ele tenha quitado a fatura.
			existing, err := tx.PaymentByRequestKey(ctx, in.RequestKey)
			if err == nil {
				if existing.InvoiceID != inv.ID {
					return apperr.Conflict("request_key_reused", "A chave do pedido já foi usada em outra fatura.")
				}
				pay = existing
				return nil
			}
			if !apperr.IsKind(err, apperr.KindNotFound) {
				return err
			}
		}
		if inv.Status != billing.Open && inv.Status != billing.PartiallyPaid && inv.Status != billing.Uncollectible {
			return apperr.Conflict("invoice_not_payable", "A fatura não aceita pagamentos nesta situação.")
		}
		pay = Payment{ID: ids.New(), TenantID: tenantID, InvoiceID: inv.ID, CustomerID: inv.CustomerID, Amount: in.Amount,
			Fee: money.Zero(in.Amount.Currency()), Method: in.Method, Source: "manual", PaidAt: in.PaidAt, Note: in.Note, RecordedBy: userID,
			RequestKey: in.RequestKey, CreatedAt: now}
		inserted, err := tx.InsertPayment(ctx, pay)
		if err != nil {
			return err
		}
		if !inserted {
			// Mesmo pedido já registrado (retentativa após falha): devolve o existente.
			existing, err := tx.PaymentByRequestKey(ctx, in.RequestKey)
			if err != nil {
				return err
			}
			pay = existing
			return nil
		}
		return s.applyToInvoice(ctx, tx, &inv, pay, false, actor, now)
	})
	return pay, err
}

// CancelOpenCharges cancela localmente cobranças em aberto de uma fatura
// cancelada (consumidor de invoice.voided).
func (s *Service) CancelOpenCharges(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, "payments.invoice_voided", messageID)
		if err != nil || !first {
			return err
		}
		charges, err := tx.OpenChargesForInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		for i := range charges {
			if err := charges[i].MarkCanceled(now); err != nil {
				return err
			}
			if err := tx.UpdateCharge(ctx, &charges[i]); err != nil {
				return err
			}
		}
		return nil
	})
}

// ConsumerInvoiceCredited identifica o consumidor que ajusta cobranças após
// nota de crédito.
const ConsumerInvoiceCredited = "payments.invoice_credited"

// HandleInvoiceCredited ajusta a cobrança depois de uma nota de crédito:
// cobranças aguardando pagamento com valor diferente do novo saldo são
// canceladas localmente e, havendo saldo, uma nova cobrança é solicitada
// pelo valor atualizado. Cobranças ainda em criação no provedor (pending)
// não são alteradas aqui: o ajuste ocorre quando a operação de criação é
// concluída (recordOutcome), de modo que a mensagem é registrada sem depender
// de reentregas dentro da janela de retentativas do consumidor. É repetível:
// na reentrega não há cobrança divergente a cancelar e a solicitação devolve
// a cobrança já criada. Um pagamento que ainda chegue pela cobrança antiga é
// aplicado normalmente e o excedente vira crédito do cliente.
func (s *Service) HandleInvoiceCredited(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		// Bloqueia as cobranças antes da fatura, na mesma ordem do registro
		// do resultado da criação e da confirmação de pagamento, para que as
		// transações concorrentes se serializem sem impasse.
		charges, err := tx.OpenChargesForInvoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		inv, err := tx.Invoice(ctx, invoiceID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		for i := range charges {
			c := &charges[i]
			if c.Status == ChargePending || chargeMatchesBalance(*c, inv) {
				continue
			}
			if err := c.MarkCanceled(now); err != nil {
				return err
			}
			if err := tx.UpdateCharge(ctx, c); err != nil {
				return err
			}
			if err := tx.Audit(ctx, audit.New(tenantID, now, systemActor, "charge.canceled_after_credit_note", "charge", c.ID.String(),
				map[string]any{"invoice_id": invoiceID, "charge_amount": c.Amount.Amount(), "amount_due": inv.AmountDue.Amount()})); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err := s.RequestCharge(ctx, tenantID, invoiceID, systemActor); err != nil {
		return err
	}
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		_, err := tx.MarkInbox(ctx, ConsumerInvoiceCredited, messageID)
		return err
	})
}

// ConsumerInvoiceIssued identifica o consumidor que cria cobranças.
const ConsumerInvoiceIssued = "payments.invoice_issued"

// HandleInvoiceIssued cria a cobrança de uma fatura emitida e depois
// registra o inbox numa transação própria. A cobrança é idempotente por
// fatura (RequestCharge retorna a existente), então uma reentrega após falha
// entre as duas etapas não duplica a cobrança.
func (s *Service) HandleInvoiceIssued(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	_, err := s.RequestCharge(ctx, tenantID, invoiceID, systemActor)
	if err != nil {
		return err
	}
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		_, err := tx.MarkInbox(ctx, ConsumerInvoiceIssued, messageID)
		return err
	})
}
