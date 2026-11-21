// Package notifications modela as notificações por e-mail do tenant aos
// clientes (fatura emitida, lembretes da régua de cobrança, pagamento
// confirmado, suspensão) e aos membros convidados.
//
// Cada notificação tem uma identidade de negócio (business key) única por
// tenant: o reprocessamento de eventos ou execuções concorrentes do job da
// régua não duplicam o envio. O conteúdo é renderizado a partir do template
// do tenant (ou do padrão do sistema) com uma lista fechada de variáveis e
// escape de HTML; o envio é assíncrono, com lease, retentativas limitadas e
// histórico de cada mudança de situação.
package notifications

import (
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// TemplateKey identifica um template de notificação.
type TemplateKey string

// Templates do sistema. Os cinco primeiros podem ser personalizados pelo
// tenant; o convite de membro usa sempre o padrão do sistema.
const (
	KeyInvoiceIssued         TemplateKey = "invoice_issued"
	KeyReminderBeforeDue     TemplateKey = "reminder_before_due"
	KeyReminderAfterDue      TemplateKey = "reminder_after_due"
	KeyPaymentConfirmed      TemplateKey = "payment_confirmed"
	KeySubscriptionSuspended TemplateKey = "subscription_suspended"
	KeyMemberInvitation      TemplateKey = "member_invitation"
)

// ChannelEmail é o único canal de envio disponível.
const ChannelEmail = "email"

// Variáveis permitidas nos templates (lista fechada).
const (
	VarOrganizationName = "organization_name"
	VarCustomerName     = "customer_name"
	VarInvoiceNumber    = "invoice_number"
	VarInvoiceTotal     = "invoice_total"
	VarAmountDue        = "amount_due"
	VarDueDate          = "due_date"
	VarDaysUntilDue     = "days_until_due"
	VarDaysOverdue      = "days_overdue"
	VarAmountPaid       = "amount_paid"
	VarPaymentDate      = "payment_date"
	VarRole             = "role"
	VarAcceptURL        = "accept_url"
	VarExpiresAt        = "expires_at"
)

var invoiceVars = []string{VarOrganizationName, VarCustomerName, VarInvoiceNumber, VarInvoiceTotal, VarAmountDue, VarDueDate}

var allowedVars = map[TemplateKey][]string{
	KeyInvoiceIssued:         invoiceVars,
	KeyReminderBeforeDue:     append(slices.Clone(invoiceVars), VarDaysUntilDue),
	KeyReminderAfterDue:      append(slices.Clone(invoiceVars), VarDaysOverdue),
	KeyPaymentConfirmed:      append(slices.Clone(invoiceVars), VarAmountPaid, VarPaymentDate),
	KeySubscriptionSuspended: append(slices.Clone(invoiceVars), VarDaysOverdue),
	KeyMemberInvitation:      {VarOrganizationName, VarRole, VarAcceptURL, VarExpiresAt},
}

// urlVars são renderizadas como link na versão HTML (somente http/https).
var urlVars = map[string]bool{VarAcceptURL: true}

// CustomizableKeys lista, em ordem estável, os templates que o tenant pode
// personalizar.
func CustomizableKeys() []TemplateKey {
	return []TemplateKey{KeyInvoiceIssued, KeyReminderBeforeDue, KeyReminderAfterDue, KeyPaymentConfirmed, KeySubscriptionSuspended}
}

// Customizable informa se o tenant pode personalizar o template.
func (k TemplateKey) Customizable() bool { return slices.Contains(CustomizableKeys(), k) }

// Variables lista as variáveis permitidas no template.
func (k TemplateKey) Variables() []string { return slices.Clone(allowedVars[k]) }

// ParseCustomizableKey valida a chave de um template personalizável.
func ParseCustomizableKey(s string) (TemplateKey, error) {
	k := TemplateKey(s)
	if !k.Customizable() {
		return "", apperr.NotFound("template_not_found", "Template de notificação desconhecido.")
	}
	return k, nil
}

// Template é o assunto e o corpo (texto simples com variáveis {{nome}}) de
// uma notificação.
type Template struct {
	Key       TemplateKey
	Subject   string
	Body      string
	IsDefault bool
	UpdatedAt *time.Time
	Version   int
}

// InvoiceFiscalNotice é o aviso do e-mail padrão de fatura emitida (RF-06):
// a fatura comercial não substitui a nota fiscal, como no documento da
// fatura. Fica no padrão do sistema; o template editado pelo tenant é
// enviado como foi gravado.
const InvoiceFiscalNotice = "A fatura comercial não é documento fiscal e não substitui a nota fiscal."

var defaults = map[TemplateKey]Template{
	KeyInvoiceIssued: {Subject: "Fatura {{invoice_number}} emitida", Body: "Olá, {{customer_name}}.\n\n" +
		"A fatura {{invoice_number}} de {{organization_name}} foi emitida no valor de {{amount_due}}, com vencimento em {{due_date}}.\n\n" +
		InvoiceFiscalNotice + "\n\n" +
		"Em caso de dúvidas, entre em contato com {{organization_name}}."},
	KeyReminderBeforeDue: {Subject: "Lembrete: a fatura {{invoice_number}} vence em {{due_date}}", Body: "Olá, {{customer_name}}.\n\n" +
		"Lembramos que a fatura {{invoice_number}} de {{organization_name}}, com saldo em aberto de {{amount_due}}, vence em {{due_date}}.\n\n" +
		"Se o pagamento já foi realizado, desconsidere esta mensagem."},
	KeyReminderAfterDue: {Subject: "Fatura {{invoice_number}} vencida em {{due_date}}", Body: "Olá, {{customer_name}}.\n\n" +
		"Não identificamos o pagamento da fatura {{invoice_number}} de {{organization_name}}, vencida em {{due_date}} " +
		"(há {{days_overdue}} dia(s)), com saldo em aberto de {{amount_due}}.\n\n" +
		"Se o pagamento já foi realizado, desconsidere esta mensagem."},
	KeyPaymentConfirmed: {Subject: "Pagamento recebido: fatura {{invoice_number}}", Body: "Olá, {{customer_name}}.\n\n" +
		"Confirmamos o recebimento de {{amount_paid}} em {{payment_date}}, referente à fatura {{invoice_number}} de {{organization_name}}. " +
		"Saldo em aberto: {{amount_due}}.\n\nObrigado."},
	KeySubscriptionSuspended: {Subject: "Assinatura suspensa por falta de pagamento", Body: "Olá, {{customer_name}}.\n\n" +
		"A assinatura com {{organization_name}} foi suspensa porque a fatura {{invoice_number}}, vencida em {{due_date}}, " +
		"está em aberto há {{days_overdue}} dia(s), com saldo de {{amount_due}}.\n\n" +
		"A regularização do pagamento permite a reativação conforme a política de cobrança de {{organization_name}}."},
	KeyMemberInvitation: {Subject: "Convite para participar de {{organization_name}} no Recorvia", Body: "Olá.\n\n" +
		"Você foi convidado a participar da organização {{organization_name}} no Recorvia com o papel {{role}}.\n\n" +
		"Para aceitar o convite, acesse o endereço abaixo com a conta deste e-mail:\n{{accept_url}}\n\n" +
		"O convite expira em {{expires_at}}. Se você não esperava esta mensagem, ignore-a."},
}

// DefaultTemplate devolve o padrão do sistema para a chave.
func DefaultTemplate(k TemplateKey) (Template, bool) {
	t, ok := defaults[k]
	if !ok {
		return Template{}, false
	}
	t.Key, t.IsDefault = k, true
	return t, true
}

// Limites do conteúdo personalizado (os mesmos do contrato da API).
const (
	MinSubjectLen = 3
	MaxSubjectLen = 200
	MinBodyLen    = 10
	MaxBodyLen    = 10000
	// MaxRenderedSubjectLen limita o assunto já renderizado.
	MaxRenderedSubjectLen = 300
)

var varName = regexp.MustCompile(`^[a-z][a-z_]{0,49}$`)

type segment struct {
	text     string
	variable string
}

// parse separa o texto em trechos literais e variáveis {{nome}}.
func parse(s string) ([]segment, error) {
	var out []segment
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			if strings.Contains(s, "}}") {
				return nil, fmt.Errorf("fechamento }} sem abertura correspondente")
			}
			if s != "" {
				out = append(out, segment{text: s})
			}
			return out, nil
		}
		if strings.Contains(s[:i], "}}") {
			return nil, fmt.Errorf("fechamento }} sem abertura correspondente")
		}
		if i > 0 {
			out = append(out, segment{text: s[:i]})
		}
		rest := s[i+2:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return nil, fmt.Errorf("variável sem fechamento }}")
		}
		name := strings.TrimSpace(rest[:j])
		if !varName.MatchString(name) {
			return nil, fmt.Errorf("nome de variável inválido: %q", truncateRunes(name, 50))
		}
		out = append(out, segment{variable: name})
		s = rest[j+2:]
	}
}

