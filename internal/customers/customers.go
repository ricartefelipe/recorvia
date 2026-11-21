// Package customers modela os clientes dos tenants (pessoa física ou
// jurídica), seus contatos, endereço de cobrança e identificador externo.
// A validação de CPF/CNPJ é apenas estrutural e não confirma identidade.
package customers

import (
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// Kind diferencia pessoa física e jurídica.
type Kind string

const (
	Individual Kind = "individual"
	Company    Kind = "company"
)

// Status do cliente.
type Status string

const (
	Active   Status = "active"
	Archived Status = "archived"
)

// Customer é um cliente do tenant.
type Customer struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	Kind             Kind
	Name             string
	LegalName        string
	DocumentType     brdoc.Kind
	DocumentNumber   string
	Email            string
	Phone            string
	ExternalID       string
	Status           Status
	PaymentTermsDays *int
	// AnonymizedAt indica que os dados pessoais foram substituídos de forma
	// irreversível (ver Anonymize).
	AnonymizedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Version      int
}

// AnonymizedName é o nome gravado no lugar do nome do cliente anonimizado.
const AnonymizedName = "Cliente anonimizado"

// anonymizedDocument é o documento neutro gravado no lugar do CPF ou CNPJ: a
// coluna é obrigatória e os zeros não identificam ninguém (não passam na
// validação de dígitos verificadores).
func anonymizedDocument(k brdoc.Kind) string {
	if k == brdoc.CPF {
		return strings.Repeat("0", 11)
	}
	return strings.Repeat("0", 14)
}

// Anonymized informa se o cliente foi anonimizado.
func (c Customer) Anonymized() bool { return c.AnonymizedAt != nil }

// Anonymize substitui os dados pessoais do cadastro por valores neutros,
// arquiva o cliente e registra o instante. O tipo (pessoa física ou jurídica)
// e o prazo de pagamento, que não identificam ninguém, são mantidos. Devolve
// os nomes dos campos que tinham valor (para o histórico, sem os valores).
func (c *Customer) Anonymize(now time.Time) []string {
	fields := []string{"name", "document_number"}
	clear := func(name string, v *string) {
		if *v != "" {
			fields = append(fields, name)
			*v = ""
		}
	}
	c.Name = AnonymizedName
	c.DocumentNumber = anonymizedDocument(c.DocumentType)
	clear("legal_name", &c.LegalName)
	clear("email", &c.Email)
	clear("phone", &c.Phone)
	clear("external_id", &c.ExternalID)
	c.Status = Archived
	c.AnonymizedAt = &now
	return fields
}

// Limites da justificativa da anonimização.
const (
	MinAnonymizationReason = 10
	MaxAnonymizationReason = 500
)

var nonDigits = regexp.MustCompile(`[^0-9A-Za-z]`)

// ValidateAnonymizationReason normaliza a justificativa e recusa texto que
// contenha dados pessoais do próprio cliente: a justificativa fica na trilha
// de auditoria, que é imutável, e deve trazer apenas a referência do pedido
// (protocolo, chamado) e o motivo.
func ValidateAnonymizationReason(reason string, c Customer) (string, error) {
	reason = strings.Join(strings.Fields(reason), " ")
	invalid := func(msg string) error {
		return apperr.Invalid("invalid_anonymization", "Justificativa da anonimização inválida.", apperr.FieldError{Field: "reason", Message: msg})
	}
	if n := len([]rune(reason)); n < MinAnonymizationReason || n > MaxAnonymizationReason {
		return "", invalid("Informe a justificativa com 10 a 500 caracteres.")
	}
	// Caracteres de controle (inclusive NUL, que o JSONB da auditoria não
	// aceita) e texto que não é UTF-8 válido são recusados.
	if !utf8.ValidString(reason) || strings.ContainsFunc(reason, unicode.IsControl) {
		return "", invalid("A justificativa contém caracteres inválidos.")
	}
	lower := strings.ToLower(reason)
	compact := strings.ToLower(nonDigits.ReplaceAllString(reason, ""))
	leaks := (len([]rune(c.Name)) >= 5 && strings.Contains(lower, strings.ToLower(c.Name))) ||
		(len([]rune(c.LegalName)) >= 5 && strings.Contains(lower, strings.ToLower(c.LegalName))) ||
		(c.Email != "" && strings.Contains(lower, strings.ToLower(c.Email))) ||
		(c.DocumentNumber != "" && strings.Contains(compact, strings.ToLower(c.DocumentNumber))) ||
		(len(c.Phone) >= 8 && strings.Contains(compact, strings.TrimPrefix(c.Phone, "+")))
	if leaks {
		return "", invalid("A justificativa não pode conter dados pessoais do cliente (nome, documento, e-mail ou telefone): informe o protocolo do pedido e o motivo.")
	}
	return reason, nil
}

