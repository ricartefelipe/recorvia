package notifications

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/tenantscan"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Consumidores de eventos deste módulo.
const (
	ConsumerInvoiceIssued    = "notifications.invoice_issued"
	ConsumerPaymentConfirmed = "notifications.payment_confirmed"
	ConsumerMemberInvited    = "notifications.member_invited"
)

// ReminderRules são as regras de envio de lembretes da política de cobrança.
type ReminderRules struct {
	Window    Window
	MaxPerDay int
}

// ListFilter filtra o histórico de notificações.
type ListFilter struct {
	Status    Status
	InvoiceID *uuid.UUID
	Limit     int
	Cursor    string
}

// Page é uma página do histórico.
type Page struct {
	Items      []Notification
	NextCursor string
}

// Tx são as operações dentro da transação do tenant.
type Tx interface {
	TemplateSource
	CustomTemplates(ctx context.Context) ([]Template, error)
	UpsertTemplate(ctx context.Context, t Template, updatedBy *uuid.UUID, now time.Time) (Template, error)

	// InsertNotification grava a notificação e seu histórico; false indica
	// que a identidade de negócio já existia (nada é gravado).
	InsertNotification(ctx context.Context, n *Notification) (bool, error)
	// DueNotifications bloqueia (SKIP LOCKED) notificações prontas para envio.
	DueNotifications(ctx context.Context, now time.Time, limit int) ([]Notification, error)
	Notification(ctx context.Context, id uuid.UUID, forUpdate bool) (Notification, error)
	// UpdateNotification grava a situação e o histórico pendente, desde que
	// o número de tentativas ainda seja expectedAttempts; false indica que
	// outra execução reivindicou a notificação.
	UpdateNotification(ctx context.Context, n *Notification, expectedAttempts int) (bool, error)
	ListNotifications(ctx context.Context, f ListFilter) (Page, error)
	// NotificationEvents devolve o histórico da notificação em ordem
	// cronológica.
	NotificationEvents(ctx context.Context, id uuid.UUID) ([]Event, error)
	NewerReminderExists(ctx context.Context, invoiceID uuid.UUID, offsetDays int) (bool, error)
	// TryLockCustomer serializa a contagem do limite de frequência por
	// cliente; false indica que outra transação detém o bloqueio.
	TryLockCustomer(ctx context.Context, customerID uuid.UUID) (bool, error)
	// RemindersSent conta os lembretes do cliente enviados em [from, to) ou
	// em envio, sem contar a notificação excludeID (a que está sendo avaliada).
	RemindersSent(ctx context.Context, customerID, excludeID uuid.UUID, from, to time.Time) (int, error)
	ReminderRules(ctx context.Context, policyID uuid.UUID) (ReminderRules, error)
	// SuspensionWindow devolve a janela de envio da política que aplicou a
	// suspensão; false quando a suspensão não existe.
	SuspensionWindow(ctx context.Context, suspensionID uuid.UUID) (Window, bool, error)

	Organization(ctx context.Context) (Organization, error)
	CustomerRecipients(ctx context.Context, customerID uuid.UUID) ([]string, error)
	Invoice(ctx context.Context, id uuid.UUID) (InvoiceInfo, error)
	Payment(ctx context.Context, id uuid.UUID) (PaymentInfo, error)
	Invitation(ctx context.Context, id uuid.UUID, now time.Time) (InvitationInfo, error)
	SubscriptionSuspended(ctx context.Context, id uuid.UUID) (bool, error)
	// SuspensionActive informa se a suspensão está ativa; found=false quando
	// a suspensão não existe.
	SuspensionActive(ctx context.Context, suspensionID uuid.UUID) (active, found bool, err error)
	PlatformPlan(ctx context.Context) (platformbilling.Plan, error)

	MarkInbox(ctx context.Context, consumer string, messageID uuid.UUID) (bool, error)
	Audit(ctx context.Context, e audit.Entry) error
}

// Store abre transações e descobre trabalho pendente.
type Store interface {
	InTenant(ctx context.Context, tenantID uuid.UUID, fn func(Tx) error) error
	// TenantsWithDueNotifications devolve até limit tenants com notificações
	// a enviar e ID maior que after (nil: desde o início), em ordem de ID.
	TenantsWithDueNotifications(ctx context.Context, now time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error)
}

// Email é a mensagem entregue ao canal de envio.
type Email struct {
	ID       uuid.UUID
	FromName string
	To       []string
	Subject  string
	Text     string
	HTML     string
	Date     time.Time
}

