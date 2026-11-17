// Package apperr define erros de aplicação com categoria estável, código
// legível por máquina e mensagem segura para o usuário. Adaptadores (HTTP,
// mensageria) traduzem a categoria sem expor detalhes internos; a causa
// original fica encadeada para logs.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifica o erro para tradução nas bordas do sistema.
type Kind int

const (
	// KindInternal é o padrão para erros não classificados.
	KindInternal Kind = iota
	// KindInvalid indica entrada inválida.
	KindInvalid
	// KindNotFound indica recurso inexistente ou fora do escopo do tenant.
	KindNotFound
	// KindConflict indica violação de estado ou unicidade.
	KindConflict
	// KindForbidden indica falta de permissão.
	KindForbidden
	// KindUnauthenticated indica ausência de identidade válida.
	KindUnauthenticated
	// KindPreconditionFailed indica versão desatualizada (concorrência otimista).
	KindPreconditionFailed
	// KindUnavailable indica dependência indisponível; a operação pode ser repetida.
	KindUnavailable
	// KindRateLimited indica excesso de requisições.
	KindRateLimited
	// KindLimitExceeded indica limite do plano da plataforma atingido.
	KindLimitExceeded
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindForbidden:
		return "forbidden"
	case KindUnauthenticated:
		return "unauthenticated"
	case KindPreconditionFailed:
		return "precondition_failed"
	case KindUnavailable:
		return "unavailable"
	case KindRateLimited:
		return "rate_limited"
	case KindLimitExceeded:
		return "limit_exceeded"
	}
	return "internal"
}

// FieldError descreve um problema de validação em um campo.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Error é um erro de aplicação.
type Error struct {
	Kind    Kind
	Code    string
	Message string
	Fields  []FieldError
	Err     error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return e.Code + ": " + e.Message
}

// Unwrap expõe a causa.
func (e *Error) Unwrap() error { return e.Err }

// Is permite comparar por categoria e código com errors.Is.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return t.Kind == e.Kind && (t.Code == "" || t.Code == e.Code)
}

// Wrap encadeia uma causa preservando categoria e código.
func (e *Error) Wrap(err error) *Error {
	c := *e
	c.Err = err
	return &c
}

func newErr(k Kind, code, msg string) *Error { return &Error{Kind: k, Code: code, Message: msg} }

// Invalid cria erro de validação.
func Invalid(code, msg string, fields ...FieldError) *Error {
	e := newErr(KindInvalid, code, msg)
	e.Fields = fields
	return e
}

// NotFound cria erro de recurso inexistente.
func NotFound(code, msg string) *Error { return newErr(KindNotFound, code, msg) }

// Conflict cria erro de conflito de estado.
func Conflict(code, msg string) *Error { return newErr(KindConflict, code, msg) }

// Forbidden cria erro de autorização.
func Forbidden(code, msg string) *Error { return newErr(KindForbidden, code, msg) }

// Unauthenticated cria erro de autenticação.
func Unauthenticated(code, msg string) *Error { return newErr(KindUnauthenticated, code, msg) }

// PreconditionFailed cria erro de versão desatualizada.
func PreconditionFailed(code, msg string) *Error { return newErr(KindPreconditionFailed, code, msg) }

// Unavailable cria erro de dependência indisponível.
func Unavailable(code, msg string) *Error { return newErr(KindUnavailable, code, msg) }

// RateLimited cria erro de limite de requisições.
func RateLimited(code, msg string) *Error { return newErr(KindRateLimited, code, msg) }

// LimitExceeded cria erro de limite do plano contratado.
func LimitExceeded(code, msg string) *Error { return newErr(KindLimitExceeded, code, msg) }

// KindOf retorna a categoria do erro; erros desconhecidos são internos.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// As extrai o *Error da cadeia.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsKind informa se err pertence à categoria k.
func IsKind(err error, k Kind) bool { return KindOf(err) == k }
