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
	// ExhaustedOperations conta, por situação, as operações do tenant paradas
	// depois de esgotar as tentativas automáticas (Operation.Exhausted).
	ExhaustedOperations(ctx context.Context, now time.Time) (map[OperationStatus]int, error)

	InsertPayment(ctx context.Context, p Payment) (bool, error)
	PaymentByRequestKey(ctx context.Context, key string) (Payment, error)
	GrantCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, kind string, invoiceID *uuid.UUID, reason string, at time.Time) error

	InsertWebhook(ctx context.Context, w StoredWebhook) (bool, error)
	Webhook(ctx context.Context, id uuid.UUID, forUpdate bool) (StoredWebhook, error)
	UpdateWebhookStatus(ctx context.Context, id uuid.UUID, status, errText string, at time.Time) error

	PostLedger(ctx context.Context, t ledger.Transaction) error
	Audit(ctx context.Context, e audit.Entry) error
	Emit(ctx context.Context, e outbox.Event) error

	RefundTx
	DisputeTx
	ReconciliationTx
}

// Store abre transações e resolve o vínculo de webhooks com tenants.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	ResolveWebhookEndpoint(ctx context.Context, provider string, tokenHash []byte) (tenantID, accountID uuid.UUID, err error)
	TenantsWithPendingOperations(ctx context.Context, now time.Time) ([]uuid.UUID, error)
	// TenantsDueReconciliation descobre tenants elegíveis à conciliação
	// agendada sem execução no intervalo informado.
	TenantsDueReconciliation(ctx context.Context, now time.Time, every time.Duration) ([]uuid.UUID, error)
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
	// ManualReconciliationBudget limita a coleta da conciliação manual
	// (padrão ManualReconciliationBudget).
	ManualReconciliationBudget time.Duration
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
	if cfg.ManualReconciliationBudget <= 0 {
		cfg.ManualReconciliationBudget = ManualReconciliationBudget
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
// existente. Sem provedor configurado, não cria cobrança. Devolve a cobrança
// vigente: se o saldo da fatura mudou enquanto a criação estava em andamento
// e a cobrança foi substituída na conclusão, devolve a substituta.
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
	current := s.executeBestEffort(ctx, tenantID, charge.ID, opID)
	return s.chargeAfter(context.WithoutCancel(ctx), tenantID, current)
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
// agendada. Devolve o ID da cobrança vigente: a informada ou, se ela foi
// substituída na conclusão da criação, a última substituta.
func (s *Service) executeBestEffort(ctx context.Context, tenantID, chargeID, opID uuid.UUID) uuid.UUID {
	current, err := s.executeOperation(ctx, tenantID, opID, execImmediate)
	if err != nil && s.cfg.Log != nil {
		s.cfg.Log.WarnContext(ctx, "criação da cobrança adiada para a recuperação", slog.String("charge_id", chargeID.String()),
			slog.String("error", err.Error()))
	}
	if current == uuid.Nil {
		return chargeID
	}
	return current
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
	// outcomeObsolete: a cobrança deixou de estar pendente e o provedor não
	// tem cobrança com a referência externa; a criação não se aplica mais.
	outcomeObsolete
)

// obsoleteOperationError é o motivo registrado ao encerrar uma criação que
// deixou de se aplicar.
const obsoleteOperationError = "A cobrança não está mais pendente (por exemplo, cancelada com a fatura) e não existe no provedor; a criação foi encerrada."

// execMode identifica a origem da execução de uma operação.
type execMode int

const (
	// execImmediate é a execução logo após gravar a operação, no mesmo
	// fluxo: nenhuma chamada ao provedor pode ter ocorrido antes. Só consulta
	// o provedor pela referência externa a partir da segunda tentativa.
	execImmediate execMode = iota
	// execRecovery retoma uma operação gravada antes (recuperação agendada).
	// A operação pode ter sido executada fora do registro local, por
	// exemplo num banco restaurado de backup em que a chamada ao provedor
	// ocorreu depois do instante da cópia: consulta pela referência externa
	// antes de criar, mesmo na primeira tentativa registrada.
	execRecovery
	// execManual é a reexecução solicitada pela operação: além de consultar
	// antes de criar, antecipa a próxima tentativa (ignora next_attempt_at) e
	// recusa, com conflito, operações concluídas, esgotadas ou em execução.
	execManual
)

// ExecuteOperation executa uma operação externa em três fases: reivindica
// com lease (transação curta), chama o provedor sem transação aberta e
// registra o resultado (transação curta). É a execução da recuperação
// agendada: respeita o horário da próxima tentativa e consulta o provedor
// pela referência externa antes de criar, para nunca repetir às cegas uma
// chamada que possa ter ocorrido. Sem nada a fazer, retorna nil.
func (s *Service) ExecuteOperation(ctx context.Context, tenantID, opID uuid.UUID) error {
	_, err := s.executeOperation(ctx, tenantID, opID, execRecovery)
	return err
}

// RetryOperation reexecuta a operação por solicitação da operação (API):
// antecipa a tentativa sem esperar next_attempt_at e consulta o provedor
// pela referência externa antes de criar. Operação concluída, em execução
// com lease vigente ou esgotada com resultado desconhecido resulta em erro de
// conflito, sem executar nada (Operation.CheckManualRetry). Como na
// recuperação agendada, uma criação obsoleta (cobrança não mais pendente) só
// consulta o provedor: vincula a cobrança encontrada ou encerra a operação.
func (s *Service) RetryOperation(ctx context.Context, tenantID, opID uuid.UUID) error {
	_, err := s.executeOperation(ctx, tenantID, opID, execManual)
	return err
}

// executeOperation implementa ExecuteOperation e RetryOperation. Quando a
// cobrança criada é substituída na conclusão (saldo da fatura alterado
// durante a criação), executa a substituta e devolve o ID da cobrança
// vigente; caso contrário, devolve uuid.Nil.
func (s *Service) executeOperation(ctx context.Context, tenantID, opID uuid.UUID, mode execMode) (uuid.UUID, error) {
	var w opWork
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		w, err = s.claimOperation(ctx, tx, opID, mode)
		return err
	})
	if err != nil || w.skip {
		return uuid.Nil, err
	}
	switch w.op.Kind {
	case OpRefund:
		return uuid.Nil, s.runRefund(ctx, tenantID, w)
	case OpCancelCharge:
		return uuid.Nil, s.runCancel(ctx, tenantID, w)
	}
	gw, err := s.resolver.Gateway(w.account)
	if err != nil {
		_, err := s.recordOutcome(ctx, tenantID, opID, outcomePermanent, nil, "", err)
		return uuid.Nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	pc, providerCustomer, res, callErr := s.createAtProvider(callCtx, gw, w.op, w.charge, w.cust, w.number, mode != execImmediate, !w.obsolete)
	cancel()
	next, err := s.recordOutcome(ctx, tenantID, opID, res, pc, providerCustomer, callErr)
	if err != nil {
		return uuid.Nil, err
	}
	s.executeAll(ctx, tenantID, next.cancelOps)
	if next.replacement == nil {
		return uuid.Nil, nil
	}
	// A cobrança criada divergia do saldo atual da fatura e foi substituída
	// na mesma transação; a nova segue o fluxo normal e passa a ser a vigente.
	return s.executeBestEffort(ctx, tenantID, next.replacement.chargeID, next.replacement.opID), nil
}

