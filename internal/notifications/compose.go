package notifications

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// MaxRecipients limita os destinatários de uma notificação.
const MaxRecipients = 20

// Organization reúne os dados do tenant usados nas mensagens.
type Organization struct {
	Name     string
	Location *time.Location
}

// InvoiceInfo é a visão da fatura usada nas notificações.
type InvoiceInfo struct {
	ID             uuid.UUID
	Number         string
	Status         billing.Status
	CustomerID     uuid.UUID
	CustomerName   string
	SubscriptionID *uuid.UUID
	Total          int64
	AmountDue      int64
	AmountPaid     int64
	AmountCredited int64
	IssuedAt       *time.Time
	DueAt          *time.Time
}

// Open informa se a fatura ainda tem saldo a cobrar.
func (i InvoiceInfo) Open() bool {
	return (i.Status == billing.Open || i.Status == billing.PartiallyPaid) && i.AmountDue > 0 && i.DueAt != nil && i.IssuedAt != nil
}

// Overdue informa se a fatura está vencida e em aberto.
func (i InvoiceInfo) Overdue(now time.Time) bool { return i.Open() && !now.Before(*i.DueAt) }

// ResolutionReason devolve o motivo que interrompe os lembretes de uma fatura
// que deixou de estar em aberto; vazio enquanto houver saldo a cobrar.
func (i InvoiceInfo) ResolutionReason() string {
	switch i.Status {
	case billing.Void:
		return ReasonInvoiceVoided
	case billing.Uncollectible:
		return ReasonInvoiceUncollectible
	case billing.Paid:
		if i.AmountPaid == 0 && i.AmountCredited > 0 {
			return ReasonInvoiceCredited
		}
		return ReasonInvoicePaid
	}
	if i.AmountDue <= 0 {
		return ReasonInvoicePaid
	}
	return ""
}

// DueDate devolve a data civil de vencimento no fuso informado.
func (i InvoiceInfo) DueDate(loc *time.Location) calendar.Date { return billing.DueDate(*i.DueAt, loc) }

// PaymentInfo é a visão de um recebimento.
type PaymentInfo struct {
	ID        uuid.UUID
	InvoiceID uuid.UUID
	Amount    int64
	PaidAt    time.Time
}

// InvitationInfo é a visão de um convite de membro.
type InvitationInfo struct {
	ID        uuid.UUID
	Email     string
	Role      tenancy.Role
	ExpiresAt time.Time
	Pending   bool
}

// InvoiceVars monta as variáveis comuns das mensagens de fatura.
func InvoiceVars(org Organization, inv InvoiceInfo) map[string]string {
	v := map[string]string{
		VarOrganizationName: org.Name,
		VarCustomerName:     inv.CustomerName,
		VarInvoiceNumber:    inv.Number,
		VarInvoiceTotal:     FormatMoney(inv.Total),
		VarAmountDue:        FormatMoney(inv.AmountDue),
	}
	if inv.DueAt != nil {
		v[VarDueDate] = FormatDate(inv.DueDate(org.Location))
	}
	return v
}

// FormatMoney formata centavos em reais no padrão brasileiro ("R$ 1.234,56").
func FormatMoney(minor int64) string {
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = uint64(-(minor + 1)) + 1
	}
	whole, cents := u/100, u%100
	digits := strconv.FormatUint(whole, 10)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	s := fmt.Sprintf("R$ %s,%02d", b.String(), cents)
	if neg {
		return "-" + s
	}
	return s
}

// FormatDate formata a data civil como dd/mm/aaaa.
func FormatDate(d calendar.Date) string {
	return fmt.Sprintf("%02d/%02d/%04d", d.Day, int(d.Month), d.Year)
}

// FormatDateTime formata o instante no fuso como "dd/mm/aaaa às HH:MM".
func FormatDateTime(t time.Time, loc *time.Location) string {
	l := t.In(loc)
	return fmt.Sprintf("%s às %02d:%02d", FormatDate(calendar.DateOf(t, loc)), l.Hour(), l.Minute())
}

var roleLabels = map[tenancy.Role]string{
	tenancy.RoleOwner: "proprietário", tenancy.RoleAdmin: "administrador", tenancy.RoleFinance: "financeiro",
	tenancy.RoleOperator: "operador", tenancy.RoleAuditor: "auditor",
}

// RoleLabel devolve o nome do papel exibido nas mensagens.
func RoleLabel(r tenancy.Role) string {
	if l, ok := roleLabels[r]; ok {
		return l
	}
	return string(r)
}

// NormalizeRecipients remove espaços, duplicidades (sem diferenciar
// maiúsculas) e endereços vazios, preservando a ordem, até MaxRecipients.
func NormalizeRecipients(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range in {
		r = strings.TrimSpace(r)
		key := strings.ToLower(r)
		if r == "" || seen[key] || strings.ContainsAny(r, "\r\n,;<>") {
			continue
		}
		seen[key] = true
		out = append(out, r)
		if len(out) == MaxRecipients {
			break
		}
	}
	return out
}