// Receipt é o resultado de um envio aceito pelo canal.
type Receipt struct {
	MessageID string
	// Refused descreve os destinatários recusados pelo servidor; os demais
	// receberam a mensagem.
	Refused []string
}

// Sender entrega e-mails. Devolve erro somente quando nenhum destinatário
// recebeu a mensagem.
type Sender interface {
	Send(ctx context.Context, e Email) (Receipt, error)
}

// Opener decifra o token do convite recebido no evento member.invited.
type Opener interface {
	Decrypt(ciphertext []byte, keyID string, aad []byte) ([]byte, error)
}

// Config parametriza o envio.
type Config struct {
	// Lease é o tempo reservado a uma tentativa; deve exceder SendTimeout.
	Lease       time.Duration
	SendTimeout time.Duration
	BatchSize   int
	// PublicURL é a base dos links enviados (aceite de convite).
	PublicURL string
	// IsPermanent classifica erros do canal que não devem ser repetidos.
	IsPermanent func(error) bool
	// Jitter devolve um valor em [0, 1) para espalhar retentativas.
	Jitter func() float64
	// TenantPageSize é o tamanho das páginas de descoberta de tenants.
	TenantPageSize int
	Log            *slog.Logger
}

// recordTimeout limita a gravação do resultado de um envio, feita mesmo com
// o job cancelado (desligamento do worker ou tempo limite da tarefa).
const recordTimeout = 10 * time.Second

// Service implementa templates, histórico, consumidores e envio.
type Service struct {
	store    Store
	sender   Sender
	opener   Opener
	clock    clock.Clock
	cfg      Config
	rotation tenantscan.Rotation
}

// NewService cria o serviço. sender e opener podem ser nulos em executáveis
// que não enviam e-mails (API).
func NewService(store Store, sender Sender, opener Opener, c clock.Clock, cfg Config) *Service {
	if cfg.Lease <= 0 {
		cfg.Lease = 2 * time.Minute
	}
	if cfg.SendTimeout <= 0 || cfg.SendTimeout >= cfg.Lease {
		cfg.SendTimeout = cfg.Lease / 2
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
	if cfg.TenantPageSize <= 0 {
		cfg.TenantPageSize = tenantscan.PageSize
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: store, sender: sender, opener: opener, clock: c, cfg: cfg}
}

// Templates lista os templates personalizáveis: o do tenant, quando
// existir, ou o padrão do sistema.
func (s *Service) Templates(ctx context.Context, tenantID uuid.UUID) ([]Template, error) {
	var custom []Template
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		custom, err = tx.CustomTemplates(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	byKey := map[TemplateKey]Template{}
	for _, t := range custom {
		byKey[t.Key] = t
	}
	out := make([]Template, 0, len(CustomizableKeys()))
	for _, k := range CustomizableKeys() {
		if t, ok := byKey[k]; ok {
			out = append(out, t)
			continue
		}
		def, _ := DefaultTemplate(k)
		out = append(out, def)
	}
	return out, nil
}

// UpdateTemplate grava o template personalizado do tenant.
func (s *Service) UpdateTemplate(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, key, subject, body string) (Template, error) {
	k, err := ParseCustomizableKey(key)
	if err != nil {
		return Template{}, err
	}
	t, err := ValidateTemplate(k, subject, body)
	if err != nil {
		return Template{}, err
	}
	now := s.clock.Now()
	var out Template
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if out, err = tx.UpsertTemplate(ctx, t, userID, now); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "notification_template.updated", "notification_template", string(k),
			map[string]any{"version": out.Version}))
	})
	return out, err
}

// List devolve o histórico de notificações.
func (s *Service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) (Page, error) {
	switch f.Status {
	case "", Scheduled, Sending, Sent, Failed, Canceled, Suppressed:
	default:
		return Page{}, apperr.Invalid("invalid_filter", "Situação inválida.", apperr.FieldError{Field: "status", Message: "Situação desconhecida."})
	}
	var p Page
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		p, err = tx.ListNotifications(ctx, f)
		return err
	})
	return p, err
}

// Events devolve a linha do tempo de uma notificação (agendamento,
// adiamentos, tentativas, envio, falha, cancelamento ou supressão).
func (s *Service) Events(ctx context.Context, tenantID, id uuid.UUID) ([]Event, error) {
	var out []Event
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		if _, err := tx.Notification(ctx, id, false); err != nil {
			return err
		}
		var err error
		out, err = tx.NotificationEvents(ctx, id)
		return err
	})
	return out, err
}

