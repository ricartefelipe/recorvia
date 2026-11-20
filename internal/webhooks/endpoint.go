// Package webhooks implementa os webhooks enviados pelo Recorvia aos
// sistemas do tenant: endpoints com eventos assinados, segredos de
// assinatura (cifrados em repouso) com rotação e janela de convivência,
// entregas com histórico, retentativas com backoff exponencial e jitter,
// limite de tentativas e reenvio manual.
//
// A assinatura segue o formato Standard Webhooks: cabeçalhos webhook-id
// (identificador estável do evento), webhook-timestamp (segundos Unix da
// tentativa) e webhook-signature ("v1,<base64>" por segredo válido, separados
// por espaço), com HMAC-SHA256 sobre "id.timestamp.corpo".
//
// A proteção contra SSRF (resolução de DNS, bloqueio de destinos internos,
// validação do IP na conexão, sem redirecionamentos, limites de tempo e de
// resposta) fica no transporte injetado; este pacote valida o formato da URL
// e consulta o validador de destino na criação e na alteração.
package webhooks

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// Status do endpoint.
type Status string

const (
	Active   Status = "active"
	Disabled Status = "disabled"
)

// ParseStatus valida a situação informada na alteração.
func ParseStatus(s string) (Status, error) {
	switch Status(s) {
	case Active, Disabled:
		return Status(s), nil
	}
	return "", apperr.Invalid("invalid_status", "Situação inválida.", apperr.FieldError{Field: "status", Message: "Use active ou disabled."})
}

// SecretInfo descreve um segredo sem expor o valor.
type SecretInfo struct {
	ID        uuid.UUID
	CreatedAt time.Time
	ExpiresAt *time.Time
}

// ValidAt informa se o segredo ainda assina entregas em now.
func (s SecretInfo) ValidAt(now time.Time) bool { return s.ExpiresAt == nil || s.ExpiresAt.After(now) }

// Secret é um segredo de assinatura com o valor em claro (somente em memória).
type Secret struct {
	SecretInfo
	Value string
	// Unavailable indica que o valor não pôde ser decifrado (chave retirada
	// do keyring ou dado corrompido); o segredo não assina entregas.
	Unavailable bool
}

// Endpoint é um destino de webhooks do tenant.
type Endpoint struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	URL         string
	Description string
	EventTypes  []string
	Status      Status
	DeletedAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int
	Secrets     []SecretInfo
}

// Deliverable informa se o endpoint recebe entregas.
func (e Endpoint) Deliverable() bool { return e.Status == Active && e.DeletedAt == nil }

// Subscribes informa se o endpoint assina o tipo de evento.
func (e Endpoint) Subscribes(eventType string) bool { return slices.Contains(e.EventTypes, eventType) }

// Input são os dados de criação e alteração.
type Input struct {
	URL         string
	Description string
	EventTypes  []string
}

// URLRules define as exigências de esquema da URL.
type URLRules struct {
	// RequireHTTPS rejeita http (produção).
	RequireHTTPS bool
}

// Limites do endpoint.
const (
	MaxURLLength         = 2000
	MaxDescriptionLength = 200
)

var errInvalidURL = errors.New("URL inválida")

// ValidateURL verifica o formato da URL: absoluta, http(s) conforme as
// regras, sem credenciais nem fragmento, com host e porta válidos. O limite
// de MaxURLLength caracteres vale para o valor informado e para a forma
// normalizada devolvida (que codifica caracteres não ASCII do caminho).
func ValidateURL(raw string, rules URLRules) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > MaxURLLength || strings.ContainsAny(raw, " \t\r\n") {
		return "", errInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return "", errInvalidURL
	}
	switch u.Scheme {
	case "https":
	case "http":
		if rules.RequireHTTPS {
			return "", errors.New("somente https é permitido")
		}
	default:
		return "", errors.New("esquema não permitido")
	}
	if u.User != nil {
		return "", errors.New("credenciais na URL não são permitidas")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return "", errors.New("fragmento na URL não é permitido")
	}
	host := u.Hostname()
	if host == "" {
		return "", errInvalidURL
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("porta inválida")
		}
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil && !validHostname(host) {
		return "", errors.New("nome de host inválido")
	}
	out := u.String()
	if utf8.RuneCountInString(out) > MaxURLLength {
		return "", errors.New("a URL normalizada excede 2000 caracteres")
	}
	return out, nil
}

func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// ValidateInput valida e normaliza os dados do endpoint (sem consultar DNS).
func ValidateInput(in Input, rules URLRules) (Input, error) {
	var f []apperr.FieldError
	u, err := ValidateURL(in.URL, rules)
	if err != nil {
		f = append(f, apperr.FieldError{Field: "url", Message: "URL inválida: " + err.Error() + "."})
	}
	in.URL = u
	in.Description = strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(in.Description) > MaxDescriptionLength || strings.ContainsRune(in.Description, 0) {
		f = append(f, apperr.FieldError{Field: "description", Message: "A descrição deve ter no máximo 200 caracteres."})
	}
	types := slices.Clone(in.EventTypes)
	slices.Sort(types)
	types = slices.Compact(types)
	if len(types) == 0 {
		f = append(f, apperr.FieldError{Field: "event_types", Message: "Assine ao menos um tipo de evento."})
	}
	for _, t := range types {
		if !Subscribable(t) {
			f = append(f, apperr.FieldError{Field: "event_types", Message: "Tipo de evento desconhecido: " + truncate(t, 80) + "."})
			break
		}
	}
	in.EventTypes = types
	if len(f) > 0 {
		return Input{}, apperr.Invalid("invalid_webhook_endpoint", "Endpoint de webhook inválido.", f...)
	}
	return in, nil
}

// SecretPrefix identifica os segredos de assinatura.
const SecretPrefix = "whsec_"

// NewSecretValue gera um segredo de 256 bits ("whsec_" + base64).
func NewSecretValue() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	return SecretPrefix + base64.StdEncoding.EncodeToString(b)
}

// secretKey extrai a chave HMAC do segredo.
func secretKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, SecretPrefix) {
		return nil, errors.New("webhooks: segredo sem prefixo")
	}
	k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, SecretPrefix))
	if err != nil || len(k) < 24 {
		return nil, errors.New("webhooks: segredo inválido")
	}
	return k, nil
}

var eventTypes = []string{
	outbox.EventChargeCreated, outbox.EventChargeFailed,
	outbox.EventCustomerCreated, outbox.EventCustomerUpdated,
	outbox.EventDisputeClosed, outbox.EventDisputeOpened,
	outbox.EventInvoiceCredited, outbox.EventInvoiceIssued, outbox.EventInvoiceOverdue, outbox.EventInvoicePaid,
	outbox.EventInvoicePartiallyPaid, outbox.EventInvoiceUncollectible, outbox.EventInvoiceVoided,
	outbox.EventPaymentConfirmed,
	outbox.EventRefundFailed, outbox.EventRefundSucceeded,
	outbox.EventSubscriptionCanceled, outbox.EventSubscriptionCreated, outbox.EventSubscriptionReactivate,
	outbox.EventSubscriptionSuspended, outbox.EventSubscriptionUpdated,
}

// EventTypes lista, em ordem alfabética, os eventos que podem ser assinados.
// Eventos internos (webhooks recebidos, convites, reprocessamento) ficam de
// fora.
func EventTypes() []string {
	out := slices.Clone(eventTypes)
	slices.Sort(out)
	return out
}

// Subscribable informa se o tipo de evento pode ser assinado.
func Subscribable(t string) bool { return slices.Contains(eventTypes, t) }

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
