package notifications

import (
	"strings"
	"testing"
)

// FuzzTemplateRender: qualquer template aceito na validação renderiza sem
// erro; o assunto nunca tem quebra de linha e nenhum valor com marcação
// aparece sem escape no HTML.
func FuzzTemplateRender(f *testing.F) {
	f.Add("Fatura {{invoice_number}}", "Olá {{customer_name}}, saldo {{amount_due}}.", "<script>alert(1)</script>")
	f.Add("{{customer_name}}", "{{ customer_name }}\n\n{{due_date}}", "a\r\nBcc: x@y.test")
	f.Add("Assunto }}", "{{", "\x00\x07")
	f.Fuzz(func(t *testing.T, subject, body, value string) {
		tpl, err := ValidateTemplate(KeyInvoiceIssued, subject, body)
		if err != nil {
			return
		}
		r, err := Render(tpl, map[string]string{VarCustomerName: value, VarInvoiceNumber: value, VarAmountDue: "R$ 1,00"})
		if err != nil {
			t.Fatalf("template validado não renderizou: %v", err)
		}
		if strings.ContainsAny(r.Subject, "\r\n") {
			t.Fatalf("assunto com quebra de linha: %q", r.Subject)
		}
		// Sem variáveis de URL neste template, a única marcação é a do
		// documento e dos parágrafos; todo o resto precisa estar escapado.
		inner := r.HTML[strings.Index(r.HTML, "<body"):]
		inner = inner[strings.Index(inner, ">")+1:]
		inner = strings.TrimSuffix(inner, "</body></html>")
		inner = strings.NewReplacer("<p>", "", "</p>", "", "<br>", "").Replace(inner)
		if strings.ContainsAny(inner, "<>") {
			t.Fatalf("marcação sem escape no HTML: %q", r.HTML)
		}
	})
}