// opWork reúne o que a execução de uma operação reivindicada precisa.
type opWork struct {
	skip bool
	// obsolete indica a criação de cobrança que deixou de se aplicar
	// (Operation.Obsolete): a execução só consulta o provedor.
	obsolete bool
	op       Operation
	charge   Charge
	account  ProviderAccount
	cust     CustomerData
	number   string
	refund   Refund
	refunds  []Refund
	// dispatched indica os reembolsos da cobrança já enviados ao provedor.
	dispatched map[uuid.UUID]bool
}

// refundSerializationDelay adia o reembolso enquanto outro reembolso da
// mesma cobrança aguarda resultado no provedor.
const refundSerializationDelay = 30 * time.Second

// claimOperation reivindica a operação com lease e carrega os dados da
// chamada. Operações já resolvidas por outro caminho (por exemplo, reembolso
// confirmado por webhook) são concluídas aqui sem chamada ao provedor.
func (s *Service) claimOperation(ctx context.Context, tx Tx, opID uuid.UUID, mode execMode) (opWork, error) {
	op, err := tx.Operation(ctx, opID, true)
	if err != nil {
		return opWork{}, err
	}
	now := s.clock.Now()
	if mode == execManual {
		// A reexecução manual antecipa a tentativa e recusa, com conflito,
		// operações concluídas, esgotadas ou em execução.
		if err := op.CheckManualRetry(now); err != nil {
			return opWork{}, err
		}
	} else if !op.Claimable(now) {
		return opWork{skip: true}, nil
	}
	switch op.Kind {
	case OpCreateCharge, OpRefund, OpCancelCharge:
	default:
		// Não reivindica operações que esta execução não sabe realizar.
		return opWork{}, fmt.Errorf("payments: operação %s não suportada", op.Kind)
	}
	w := opWork{op: op}
	if w.charge, err = tx.Charge(ctx, op.ChargeID, false); err != nil {
		return opWork{}, err
	}
	if w.account, err = tx.Account(ctx, w.charge.ProviderAccountID); err != nil {
		return opWork{}, err
	}
	switch op.Kind {
	case OpCreateCharge:
		// A cobrança pode ter deixado de estar pendente enquanto a criação
		// aguardava nova tentativa (fatura cancelada, por exemplo): a
		// execução passa a só consultar o provedor.
		w.obsolete = op.Obsolete(w.charge.Status)
		if w.cust, err = tx.CustomerData(ctx, w.charge.CustomerID, w.charge.Provider); err != nil {
			return opWork{}, err
		}
		inv, err := tx.Invoice(ctx, w.charge.InvoiceID, false)
		if err != nil {
			return opWork{}, err
		}
		w.number = inv.Number
	case OpRefund:
		done, err := s.prepareRefundWork(ctx, tx, &w, now)
		if err != nil || done {
			return opWork{skip: true}, err
		}
	case OpCancelCharge:
		if w.charge.ProviderChargeID == "" {
			// Nada a cancelar no provedor.
			op.Status, op.CompletedAt, op.UpdatedAt, op.LastError = OpSucceeded, &now, now, ""
			return opWork{skip: true}, tx.UpdateOperation(ctx, op)
		}
	}
	lease := now.Add(s.cfg.Lease)
	op.Status, op.LeaseUntil, op.UpdatedAt = OpInFlight, &lease, now
	op.Attempts++
	if err := tx.UpdateOperation(ctx, op); err != nil {
		return opWork{}, err
	}
	w.op = op
	if w.dispatched != nil && w.refund.ID != uuid.Nil {
		w.dispatched[w.refund.ID] = true
	}
	return w, nil
}