// --- consumidores ---

// HandleInvoiceIssued agenda o aviso de fatura emitida aos contatos de
// cobrança do cliente. É idempotente pela identidade de negócio.
func (s *Service) HandleInvoiceIssued(ctx context.Context, tenantID, invoiceID, messageID uuid.UUID) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerInvoiceIssued, messageID)
		if err != nil || !first {
			return err
		}
		inv, err := tx.Invoice(ctx, invoiceID)
		if err != nil {
			return err
		}
		if inv.Status == billing.Void || inv.IssuedAt == nil {
			return nil
		}
		return s.enqueueForInvoice(ctx, tx, inv, KeyInvoiceIssued, InvoiceIssuedKey(inv.ID), nil, now)
	})
}

// HandlePaymentConfirmed agenda o aviso de pagamento recebido.
func (s *Service) HandlePaymentConfirmed(ctx context.Context, tenantID, paymentID, messageID uuid.UUID) error {
	now := s.clock.Now()
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerPaymentConfirmed, messageID)
		if err != nil || !first {
			return err
		}
		pay, err := tx.Payment(ctx, paymentID)
		if err != nil {
			return err
		}
		inv, err := tx.Invoice(ctx, pay.InvoiceID)
		if err != nil {
			return err
		}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		extra := map[string]string{VarAmountPaid: FormatMoney(pay.Amount), VarPaymentDate: FormatDateTime(pay.PaidAt, org.Location)}
		return s.enqueueForInvoice(ctx, tx, inv, KeyPaymentConfirmed, PaymentConfirmedKey(pay.ID), extra, now)
	})
}

func (s *Service) enqueueForInvoice(ctx context.Context, tx Tx, inv InvoiceInfo, key TemplateKey, businessKey string, extra map[string]string, now time.Time) error {
	org, err := tx.Organization(ctx)
	if err != nil {
		return err
	}
	recipients, err := tx.CustomerRecipients(ctx, inv.CustomerID)
	if err != nil {
		return err
	}
	vars := InvoiceVars(org, inv)
	for k, v := range extra {
		vars[k] = v
	}
	n, err := Compose(ctx, tx, Request{BusinessKey: businessKey, Key: key, Recipients: recipients, Vars: vars,
		CustomerID: &inv.CustomerID, InvoiceID: &inv.ID, SubscriptionID: inv.SubscriptionID, SendAfter: now}, now)
	if err != nil {
		return err
	}
	_, err = tx.InsertNotification(ctx, &n)
	return err
}

// MemberInvited é o conteúdo do evento member.invited usado no e-mail.
type MemberInvited struct {
	InvitationID    uuid.UUID
	TokenCiphertext string
	TokenKeyID      string
}

// HandleMemberInvited agenda o e-mail de convite. O token chega cifrado no
// evento e só existe em claro no conteúdo cifrado da notificação.
func (s *Service) HandleMemberInvited(ctx context.Context, tenantID, messageID uuid.UUID, ev MemberInvited) error {
	now := s.clock.Now()
	token, tokenErr := s.openToken(tenantID, ev)
	return s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		first, err := tx.MarkInbox(ctx, ConsumerMemberInvited, messageID)
		if err != nil || !first {
			return err
		}
		inv, err := tx.Invitation(ctx, ev.InvitationID, now)
		if err != nil {
			return err
		}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		req := Request{BusinessKey: InvitationKey(inv.ID), Key: KeyMemberInvitation, Recipients: []string{inv.Email},
			InvitationID: &inv.ID, SendAfter: now, Vars: map[string]string{VarOrganizationName: org.Name, VarRole: RoleLabel(inv.Role),
				VarExpiresAt: FormatDateTime(inv.ExpiresAt, org.Location)}}
		switch {
		case !inv.Pending:
			req.SuppressReason = ReasonInvitationNotPending
		case tokenErr != nil:
			s.cfg.Log.WarnContext(ctx, "token do convite indisponível; e-mail suprimido", slog.String("invitation_id", inv.ID.String()),
				slog.String("error", tokenErr.Error()))
			req.SuppressReason = ReasonLinkUnavailable
		default:
			req.Vars[VarAcceptURL] = tenancy.AcceptURL(s.cfg.PublicURL, token)
		}
		n, err := Compose(ctx, tx, req, now)
		if err != nil {
			return err
		}
		_, err = tx.InsertNotification(ctx, &n)
		return err
	})
}

