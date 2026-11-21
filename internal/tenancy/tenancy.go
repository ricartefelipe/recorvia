// Package tenancy modela as organizações contratantes (tenants), a
// associação de usuários com papéis e os convites. Um usuário pode
// participar de várias organizações com papéis diferentes; o acesso a uma
// organização é sempre validado contra a associação ativa.
package tenancy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
)

// TenantStatus é a situação da organização.
type TenantStatus string

const (
	TenantActive    TenantStatus = "active"
	TenantSuspended TenantStatus = "suspended"
	TenantClosed    TenantStatus = "closed"
)

// Tenant é uma organização contratante.
type Tenant struct {
	ID               uuid.UUID
	Slug             string
	Name             string
	LegalName        string
	DocumentNumber   string
	Timezone         string
	Currency         string
	InvoicePrefix    string
	PaymentTermsDays int
	// Address é o endereço da organização (nil enquanto não cadastrado),
	// copiado para o emitente de cada fatura emitida.
	Address   *address.Address
	Status    TenantStatus
	CreatedAt time.Time
	UpdatedAt time.Time
	Version   int
}

// Location retorna o fuso IANA da organização.
func (t Tenant) Location() (*time.Location, error) { return time.LoadLocation(t.Timezone) }

// TenantInput são os dados de cadastro/alteração da organização.
type TenantInput struct {
	Name             string
	Slug             string
	LegalName        string
	DocumentNumber   string
	Timezone         string
	InvoicePrefix    string
	PaymentTermsDays *int
	// Address, quando informado, substitui o endereço da organização; nil
	// mantém o atual. O endereço não é removido, como o de cobrança do
	// cliente.
	Address *address.Address
}

var (
	slugRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	prefixRe = regexp.MustCompile(`^[A-Z0-9]{1,10}$`)
)

func validateTenantInput(in TenantInput, requireSlug bool) (TenantInput, error) {
	var fields []apperr.FieldError
	in.Name = strings.TrimSpace(in.Name)
	if n := len([]rune(in.Name)); n < 2 || n > 200 {
		fields = append(fields, apperr.FieldError{Field: "name", Message: "Informe um nome entre 2 e 200 caracteres."})
	}
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if requireSlug && !slugRe.MatchString(in.Slug) {
		fields = append(fields, apperr.FieldError{Field: "slug", Message: "Use de 2 a 63 letras minúsculas, números ou hífens."})
	}
	in.LegalName = strings.TrimSpace(in.LegalName)
	if len([]rune(in.LegalName)) > 200 {
		fields = append(fields, apperr.FieldError{Field: "legal_name", Message: "Razão social deve ter no máximo 200 caracteres."})
	}
	if in.DocumentNumber != "" {
		doc, err := brdoc.Validate(brdoc.CNPJ, in.DocumentNumber)
		if err != nil {
			fields = append(fields, apperr.FieldError{Field: "document_number", Message: "CNPJ com formato ou dígitos verificadores inválidos."})
		}
		in.DocumentNumber = doc
	}
	if in.Timezone == "" {
		in.Timezone = "America/Sao_Paulo"
	}
	if _, err := time.LoadLocation(in.Timezone); err != nil || !strings.Contains(in.Timezone, "/") {
		fields = append(fields, apperr.FieldError{Field: "timezone", Message: "Informe um fuso horário IANA, por exemplo America/Sao_Paulo."})
	}
	in.InvoicePrefix = strings.ToUpper(strings.TrimSpace(in.InvoicePrefix))
	if in.InvoicePrefix == "" {
		in.InvoicePrefix = "FAT"
	}
	if !prefixRe.MatchString(in.InvoicePrefix) {
		fields = append(fields, apperr.FieldError{Field: "invoice_prefix", Message: "Use de 1 a 10 letras maiúsculas ou números."})
	}
	if in.PaymentTermsDays != nil && (*in.PaymentTermsDays < 0 || *in.PaymentTermsDays > 120) {
		fields = append(fields, apperr.FieldError{Field: "payment_terms_days", Message: "O prazo deve ficar entre 0 e 120 dias."})
	}
	if in.Address != nil {
		a, invalid := address.Normalize(*in.Address)
		for _, f := range invalid {
			fields = append(fields, apperr.FieldError{Field: "address." + f.Field, Message: f.Message})
		}
		in.Address = &a
	}
	if len(fields) > 0 {
		return in, apperr.Invalid("invalid_organization", "Dados da organização inválidos.", fields...)
	}
	return in, nil
}