// executeAll executa operações após o commit, em melhor esforço: falhas
// ficam com a recuperação agendada.
func (s *Service) executeAll(ctx context.Context, tenantID uuid.UUID, opIDs []uuid.UUID) {
	for _, id := range opIDs {
		if _, err := s.executeOperation(ctx, tenantID, id, execImmediate); err != nil && s.cfg.Log != nil {
			s.cfg.Log.WarnContext(ctx, "operação externa adiada para a recuperação", slog.String("operation_id", id.String()),
				slog.String("error", err.Error()))
		}
	}
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

// createAtProvider cria a cobrança no provedor. Com lookupFirst, ou a partir
// da segunda tentativa, consulta antes pela referência externa e vincula a
// cobrança encontrada em vez de criar outra. Sem create (operação obsoleta),
// só consulta: sem cobrança no provedor, devolve outcomeObsolete.
func (s *Service) createAtProvider(ctx context.Context, gw Gateway, op Operation, c Charge, cust CustomerData, number string,
	lookupFirst, create bool) (*ProviderCharge, string, outcome, error) {
	if lookupFirst || op.Attempts > 1 || !create {
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
	if !create {
		return nil, "", outcomeObsolete, nil
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

// followUp reúne o que executar depois do commit do resultado da criação: a
// cobrança substituta e os cancelamentos no provedor.
type followUp struct {
	replacement *replacement
	cancelOps   []uuid.UUID
}

// recordOutcome registra o resultado da chamada ao provedor. Quando a
// cobrança recém-criada já não corresponde ao saldo da fatura, devolve a
// cobrança substituta gravada na mesma transação, para execução após o
// commit, e o cancelamento da cobrança substituída no provedor.
func (s *Service) recordOutcome(ctx context.Context, tenantID, opID uuid.UUID, res outcome, pc *ProviderCharge, providerCustomer string, callErr error) (followUp, error) {
	var next followUp
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		next = followUp{}
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
		// A cobrança pode ter deixado de estar pendente antes ou durante a
		// chamada (fatura cancelada): o resultado é registrado sem anunciar a
		// cobrança como criada nem recusá-la.
		obsolete := op.Obsolete(charge.Status)
		if obsolete && res == outcomeRetryable {
			// O provedor recusou a criação de forma transitória: a cobrança
			// não existe nele e não será mais criada.
			res = outcomeObsolete
		}
		switch res {
		case outcomeSucceeded:
			// Numa cobrança cancelada, apenas vincula a cobrança do provedor
			// (sem transição): um pagamento que chegue por ela é aplicado e
			// vira crédito do cliente.
			if err := charge.MarkCreated(pc.ProviderChargeID, pc.CheckoutURL, now); err != nil {
				return err
			}
			charge.RecordProviderStatus(pc.RawStatus, pc.UpdatedAt)
			op.Status, op.CompletedAt, op.LastError = OpSucceeded, &now, ""
			op.ProviderResponse = map[string]any{"provider_charge_id": pc.ProviderChargeID, "status": pc.RawStatus}
			replaced := false
			if charge.Status == ChargeAwaitingPayment && !pc.Status.IsPaid() {
				if replaced, next.replacement, err = s.replaceIfBalanceChanged(ctx, tx, tenantID, &charge, now); err != nil {
					return err
				}
			}
			if err := tx.UpdateCharge(ctx, &charge); err != nil {
				return err
			}
			switch {
			case obsolete:
				// A cobrança continua aberta no provedor para uma cobrança
				// cancelada localmente: registrada para a conciliação, sem
				// charge.created (o link não deve chegar ao cliente).
				if err := tx.Audit(ctx, audit.New(tenantID, now, systemActor, "charge.linked_after_local_cancellation", "charge", charge.ID.String(),
					map[string]any{"invoice_id": charge.InvoiceID, "provider_charge_id": charge.ProviderChargeID, "status": string(charge.Status)})); err != nil {
					return err
				}
			case !replaced:
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
			// Cobrança cancelada localmente (substituída ou com a fatura
			// cancelada durante a criação) é cancelada também no provedor.
			if charge.Status == ChargeCanceled {
				id, ok, err := s.scheduleProviderCancel(ctx, tx, charge, now)
				if err != nil {
					return err
				}
				if ok {
					next.cancelOps = append(next.cancelOps, id)
				}
			}
			// O vínculo do cliente no provedor é gravado por último: a
			// inserção verifica a chave estrangeira do cliente (bloqueio
			// compartilhado da linha), que precisa vir depois do bloqueio da
			// assinatura feito na aplicação de um pagamento já confirmado.
			if providerCustomer != "" {
				if err := tx.SaveProviderCustomer(ctx, charge.CustomerID, charge.Provider, providerCustomer); err != nil {
					return err
				}
			}
		case outcomePermanent:
			op.Status, op.CompletedAt = OpFailed, &now
			if obsolete {
				break // a cobrança já foi encerrada localmente
			}
			if err := charge.MarkFailed(op.LastError, now); err != nil {
				return err
			}
			if err := tx.UpdateCharge(ctx, &charge); err != nil {
				return err
			}
			if err := tx.Emit(ctx, outbox.New(tenantID, outbox.EventChargeFailed, "charge", charge.ID,
				map[string]any{"charge_id": charge.ID, "invoice_id": charge.InvoiceID}, now)); err != nil {
				return err
			}
		case outcomeObsolete:
			op.Status, op.CompletedAt, op.LastError = OpFailed, &now, obsoleteOperationError
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
		return followUp{}, err
	}
	return next, nil
}

// chargeMatchesBalance informa se a cobrança corresponde ao saldo atual da
// fatura.
func chargeMatchesBalance(c Charge, inv billing.Invoice) bool {
	return inv.AmountDue.IsPositive() && c.Amount == inv.AmountDue
}

// replaceIfBalanceChanged ajusta a cobrança cuja criação no provedor terminou
// depois de uma alteração do saldo da fatura (por exemplo, nota de crédito ou
// pagamento manual parcial registrados enquanto a criação estava em andamento
// ou em retentativa): a cobrança acompanha o saldo vigente. Se o valor
// diverge do saldo, a cobrança é cancelada localmente e, havendo saldo, uma
// nova cobrança é gravada pelo valor atualizado na mesma transação; a chamada
// ao provedor ocorre depois do commit e, se falhar, fica com a recuperação
// agendada. O ajuste não depende de reentrega de mensagens. Faturas
// canceladas ou baixadas seguem os fluxos próprios.
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

// RecoveryResult resume uma execução da recuperação de operações.
type RecoveryResult struct {
	// Executed conta as operações executadas sem erro.
	Executed int
	// Exhausted conta, por situação, as operações paradas depois de esgotar
	// as tentativas automáticas (Operation.Exhausted) nos tenants
	// consultados.
	Exhausted map[OperationStatus]int
	// Complete indica que todos os tenants com operações vencidas foram
	// consultados sem erro; caso contrário, Exhausted é parcial e não deve
	// substituir uma contagem anterior.
	Complete bool
}

// RecoverOperations retoma operações pendentes, com lease expirado ou com
// resultado desconhecido elegível a nova consulta, e conta as que
// esgotaram as tentativas automáticas, que a recuperação não reivindica
// mais. A descoberta de tenants inclui as operações esgotadas (a função
// do banco não filtra tentativas), de modo que a contagem cobre todos os
// tenants afetados.
func (s *Service) RecoverOperations(ctx context.Context) (RecoveryResult, error) {
	now := s.clock.Now()
	res := RecoveryResult{Exhausted: map[OperationStatus]int{}}
	tenants, err := s.store.TenantsWithPendingOperations(ctx, now)
	if err != nil {
		return res, err
	}
	complete := true
	var errs []error
	for _, t := range tenants {
		var (
			opIDs     []uuid.UUID
			exhausted map[OperationStatus]int
		)
		err := s.store.InTenant(ctx, t, func(tx Tx) error {
			var err error
			if opIDs, err = tx.DueOperationIDs(ctx, now, 50); err != nil {
				return err
			}
			exhausted, err = tx.ExhaustedOperations(ctx, now)
			return err
		})
		if err != nil {
			complete = false
			errs = append(errs, err)
			continue
		}
		for status, n := range exhausted {
			res.Exhausted[status] += n
		}
		for _, id := range opIDs {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			if err := s.ExecuteOperation(ctx, t, id); err != nil {
				errs = append(errs, err)
				continue
			}
			res.Executed++
		}
	}
	res.Complete = complete
	return res, errors.Join(errs...)
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
	authoritative := false
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
		snapshot, authoritative = &pc, true
	} else if found {
		snapshot = ev.Charge
		if snapshot != nil && snapshot.UpdatedAt == nil && ev.OccurredAt != nil {
			// Sem instante no retrato, vale o instante do evento: permite
			// descartar retratos anteriores ao último registrado.
			cp := *snapshot
			cp.UpdatedAt = ev.OccurredAt
			snapshot = &cp
		}
	}
	var conflict error
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		conflict = nil
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
		if err := s.applyProviderStateFrom(ctx, tx, &c, *snapshot, now, providerActor, authoritative); err != nil {
			if apperr.IsKind(err, apperr.KindConflict) {
				// O conflito pode surgir depois de efeitos já gravados na
				// transação (por exemplo, o primeiro de dois reembolsos
				// confirmados ou o reembolso antes do consumo do crédito):
				// a transação é desfeita por inteiro e o webhook é marcado
				// como ignorado numa transação própria.
				conflict = err
				return errWebhookConflict
			}
			return err
		}
		return tx.UpdateWebhookStatus(ctx, webhookID, "processed", "", now)
	})
	if !errors.Is(err, errWebhookConflict) {
		return err
	}
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerWebhooks, messageID)
		if err != nil || !first {
			return err
		}
		return tx.UpdateWebhookStatus(ctx, webhookID, "ignored", conflict.Error(), s.clock.Now())
	})
}