func (s *Service) openToken(tenantID uuid.UUID, ev MemberInvited) (string, error) {
	if s.opener == nil {
		return "", errors.New("notifications: decifrador não configurado")
	}
	if ev.TokenCiphertext == "" || ev.TokenKeyID == "" {
		return "", errors.New("notifications: evento sem token cifrado")
	}
	ct, err := base64.StdEncoding.DecodeString(ev.TokenCiphertext)
	if err != nil {
		return "", fmt.Errorf("notifications: token cifrado inválido: %w", err)
	}
	pt, err := s.opener.Decrypt(ct, ev.TokenKeyID, tenancy.InvitationTokenAAD(tenantID, ev.InvitationID))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// --- envio ---

// DispatchResult resume uma execução do envio.
type DispatchResult struct {
	Sent      int
	Failed    int
	Retried   int
	Skipped   int
	Deferred  int
	LostLease int
}

func (r *DispatchResult) add(o DispatchResult) {
	r.Sent += o.Sent
	r.Failed += o.Failed
	r.Retried += o.Retried
	r.Skipped += o.Skipped
	r.Deferred += o.Deferred
	r.LostLease += o.LostLease
}

// Dispatch envia as notificações vencidas de todos os tenants. É seguro com
// várias instâncias: cada notificação é reivindicada com lease dentro de uma
// transação curta (SKIP LOCKED), o envio ocorre fora de transação e o
// resultado só é gravado por quem detém a tentativa corrente. A descoberta é
// paginada e começa depois do último tenant da execução anterior (rodízio).
func (s *Service) Dispatch(ctx context.Context) (DispatchResult, error) {
	var res DispatchResult
	if s.sender == nil {
		return res, errors.New("notifications: canal de envio não configurado")
	}
	now := s.clock.Now()
	page := func(ctx context.Context, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
		return s.store.TenantsWithDueNotifications(ctx, now, after, limit)
	}
	var errs []error
	err := s.rotation.Each(ctx, s.cfg.TenantPageSize, page, nil, func(t uuid.UUID) {
		r, err := s.dispatchTenant(ctx, t)
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

type claimed struct {
	n   Notification
	org Organization
}

func (s *Service) dispatchTenant(ctx context.Context, tenantID uuid.UUID) (DispatchResult, error) {
	var res DispatchResult
	var work []claimed
	now := s.clock.Now()
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		work, res = nil, DispatchResult{}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		plan, err := tx.PlatformPlan(ctx)
		if err != nil {
			return err
		}
		due, err := tx.DueNotifications(ctx, now, s.cfg.BatchSize)
		if err != nil {
			return err
		}
		rules := map[uuid.UUID]ReminderRules{}
		for i := range due {
			n := &due[i]
			before := n.Attempts
			action, err := s.prepare(ctx, tx, n, org, plan, rules, now)
			if err != nil {
				return err
			}
			if action == actionSkip {
				res.Skipped++
				continue
			}
			ok, err := tx.UpdateNotification(ctx, n, before)
			if err != nil {
				return err
			}
			switch {
			case !ok:
				res.LostLease++
			case action == actionSend:
				work = append(work, claimed{n: *n, org: org})
			case action == actionDefer:
				res.Deferred++
			default:
				res.Skipped++
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	var errs []error
	for _, w := range work {
		r, err := s.send(ctx, tenantID, w)
		res.add(r)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

type action int

const (
	actionSend action = iota
	actionDefer
	actionClose
	actionSkip
)

// prepare aplica as regras de envio a uma notificação vencida: interrupção
// após resolução, preferências de contato, janela de horário e limite de
// frequência dos lembretes; se puder ser enviada, reivindica a tentativa.
func (s *Service) prepare(ctx context.Context, tx Tx, n *Notification, org Organization, plan platformbilling.Plan,
	rules map[uuid.UUID]ReminderRules, now time.Time) (action, error) {
	reason, err := s.stopReason(ctx, tx, n, plan, now)
	if err != nil {
		return 0, err
	}
	if reason != "" {
		return actionClose, n.Cancel(now, reason)
	}
	if n.CustomerID != nil {
		recipients, err := tx.CustomerRecipients(ctx, *n.CustomerID)
		if err != nil {
			return 0, err
		}
		if recipients = NormalizeRecipients(recipients); len(recipients) == 0 {
			return actionClose, n.Suppress(now, ReasonNoRecipient)
		}
		n.Recipients = recipients
	}
	if n.ContentError != "" {
		// Conteúdo que não decifra (chave retirada ou dado corrompido) não pode
		// ser enviado: encerra só esta notificação, sem bloquear as demais.
		s.cfg.Log.WarnContext(ctx, "conteúdo da notificação indisponível; notificação encerrada", slog.String("notification_id", n.ID.String()),
			slog.String("error", n.ContentError))
		return actionClose, n.Fail(now, ReasonContentUnavailable, "Conteúdo cifrado indisponível: "+n.ContentError)
	}
	if n.TemplateKey == KeySubscriptionSuspended {
		// O aviso de suspensão faz parte da régua: respeita a janela de
		// horário da política que suspendeu, no fuso do tenant (ADR-081). O
		// limite diário de frequência vale só para os lembretes.
		w, ok, err := s.suspensionWindow(ctx, tx, *n)
		if err != nil {
			return 0, err
		}
		if ok && !w.Contains(now, org.Location) {
			return actionDefer, n.Defer(now, w.Next(now, org.Location), ReasonOutsideWindow)
		}
	}
	if n.IsReminder() {
		r, ok := rules[*n.PolicyID]
		if !ok {
			if r, err = tx.ReminderRules(ctx, *n.PolicyID); err != nil {
				return 0, err
			}
			rules[*n.PolicyID] = r
		}
		if !r.Window.Contains(now, org.Location) {
			return actionDefer, n.Defer(now, r.Window.Next(now, org.Location), ReasonOutsideWindow)
		}
		if n.CustomerID != nil {
			locked, err := tx.TryLockCustomer(ctx, *n.CustomerID)
			if err != nil {
				return 0, err
			}
			if !locked {
				return actionSkip, nil // outra instância conta os envios deste cliente agora
			}
			from, to := DayBounds(now, org.Location)
			sent, err := tx.RemindersSent(ctx, *n.CustomerID, n.ID, from, to)
			if err != nil {
				return 0, err
			}
			if sent >= r.MaxPerDay {
				return actionDefer, n.Defer(now, r.Window.NextDayStart(now, org.Location), ReasonFrequencyLimit)
			}
		}
	}
	return actionSend, n.Claim(now, now.Add(s.cfg.Lease))
}

// suspensionWindow devolve a janela de envio do aviso de suspensão: a da
// política que aplicou a suspensão identificada pela chave de negócio.
// Aviso sem suspensão identificável (chave fora do padrão ou suspensão
// inexistente) não tem janela.
func (s *Service) suspensionWindow(ctx context.Context, tx Tx, n Notification) (Window, bool, error) {
	id, ok := suspensionIDFromKey(n.BusinessKey)
	if !ok {
		return Window{}, false, nil
	}
	w, found, err := tx.SuspensionWindow(ctx, id)
	if err != nil || !found || !w.Valid() {
		return Window{}, false, err
	}
	return w, true, nil
}

// stopReason devolve o motivo para não enviar mais a notificação, ou vazio.
func (s *Service) stopReason(ctx context.Context, tx Tx, n *Notification, plan platformbilling.Plan, now time.Time) (string, error) {
	if n.IsReminder() && !plan.HasFeature(platformbilling.FeatureEmailDunning) {
		return ReasonFeatureUnavailable, nil
	}
	if n.InvoiceID != nil && (n.IsReminder() || n.TemplateKey == KeyInvoiceIssued) {
		inv, err := tx.Invoice(ctx, *n.InvoiceID)
		if err != nil {
			return "", err
		}
		switch {
		case n.TemplateKey == KeyInvoiceIssued:
			if inv.Status == billing.Void {
				return ReasonInvoiceVoided, nil
			}
		case n.IsReminder():
			if r := inv.ResolutionReason(); r != "" {
				return r, nil
			}
			if n.TemplateKey == KeyReminderBeforeDue && inv.Overdue(now) {
				return ReasonDueDatePassed, nil
			}
		}
	}
	if n.IsReminder() && n.InvoiceID != nil && n.StepOffsetDays != nil {
		newer, err := tx.NewerReminderExists(ctx, *n.InvoiceID, *n.StepOffsetDays)
		if err != nil || newer {
			return ReasonSuperseded, err
		}
	}
	if n.TemplateKey == KeySubscriptionSuspended {
		// O aviso é da suspensão da chave: encerrada essa suspensão, ele não
		// sai mais, ainda que a assinatura tenha sido suspensa de novo (a nova
		// suspensão tem aviso próprio).
		if id, ok := suspensionIDFromKey(n.BusinessKey); ok {
			active, found, err := tx.SuspensionActive(ctx, id)
			if err != nil || (found && !active) {
				return ReasonSubscriptionNotActive, err
			}
		}
		if n.SubscriptionID != nil {
			suspended, err := tx.SubscriptionSuspended(ctx, *n.SubscriptionID)
			if err != nil || !suspended {
				return ReasonSubscriptionNotActive, err
			}
		}
	}
	if n.InvitationID != nil {
		inv, err := tx.Invitation(ctx, *n.InvitationID, now)
		if err != nil || !inv.Pending {
			return ReasonInvitationNotPending, err
		}
	}
	return "", nil
}

func (s *Service) send(ctx context.Context, tenantID uuid.UUID, w claimed) (DispatchResult, error) {
	var res DispatchResult
	n := w.n
	sendCtx, cancel := context.WithTimeout(ctx, s.cfg.SendTimeout)
	receipt, sendErr := s.sender.Send(sendCtx, Email{ID: n.ID, FromName: w.org.Name, To: n.Recipients, Subject: n.Subject,
		Text: n.Content.Text, HTML: n.Content.HTML, Date: s.clock.Now()})
	cancel()
	now := s.clock.Now()
	// O resultado é gravado mesmo com o job cancelado: sem o registro, a
	// notificação aceita pelo servidor seria enviada de novo após o lease.
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer wcancel()
	err := s.store.InTenant(wctx, tenantID, func(tx Tx) error {
		res = DispatchResult{}
		cur, err := tx.Notification(wctx, n.ID, true)
		if err != nil {
			return err
		}
		if cur.Status != Sending || cur.Attempts != n.Attempts {
			res.LostLease++
			return nil // outra execução assumiu a tentativa após o lease
		}
		cur.Recipients = n.Recipients
		if sendErr == nil {
			res.Sent++
			err = cur.MarkSent(now, receipt.MessageID, receipt.Refused...)
		} else {
			permanent := s.cfg.IsPermanent(sendErr)
			err = cur.MarkAttemptFailed(now, sendErr.Error(), permanent, now.Add(RetryDelay(cur.Attempts, s.cfg.Jitter())))
			if cur.Status == Failed {
				res.Failed++
			} else {
				res.Retried++
			}
		}
		if err != nil {
			return err
		}
		ok, err := tx.UpdateNotification(wctx, &cur, n.Attempts)
		if err == nil && !ok {
			res = DispatchResult{LostLease: 1}
		}
		return err
	})
	if sendErr != nil {
		s.cfg.Log.WarnContext(ctx, "falha no envio de notificação", slog.String("tenant_id", tenantID.String()),
			slog.String("notification_id", n.ID.String()), slog.Int("attempt", n.Attempts), slog.String("error", sanitizeError(sendErr.Error())))
	}
	return res, err
}

// ReasonDescription devolve a descrição do motivo de cancelamento ou
// supressão exibida no histórico; vazio para motivos sem descrição.
func ReasonDescription(reason string) string {
	switch reason {
	case ReasonNoRecipient:
		return "Sem contato que receba e-mails de cobrança."
	case ReasonLinkUnavailable:
		return "Link de aceite do convite indisponível."
	case ReasonInvoicePaid:
		return "Fatura paga."
	case ReasonInvoiceCredited:
		return "Fatura quitada por nota de crédito."
	case ReasonInvoiceVoided:
		return "Fatura cancelada."
	case ReasonInvoiceUncollectible:
		return "Fatura baixada como incobrável."
	case ReasonSuperseded:
		return "Substituída por um passo posterior da régua."
	case ReasonDueDatePassed:
		return "O vencimento já passou."
	case ReasonInvitationNotPending:
		return "O convite não está mais pendente."
	case ReasonSubscriptionNotActive:
		return "A assinatura não está mais suspensa."
	case ReasonFeatureUnavailable:
		return "Recurso indisponível no plano da plataforma."
	case ReasonContentUnavailable:
		return "O conteúdo cifrado da notificação não pôde ser lido."
	}
	return ""
}