// TemplateSource fornece o template personalizado do tenant, se houver.
type TemplateSource interface {
	CustomTemplate(ctx context.Context, key TemplateKey) (*Template, error)
}

// Request descreve uma notificação a compor.
type Request struct {
	BusinessKey    string
	Key            TemplateKey
	Recipients     []string
	Vars           map[string]string
	CustomerID     *uuid.UUID
	InvoiceID      *uuid.UUID
	SubscriptionID *uuid.UUID
	InvitationID   *uuid.UUID
	PolicyID       *uuid.UUID
	StepOffsetDays *int
	SendAfter      time.Time
	// SuppressReason força a supressão (por exemplo, token indisponível).
	SuppressReason string
}

// ResolveTemplate devolve o template personalizado do tenant ou o padrão.
func ResolveTemplate(ctx context.Context, src TemplateSource, key TemplateKey) (Template, error) {
	def, ok := DefaultTemplate(key)
	if !ok {
		return Template{}, fmt.Errorf("notifications: template desconhecido %q", key)
	}
	if !key.Customizable() || src == nil {
		return def, nil
	}
	custom, err := src.CustomTemplate(ctx, key)
	if err != nil {
		return Template{}, err
	}
	if custom == nil {
		return def, nil
	}
	return *custom, nil
}

// Compose renderiza o template e cria a notificação agendada (ou suprimida,
// sem destinatários). A gravação é feita pelo chamador, na sua transação.
func Compose(ctx context.Context, src TemplateSource, req Request, now time.Time) (Notification, error) {
	if l := len(req.BusinessKey); l < 3 || l > 200 {
		return Notification{}, fmt.Errorf("notifications: identidade de negócio inválida %q", req.BusinessKey)
	}
	if (req.PolicyID == nil) != (req.StepOffsetDays == nil) {
		return Notification{}, fmt.Errorf("notifications: passo da régua sem política (ou o contrário)")
	}
	t, err := ResolveTemplate(ctx, src, req.Key)
	if err != nil {
		return Notification{}, err
	}
	r, err := Render(t, req.Vars)
	if err != nil {
		return Notification{}, err
	}
	sendAfter := req.SendAfter
	if sendAfter.Before(now) {
		sendAfter = now
	}
	n := Notification{ID: ids.New(), BusinessKey: req.BusinessKey, TemplateKey: req.Key, Channel: ChannelEmail,
		Recipients: NormalizeRecipients(req.Recipients), Subject: r.Subject, Content: Content{Text: r.Text, HTML: r.HTML},
		CustomerID: req.CustomerID, InvoiceID: req.InvoiceID, SubscriptionID: req.SubscriptionID, InvitationID: req.InvitationID,
		PolicyID: req.PolicyID, StepOffsetDays: req.StepOffsetDays, Status: Scheduled, SendAfter: sendAfter.UTC(),
		MaxAttempts: DefaultMaxAttempts, CreatedAt: now, UpdatedAt: now}
	switch {
	case req.SuppressReason != "":
		n.Status, n.StatusReason = Suppressed, req.SuppressReason
		n.record(req.SuppressReason, "", now)
	case len(n.Recipients) == 0:
		n.Status, n.StatusReason = Suppressed, ReasonNoRecipient
		n.record(ReasonNoRecipient, "", now)
	default:
		n.record(ReasonScheduled, "", now)
	}
	return n, nil
}

// Business keys estáveis das notificações.

// ReminderKey identifica o lembrete de uma fatura num passo da régua.
func ReminderKey(invoiceID uuid.UUID, offsetDays int) string {
	return "reminder:" + invoiceID.String() + ":" + strconv.Itoa(offsetDays)
}

// InvoiceIssuedKey identifica o aviso de fatura emitida.
func InvoiceIssuedKey(invoiceID uuid.UUID) string { return "invoice_issued:" + invoiceID.String() }

// PaymentConfirmedKey identifica o aviso de pagamento confirmado.
func PaymentConfirmedKey(paymentID uuid.UUID) string {
	return "payment_confirmed:" + paymentID.String()
}

// SuspensionKey identifica o aviso de suspensão pela política.
func SuspensionKey(suspensionID uuid.UUID) string {
	return "subscription_suspended:" + suspensionID.String()
}

// InvitationKey identifica o e-mail de convite.
func InvitationKey(invitationID uuid.UUID) string {
	return "member_invitation:" + invitationID.String()
}

// IsReminderKey informa se a chave é de template de lembrete.
func IsReminderKey(k TemplateKey) bool {
	return slices.Contains([]TemplateKey{KeyReminderBeforeDue, KeyReminderAfterDue}, k)
}