// errWebhookConflict desfaz a transação do webhook cujo estado não pôde ser
// aplicado por conflito com o estado local.
var errWebhookConflict = errors.New("payments: webhook em conflito com o estado local")

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
// regredir estados e de forma idempotente. A confirmação de pagamento vale
// também para cobrança recusada ou cancelada localmente (ver MarkPaid). Em
// cobrança paga, sincroniza reembolsos (dados cumulativos, aplicáveis em
// qualquer ordem) e disputas (somente com retrato não anterior ao último
// registrado).
func (s *Service) applyProviderState(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor) error {
	return s.applyProviderStateFrom(ctx, tx, c, pc, now, actor, false)
}

// applyProviderStateFrom implementa applyProviderState; authoritative indica
// retrato obtido por consulta ao provedor (não por evento).
func (s *Service) applyProviderStateFrom(ctx context.Context, tx Tx, c *Charge, pc ProviderCharge, now time.Time, actor audit.Actor, authoritative bool) error {
	prevAt := c.ProviderStatusAt
	c.RecordProviderStatus(pc.RawStatus, pc.UpdatedAt)
	older := statusRank(pc.Status) < localRank(*c) && pc.Status != ProviderFailed && pc.Status != ProviderCanceled
	if !older {
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
	}
	if IsSettledStatus(c.Status) {
		if err := s.syncRefunds(ctx, tx, c, pc, now, actor); err != nil {
			return err
		}
		stale := pc.UpdatedAt != nil && prevAt != nil && pc.UpdatedAt.Before(*prevAt)
		if !stale {
			if err := s.syncDispute(ctx, tx, c, pc, now, actor, authoritative); err != nil {
				return err
			}
		}
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
	// Cobrança recusada ou cancelada apenas no Recorvia continua válida no
	// provedor: a confirmação prevalece e o valor segue para a fatura
	// (excedente vira crédito do cliente), nunca é descartado.
	previous, failureReason := c.Status, c.FailureReason
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
	if action, ok := paidAfterClosureAudit[previous]; ok {
		details := map[string]any{"invoice_id": inv.ID, "payment_id": pay.ID, "amount": gross.Amount(), "invoice_status": string(inv.Status),
			"previous_status": string(previous)}
		if failureReason != "" {
			details["failure_reason"] = failureReason
		}
		if err := tx.Audit(ctx, audit.New(c.TenantID, now, actor, action, "charge", c.ID.String(), details)); err != nil {
			return err
		}
	}
	return s.applyToInvoice(ctx, tx, &inv, pay, true, actor, now)
}

// paidAfterClosureAudit identifica, na auditoria, a confirmação de pagamento
// de cobrança que estava recusada ou cancelada localmente (transição
// incomum, que merece verificação na conciliação).
var paidAfterClosureAudit = map[ChargeStatus]string{
	ChargeFailed:   "charge.paid_after_failure",
	ChargeCanceled: "charge.paid_after_local_cancellation",
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
	// A situação da assinatura é atualizada antes do crédito do excedente:
	// a assinatura é bloqueada antes do cliente, na mesma ordem do motor, das
	// mudanças de assinatura, da correção de consumo e da ingestão, para que
	// essas transações se serializem sem impasse.
	event := outbox.EventInvoicePartiallyPaid
	if inv.Status == billing.Paid {
		event = outbox.EventInvoicePaid
		if inv.SubscriptionID != nil {
			if err := tx.RefreshSubscriptionStanding(ctx, *inv.SubscriptionID, actor, now); err != nil {
				return err
			}
		}
	}
	if excess.IsPositive() {
		if err := s.creditOverpayment(ctx, tx, inv, pay, excess, viaProvider, actor, now); err != nil {
			return err
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
// cancelada (consumidor de invoice.voided) e as cancela no provedor depois
// do commit. Cobranças ainda em criação são canceladas no provedor quando a
// criação termina (recordOutcome).
func (s *Service) CancelOpenCharges(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	var cancelOps []uuid.UUID
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cancelOps = nil
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
			id, ok, err := s.scheduleProviderCancel(ctx, tx, charges[i], now)
			if err != nil {
				return err
			}
			if ok {
				cancelOps = append(cancelOps, id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.executeAll(ctx, tenantID, cancelOps)
	return nil
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
	var cancelOps []uuid.UUID
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cancelOps = nil
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
			id, ok, err := s.scheduleProviderCancel(ctx, tx, *c, now)
			if err != nil {
				return err
			}
			if ok {
				cancelOps = append(cancelOps, id)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.executeAll(ctx, tenantID, cancelOps)
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
