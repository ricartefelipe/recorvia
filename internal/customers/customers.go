// Package customers modela os clientes dos tenants (pessoa física ou
// jurídica), seus contatos, endereço de cobrança e identificador externo.
// A validação de CPF/CNPJ é apenas estrutural e não confirma identidade.
package customers

import (
	"regexp"
	"strings"
	"time"

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
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Version          int
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