func checkVariables(key TemplateKey, segs []segment) error {
	allowed := allowedVars[key]
	for _, sg := range segs {
		if sg.variable != "" && !slices.Contains(allowed, sg.variable) {
			return fmt.Errorf("variável não permitida neste template: {{%s}}", sg.variable)
		}
	}
	return nil
}

// ValidateTemplate valida assunto e corpo personalizados: tamanhos, texto
// válido e somente variáveis permitidas para a chave.
func ValidateTemplate(key TemplateKey, subject, body string) (Template, error) {
	subject, body = strings.TrimSpace(subject), strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
	var f []apperr.FieldError
	check := func(field, value string, minLen, maxLen int, singleLine bool) {
		n := utf8.RuneCountInString(value)
		switch {
		case !utf8.ValidString(value) || strings.ContainsRune(value, 0):
			f = append(f, apperr.FieldError{Field: field, Message: "O texto contém caracteres inválidos."})
			return
		case n < minLen || n > maxLen:
			f = append(f, apperr.FieldError{Field: field, Message: fmt.Sprintf("Informe entre %d e %d caracteres.", minLen, maxLen)})
			return
		case singleLine && strings.ContainsAny(value, "\r\n"):
			f = append(f, apperr.FieldError{Field: field, Message: "O assunto deve ocupar uma única linha."})
			return
		}
		segs, err := parse(value)
		if err == nil {
			err = checkVariables(key, segs)
		}
		if err != nil {
			f = append(f, apperr.FieldError{Field: field, Message: capitalize(err.Error()) + "."})
		}
	}
	check("subject", subject, MinSubjectLen, MaxSubjectLen, true)
	check("body", body, MinBodyLen, MaxBodyLen, false)
	if len(f) > 0 {
		return Template{}, apperr.Invalid("invalid_template", "Template inválido.", f...)
	}
	return Template{Key: key, Subject: subject, Body: body}, nil
}

