package notifications

import (
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

func TestValidateTemplate(t *testing.T) {
	long := strings.Repeat("a", MaxBodyLen+1)
	cases := []struct {
		name, subject, body string
		key                 TemplateKey
		field               string
	}{
		{"válido", "Fatura {{invoice_number}}", "Olá, {{ customer_name }}. Valor: {{amount_due}}.", KeyInvoiceIssued, ""},
		{"assunto curto", "Oi", "Corpo suficiente para o teste.", KeyInvoiceIssued, "subject"},
		{"corpo curto", "Assunto válido", "curto", KeyInvoiceIssued, "body"},
		{"corpo longo", "Assunto válido", long, KeyInvoiceIssued, "body"},
		{"variável não permitida", "Assunto {{accept_url}}", "Corpo suficiente para o teste.", KeyInvoiceIssued, "subject"},
		{"variável de outro template", "Assunto válido", "Atraso de {{days_overdue}} dias no corpo.", KeyReminderBeforeDue, "body"},
		{"sem fechamento", "Assunto válido", "Olá {{customer_name, corpo longo.", KeyInvoiceIssued, "body"},
		{"fechamento solto", "Assunto válido", "Olá customer_name}} corpo longo.", KeyInvoiceIssued, "body"},
		{"fechamento antes da abertura", "Assunto válido", "Olá }} e depois {{customer_name}}.", KeyInvoiceIssued, "body"},
		{"nome inválido", "Assunto válido", "Olá {{Customer-Name}} corpo longo.", KeyInvoiceIssued, "body"},
		{"assunto com quebra", "Assunto\nBcc: x@y.test", "Corpo suficiente para o teste.", KeyInvoiceIssued, "subject"},
		{"caractere nulo", "Assunto válido", "Corpo com \x00 nulo no meio.", KeyInvoiceIssued, "body"},
		{"utf-8 inválido", "Assunto válido", "Corpo inválido \xff\xfe aqui.", KeyInvoiceIssued, "body"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tpl, err := ValidateTemplate(c.key, c.subject, c.body)
			if c.field == "" {
				if err != nil {
					t.Fatalf("esperava válido: %v", err)
				}
				if tpl.Key != c.key || tpl.Subject != c.subject {
					t.Fatalf("template inesperado: %+v", tpl)
				}
				return
			}
			e, ok := apperr.As(err)
			if !ok || e.Kind != apperr.KindInvalid {
				t.Fatalf("esperava erro de validação, obtive %v", err)
			}
			found := false
			for _, f := range e.Fields {
				found = found || f.Field == c.field
			}
			if !found {
				t.Fatalf("esperava erro no campo %s: %+v", c.field, e.Fields)
			}
		})
	}
}

func TestValidateTemplateNormalizesLineEndings(t *testing.T) {
	tpl, err := ValidateTemplate(KeyInvoiceIssued, "  Assunto válido  ", "Linha 1\r\nLinha 2 com texto")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Subject != "Assunto válido" || tpl.Body != "Linha 1\nLinha 2 com texto" {
		t.Fatalf("normalização inesperada: %q %q", tpl.Subject, tpl.Body)
	}
}