// Contact é um contato do cliente.
type Contact struct {
	ID                   uuid.UUID
	CustomerID           uuid.UUID
	Name                 string
	Email                string
	Phone                string
	Role                 string
	ReceivesBillingEmail bool
	CreatedAt            time.Time
}

// Address é o endereço de cobrança.
type Address struct {
	Street     string `json:"street"`
	Number     string `json:"number"`
	Complement string `json:"complement,omitempty"`
	District   string `json:"district"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// Input são os dados de criação/alteração.
type Input struct {
	Kind             Kind
	Name             string
	LegalName        string
	DocumentNumber   string
	Email            string
	Phone            string
	ExternalID       string
	PaymentTermsDays *int
}

var (
	phoneRe      = regexp.MustCompile(`^\+?[0-9]{8,15}$`)
	externalIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	stateRe      = regexp.MustCompile(`^[A-Z]{2}$`)
	postalRe     = regexp.MustCompile(`^[0-9]{8}$`)
)

func normalizePhone(p string) string {
	r := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "")
	return r.Replace(strings.TrimSpace(p))
}

func validate(in Input) (Input, brdoc.Kind, []apperr.FieldError) {
	var f []apperr.FieldError
	add := func(field, msg string) { f = append(f, apperr.FieldError{Field: field, Message: msg}) }
	var docKind brdoc.Kind
	switch in.Kind {
	case Individual:
		docKind = brdoc.CPF
	case Company:
		docKind = brdoc.CNPJ
	default:
		add("kind", "Informe pessoa física (individual) ou jurídica (company).")
	}
	in.Name = strings.TrimSpace(in.Name)
	if n := len([]rune(in.Name)); n < 2 || n > 200 {
		add("name", "Informe um nome entre 2 e 200 caracteres.")
	}
	in.LegalName = strings.TrimSpace(in.LegalName)
	if len([]rune(in.LegalName)) > 200 {
		add("legal_name", "Razão social deve ter no máximo 200 caracteres.")
	}
	if docKind != "" {
		doc, err := brdoc.Validate(docKind, in.DocumentNumber)
		if err != nil {
			if docKind == brdoc.CPF {
				add("document_number", "CPF com formato ou dígitos verificadores inválidos.")
			} else {
				add("document_number", "CNPJ com formato ou dígitos verificadores inválidos.")
			}
		}
		in.DocumentNumber = doc
	}
	if in.Email != "" {
		e, err := tenancy.NormalizeEmail(in.Email)
		if err != nil {
			add("email", "Endereço de e-mail inválido.")
		}
		in.Email = e
	}
	if in.Phone != "" {
		in.Phone = normalizePhone(in.Phone)
		if !phoneRe.MatchString(in.Phone) {
			add("phone", "Telefone deve ter de 8 a 15 dígitos, com DDI opcional.")
		}
	}
	in.ExternalID = strings.TrimSpace(in.ExternalID)
	if in.ExternalID != "" && !externalIDRe.MatchString(in.ExternalID) {
		add("external_id", "Use até 128 letras, números ou os símbolos _ . : -")
	}
	if in.PaymentTermsDays != nil && (*in.PaymentTermsDays < 0 || *in.PaymentTermsDays > 120) {
		add("payment_terms_days", "O prazo deve ficar entre 0 e 120 dias.")
	}
	return in, docKind, f
}

// New valida e cria um cliente.
func New(id, tenantID uuid.UUID, in Input, now time.Time) (Customer, error) {
	in, docKind, f := validate(in)
	if len(f) > 0 {
		return Customer{}, apperr.Invalid("invalid_customer", "Dados do cliente inválidos.", f...)
	}
	return Customer{
		ID: id, TenantID: tenantID, Kind: in.Kind, Name: in.Name, LegalName: in.LegalName,
		DocumentType: docKind, DocumentNumber: in.DocumentNumber, Email: in.Email, Phone: in.Phone,
		ExternalID: in.ExternalID, Status: Active, PaymentTermsDays: in.PaymentTermsDays,
		CreatedAt: now, UpdatedAt: now, Version: 1,
	}, nil
}

// Apply altera os dados e retorna os nomes dos campos modificados (para o
// histórico, sem valores pessoais).
func (c *Customer) Apply(in Input) ([]string, error) {
	in, docKind, f := validate(in)
	if len(f) > 0 {
		return nil, apperr.Invalid("invalid_customer", "Dados do cliente inválidos.", f...)
	}
	var changed []string
	set := func(name string, dst *string, v string) {
		if *dst != v {
			*dst = v
			changed = append(changed, name)
		}
	}
	if c.Kind != in.Kind {
		c.Kind, c.DocumentType = in.Kind, docKind
		changed = append(changed, "kind")
	}
	set("name", &c.Name, in.Name)
	set("legal_name", &c.LegalName, in.LegalName)
	set("document_number", &c.DocumentNumber, in.DocumentNumber)
	set("email", &c.Email, in.Email)
	set("phone", &c.Phone, in.Phone)
	set("external_id", &c.ExternalID, in.ExternalID)
	if !equalIntPtr(c.PaymentTermsDays, in.PaymentTermsDays) {
		c.PaymentTermsDays = in.PaymentTermsDays
		changed = append(changed, "payment_terms_days")
	}
	return changed, nil
}

func equalIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ContactInput são os dados de um contato.
type ContactInput struct {
	Name                 string
	Email                string
	Phone                string
	Role                 string
	ReceivesBillingEmail bool
}

// NewContact valida e cria um contato.
func NewContact(id, customerID uuid.UUID, in ContactInput, now time.Time) (Contact, error) {
	var f []apperr.FieldError
	in.Name = strings.TrimSpace(in.Name)
	if n := len([]rune(in.Name)); n < 1 || n > 200 {
		f = append(f, apperr.FieldError{Field: "name", Message: "Informe o nome do contato."})
	}
	email, err := tenancy.NormalizeEmail(in.Email)
	if err != nil {
		f = append(f, apperr.FieldError{Field: "email", Message: "Endereço de e-mail inválido."})
	}
	if in.Phone != "" {
		in.Phone = normalizePhone(in.Phone)
		if !phoneRe.MatchString(in.Phone) {
			f = append(f, apperr.FieldError{Field: "phone", Message: "Telefone deve ter de 8 a 15 dígitos, com DDI opcional."})
		}
	}
	in.Role = strings.TrimSpace(in.Role)
	if len([]rune(in.Role)) > 100 {
		f = append(f, apperr.FieldError{Field: "role", Message: "Cargo deve ter no máximo 100 caracteres."})
	}
	if len(f) > 0 {
		return Contact{}, apperr.Invalid("invalid_contact", "Dados do contato inválidos.", f...)
	}
	return Contact{ID: id, CustomerID: customerID, Name: in.Name, Email: email, Phone: in.Phone, Role: in.Role,
		ReceivesBillingEmail: in.ReceivesBillingEmail, CreatedAt: now}, nil
}

// ContactPatch é a alteração parcial de um contato: campo nil mantém o valor
// atual (por exemplo, a preferência de e-mail de cobrança que o cliente
// alterou pelo portal não volta ao padrão quando omitida).
type ContactPatch struct {
	Name                 *string
	Email                *string
	Phone                *string
	Role                 *string
	ReceivesBillingEmail *bool
}

// Merge devolve os dados do contato com a alteração aplicada.
func (p ContactPatch) Merge(c Contact) ContactInput {
	in := ContactInput{Name: c.Name, Email: c.Email, Phone: c.Phone, Role: c.Role, ReceivesBillingEmail: c.ReceivesBillingEmail}
	for _, f := range []struct {
		dst *string
		v   *string
	}{{&in.Name, p.Name}, {&in.Email, p.Email}, {&in.Phone, p.Phone}, {&in.Role, p.Role}} {
		if f.v != nil {
			*f.dst = *f.v
		}
	}
	if p.ReceivesBillingEmail != nil {
		in.ReceivesBillingEmail = *p.ReceivesBillingEmail
	}
	return in
}

// Apply valida os novos dados do contato, aplica-os e retorna os nomes dos
// campos modificados (para o histórico, sem valores pessoais).
func (c *Contact) Apply(in ContactInput) ([]string, error) {
	n, err := NewContact(c.ID, c.CustomerID, in, c.CreatedAt)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, f := range []struct {
		name     string
		old, new string
	}{{"name", c.Name, n.Name}, {"email", c.Email, n.Email}, {"phone", c.Phone, n.Phone}, {"role", c.Role, n.Role}} {
		if f.old != f.new {
			changed = append(changed, f.name)
		}
	}
	if c.ReceivesBillingEmail != n.ReceivesBillingEmail {
		changed = append(changed, "receives_billing_email")
	}
	*c = n
	return changed, nil
}

// ChangedFields devolve os nomes dos campos do endereço que diferem de prev
// (todos os preenchidos, quando não havia endereço). Somente nomes, nunca
// valores (ADR-061).
func (a Address) ChangedFields(prev *Address) []string {
	var old Address
	if prev != nil {
		old = *prev
	}
	var changed []string
	for _, f := range []struct {
		name     string
		old, new string
	}{{"street", old.Street, a.Street}, {"number", old.Number, a.Number}, {"complement", old.Complement, a.Complement},
		{"district", old.District, a.District}, {"city", old.City, a.City}, {"state", old.State, a.State},
		{"postal_code", old.PostalCode, a.PostalCode}, {"country", old.Country, a.Country}} {
		if f.old != f.new {
			changed = append(changed, f.name)
		}
	}
	return changed
}

// ValidateAddress normaliza e valida o endereço de cobrança.
func ValidateAddress(a Address) (Address, error) {
	var f []apperr.FieldError
	trim := func(s string) string { return strings.TrimSpace(s) }
	a.Street, a.Number, a.Complement = trim(a.Street), trim(a.Number), trim(a.Complement)
	a.District, a.City = trim(a.District), trim(a.City)
	a.State = strings.ToUpper(trim(a.State))
	a.PostalCode = strings.NewReplacer("-", "", ".", "", " ", "").Replace(a.PostalCode)
	a.Country = strings.ToUpper(trim(a.Country))
	if a.Country == "" {
		a.Country = "BR"
	}
	req := func(field, v string, maxLen int) {
		if n := len([]rune(v)); n < 1 || n > maxLen {
			f = append(f, apperr.FieldError{Field: field, Message: "Campo obrigatório."})
		}
	}
	req("street", a.Street, 200)
	req("number", a.Number, 20)
	req("district", a.District, 100)
	req("city", a.City, 100)
	if len([]rune(a.Complement)) > 100 {
		f = append(f, apperr.FieldError{Field: "complement", Message: "Complemento deve ter no máximo 100 caracteres."})
	}
	if !stateRe.MatchString(a.State) {
		f = append(f, apperr.FieldError{Field: "state", Message: "Informe a UF com duas letras."})
	}
	if !postalRe.MatchString(a.PostalCode) {
		f = append(f, apperr.FieldError{Field: "postal_code", Message: "CEP deve ter 8 dígitos."})
	}
	if !stateRe.MatchString(a.Country) {
		f = append(f, apperr.FieldError{Field: "country", Message: "País deve ter duas letras (ISO 3166-1)."})
	}
	if len(f) > 0 {
		return Address{}, apperr.Invalid("invalid_address", "Endereço de cobrança inválido.", f...)
	}
	return a, nil
}