// Rendered é o conteúdo final de uma notificação.
type Rendered struct {
	Subject string
	Text    string
	HTML    string
}

// Render substitui as variáveis do template. O assunto é reduzido a uma linha
// (sem quebras que permitam injeção de cabeçalhos); a versão HTML escapa todo
// o texto, inclusive o do tenant, e só gera links para variáveis de URL com
// esquema http ou https. Variáveis permitidas sem valor viram texto vazio.
func Render(t Template, vars map[string]string) (Rendered, error) {
	subjectSegs, err := parse(t.Subject)
	if err == nil {
		err = checkVariables(t.Key, subjectSegs)
	}
	if err != nil {
		return Rendered{}, fmt.Errorf("notifications: assunto do template %s: %w", t.Key, err)
	}
	bodySegs, err := parse(t.Body)
	if err == nil {
		err = checkVariables(t.Key, bodySegs)
	}
	if err != nil {
		return Rendered{}, fmt.Errorf("notifications: corpo do template %s: %w", t.Key, err)
	}
	clean := make(map[string]string, len(vars))
	for k, v := range vars {
		clean[k] = cleanValue(v)
	}
	var subject, text, body strings.Builder
	for _, sg := range subjectSegs {
		if sg.variable != "" {
			subject.WriteString(clean[sg.variable])
		} else {
			subject.WriteString(sg.text)
		}
	}
	for _, sg := range bodySegs {
		if sg.variable == "" {
			text.WriteString(sg.text)
			body.WriteString(html.EscapeString(sg.text))
			continue
		}
		v := clean[sg.variable]
		text.WriteString(v)
		if urlVars[sg.variable] && (strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")) {
			esc := html.EscapeString(v)
			body.WriteString(`<a href="` + esc + `">` + esc + `</a>`)
			continue
		}
		body.WriteString(html.EscapeString(v))
	}
	return Rendered{Subject: singleLine(subject.String()), Text: text.String(), HTML: htmlDocument(body.String())}, nil
}

// htmlDocument converte o corpo já escapado em parágrafos.
func htmlDocument(escaped string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html lang="pt-BR"><head><meta charset="utf-8"></head>`)
	b.WriteString(`<body style="font-family: Arial, Helvetica, sans-serif; font-size: 15px; line-height: 1.5; color: #1f2933;">`)
	for _, p := range strings.Split(escaped, "\n\n") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		b.WriteString("<p>" + strings.ReplaceAll(p, "\n", "<br>") + "</p>")
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

// cleanValue remove caracteres de controle (exceto quebra de linha e
// tabulação) e texto UTF-8 inválido dos valores das variáveis.
func cleanValue(v string) string {
	v = strings.ToValidUTF8(v, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, v)
}

// singleLine reduz o texto a uma linha com espaços simples e limita o tamanho.
func singleLine(s string) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), MaxRenderedSubjectLen)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