func TestRenderEscapesHTMLAndSanitizesSubject(t *testing.T) {
	tpl := Template{Key: KeyMemberInvitation, Subject: "Convite {{organization_name}}",
		Body: "Olá <b>{{role}}</b>.\n\nAcesse {{accept_url}}\nExpira {{expires_at}}"}
	r, err := Render(tpl, map[string]string{
		VarOrganizationName: "Acme\r\nBcc: invasor@x.test", VarRole: `<script>alert("x")</script>`,
		VarAcceptURL: "https://app.test/convite?token=a&b=1", VarExpiresAt: "amanhã\x07",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(r.Subject, "\r\n") || r.Subject != "Convite Acme Bcc: invasor@x.test" {
		t.Fatalf("assunto não sanitizado: %q", r.Subject)
	}
	if strings.Contains(r.HTML, "<script>") || strings.Contains(r.HTML, "<b>") {
		t.Fatalf("HTML sem escape: %s", r.HTML)
	}
	if !strings.Contains(r.HTML, `&lt;script&gt;`) || !strings.Contains(r.HTML, `&lt;b&gt;`) {
		t.Fatalf("escape ausente: %s", r.HTML)
	}
	if !strings.Contains(r.HTML, `<a href="https://app.test/convite?token=a&amp;b=1">`) {
		t.Fatalf("link ausente: %s", r.HTML)
	}
	if strings.Contains(r.Text, "\x07") || !strings.Contains(r.Text, "Acesse https://app.test/convite?token=a&b=1") {
		t.Fatalf("texto inesperado: %q", r.Text)
	}
	if strings.Count(r.HTML, "<p>") != 2 || !strings.Contains(r.HTML, "<br>") {
		t.Fatalf("parágrafos inesperados: %s", r.HTML)
	}
}

func TestRenderDoesNotLinkUnsafeSchemes(t *testing.T) {
	tpl := Template{Key: KeyMemberInvitation, Subject: "Convite", Body: "Link: {{accept_url}}"}
	r, err := Render(tpl, map[string]string{VarAcceptURL: "javascript:alert(1)"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.HTML, "<a ") {
		t.Fatalf("não deveria gerar link: %s", r.HTML)
	}
}

func TestRenderRejectsInvalidTemplates(t *testing.T) {
	for _, tpl := range []Template{
		{Key: KeyInvoiceIssued, Subject: "{{accept_url}}", Body: "Corpo"},
		{Key: KeyInvoiceIssued, Subject: "Assunto", Body: "{{nao_existe}}"},
		{Key: KeyInvoiceIssued, Subject: "{{aberto", Body: "Corpo"},
		{Key: KeyInvoiceIssued, Subject: "Assunto", Body: "{{aberto"},
	} {
		if _, err := Render(tpl, nil); err == nil {
			t.Errorf("esperava erro para %+v", tpl)
		}
	}
}

func TestRenderMissingVariableIsEmptyAndSubjectIsTruncated(t *testing.T) {
	tpl := Template{Key: KeyInvoiceIssued, Subject: strings.Repeat("x", 150) + "{{customer_name}}", Body: "Fatura {{invoice_number}}."}
	r, err := Render(tpl, map[string]string{VarCustomerName: strings.Repeat("y", 400)})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "Fatura ." {
		t.Fatalf("variável ausente deveria ficar vazia: %q", r.Text)
	}
	if n := len([]rune(r.Subject)); n != MaxRenderedSubjectLen {
		t.Fatalf("assunto deveria ter %d caracteres, tem %d", MaxRenderedSubjectLen, n)
	}
}

// Os padrões do sistema são válidos e usam somente variáveis permitidas.
func TestDefaultTemplatesAreValidAndComplete(t *testing.T) {
	keys := append(CustomizableKeys(), KeyMemberInvitation)
	for _, k := range keys {
		def, ok := DefaultTemplate(k)
		if !ok || !def.IsDefault || def.Key != k {
			t.Fatalf("padrão ausente para %s", k)
		}
		if k.Customizable() {
			if _, err := ValidateTemplate(k, def.Subject, def.Body); err != nil {
				t.Errorf("padrão inválido para %s: %v", k, err)
			}
		}
		vars := map[string]string{}
		for _, v := range k.Variables() {
			vars[v] = "valor-" + v
		}
		r, err := Render(def, vars)
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if strings.Contains(r.Text, "{{") || strings.Contains(r.Subject, "{{") {
			t.Errorf("%s: variável não substituída: %q", k, r.Text)
		}
	}
	if _, ok := DefaultTemplate("desconhecido"); ok {
		t.Fatal("chave desconhecida não deveria ter padrão")
	}
}

func TestCustomizableKeys(t *testing.T) {
	if len(CustomizableKeys()) != 5 || KeyMemberInvitation.Customizable() {
		t.Fatal("chaves personalizáveis inesperadas")
	}
	if _, err := ParseCustomizableKey("member_invitation"); !apperr.IsKind(err, apperr.KindNotFound) {
		t.Fatalf("convite não é personalizável: %v", err)
	}
	k, err := ParseCustomizableKey("reminder_after_due")
	if err != nil || k != KeyReminderAfterDue {
		t.Fatalf("chave válida rejeitada: %v", err)
	}
	vars := KeyReminderAfterDue.Variables()
	vars[0] = "alterado"
	if KeyReminderAfterDue.Variables()[0] == "alterado" {
		t.Fatal("Variables deve devolver cópia")
	}
}

func TestCapitalizeHandlesEmptyAndInvalid(t *testing.T) {
	if capitalize("") != "" || capitalize("\xff") != "\xff" || capitalize("ábc") != "Ábc" {
		t.Fatal("capitalização inesperada")
	}
}