// NewTenant valida e cria uma organização.
func NewTenant(id uuid.UUID, in TenantInput, now time.Time) (Tenant, error) {
	in, err := validateTenantInput(in, true)
	if err != nil {
		return Tenant{}, err
	}
	terms := 5
	if in.PaymentTermsDays != nil {
		terms = *in.PaymentTermsDays
	}
	return Tenant{
		ID: id, Slug: in.Slug, Name: in.Name, LegalName: in.LegalName, DocumentNumber: in.DocumentNumber,
		Timezone: in.Timezone, Currency: "BRL", InvoicePrefix: in.InvoicePrefix, PaymentTermsDays: terms,
		Status: TenantActive, CreatedAt: now, UpdatedAt: now, Version: 1,
	}, nil
}

// ApplyUpdate altera os dados editáveis (o slug é permanente).
func (t *Tenant) ApplyUpdate(in TenantInput) error {
	in, err := validateTenantInput(in, false)
	if err != nil {
		return err
	}
	t.Name, t.LegalName, t.DocumentNumber = in.Name, in.LegalName, in.DocumentNumber
	t.Timezone, t.InvoicePrefix = in.Timezone, in.InvoicePrefix
	if in.PaymentTermsDays != nil {
		t.PaymentTermsDays = *in.PaymentTermsDays
	}
	if in.Address != nil {
		t.Address = in.Address
	}
	return nil
}

// MembershipStatus é a situação da associação.
type MembershipStatus string

const (
	MembershipActive  MembershipStatus = "active"
	MembershipRemoved MembershipStatus = "removed"
)

// Membership associa um usuário a uma organização.
type Membership struct {
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Role      Role
	Status    MembershipStatus
	InvitedBy *uuid.UUID
	CreatedAt time.Time
	Version   int
}

// Member é uma associação com dados do usuário.
type Member struct {
	Membership
	Email       string
	DisplayName string
}

// UserMembership lista uma organização acessível ao usuário.
type UserMembership struct {
	TenantID     uuid.UUID
	TenantName   string
	TenantSlug   string
	Role         Role
	TenantStatus TenantStatus
}

// InvitationTTL é a validade padrão de um convite.
const InvitationTTL = 7 * 24 * time.Hour

// InvitationState é a situação derivada de um convite.
type InvitationState string

const (
	InvitationPending  InvitationState = "pending"
	InvitationAccepted InvitationState = "accepted"
	InvitationRevoked  InvitationState = "revoked"
	InvitationExpired  InvitationState = "expired"
)

// Invitation é um convite de uso único com expiração.
type Invitation struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Email      string
	Role       Role
	TokenHash  []byte
	InvitedBy  uuid.UUID
	ExpiresAt  time.Time
	AcceptedAt *time.Time
	AcceptedBy *uuid.UUID
	RevokedAt  *time.Time
	RevokedBy  *uuid.UUID
	CreatedAt  time.Time
}

// State calcula a situação no instante now.
func (i Invitation) State(now time.Time) InvitationState {
	switch {
	case i.AcceptedAt != nil:
		return InvitationAccepted
	case i.RevokedAt != nil:
		return InvitationRevoked
	case !now.Before(i.ExpiresAt):
		return InvitationExpired
	}
	return InvitationPending
}

// NewToken gera um token aleatório de 256 bits (base64url) e seu hash SHA-256.
// Somente o hash é persistido.
func NewToken() (token string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

// HashToken calcula o hash de um token recebido.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// AcceptURL monta o endereço de aceite de um convite a partir da URL pública
// da interface (vazia: caminho relativo).
func AcceptURL(publicURL, token string) string {
	return strings.TrimRight(publicURL, "/") + "/convite?token=" + url.QueryEscape(token)
}

// InvitationTokenAAD vincula o token cifrado no evento member.invited ao
// tenant e ao convite: o ciphertext não pode ser reaproveitado em outro
// registro.
func InvitationTokenAAD(tenantID, invitationID uuid.UUID) []byte {
	return []byte(tenantID.String() + "/invitation/" + invitationID.String() + "/token")
}

// NormalizeEmail valida e normaliza um endereço de e-mail.
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || len(s) > 320 || !strings.Contains(s[strings.LastIndex(s, "@"):], ".") {
		return "", apperr.Invalid("invalid_email", "Endereço de e-mail inválido.", apperr.FieldError{Field: "email", Message: "Endereço de e-mail inválido."})
	}
	return strings.ToLower(s), nil
}
