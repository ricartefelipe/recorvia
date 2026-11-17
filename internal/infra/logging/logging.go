// Package logging configura log/slog com saída JSON, correlação por contexto
// e redação de informações sensíveis.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

type ctxKey int

const attrsKey ctxKey = iota

// WithAttrs anexa atributos de correlação ao contexto (request_id, tenant_id,
// trace_id, consumer, job). Eles são incluídos em todos os logs emitidos com
// esse contexto.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(attrsKey).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(prev)+len(attrs))
	merged = append(merged, prev...)
	for _, a := range attrs {
		replaced := false
		for i := range merged {
			if merged[i].Key == a.Key {
				merged[i] = a
				replaced = true
			}
		}
		if !replaced {
			merged = append(merged, a)
		}
	}
	return context.WithValue(ctx, attrsKey, merged)
}

// Attrs retorna os atributos de correlação do contexto.
func Attrs(ctx context.Context) []slog.Attr {
	a, _ := ctx.Value(attrsKey).([]slog.Attr)
	return a
}

// AttrValue retorna o valor textual de um atributo de correlação.
func AttrValue(ctx context.Context, key string) string {
	for _, a := range Attrs(ctx) {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Attrs(ctx); len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// sensitiveKeys são redigidas integralmente.
var sensitiveKeys = []string{
	"password", "senha", "secret", "token", "authorization", "cookie",
	"api_key", "apikey", "access_token", "refresh_token", "id_token",
	"card", "cvv", "pan", "private_key", "credential",
}

// maskedKeys têm o conteúdo parcialmente mascarado.
var maskedKeys = []string{"document", "cpf", "cnpj", "email", "phone"}

// Redacted é o marcador usado no lugar de valores sensíveis.
const Redacted = "[REDACTED]"

// IsSensitiveKey informa se uma chave deve ser redigida integralmente.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func shouldMask(key string) bool {
	k := strings.ToLower(key)
	for _, s := range maskedKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Mask preserva no máximo os dois primeiros e os dois últimos caracteres.
func Mask(v string) string {
	r := []rune(v)
	if len(r) <= 4 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:2]) + strings.Repeat("*", len(r)-4) + string(r[len(r)-2:])
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	switch {
	case IsSensitiveKey(a.Key):
		return slog.String(a.Key, Redacted)
	case shouldMask(a.Key) && a.Value.Kind() == slog.KindString:
		return slog.String(a.Key, Mask(a.Value.String()))
	}
	return a
}

// ParseLevel converte texto em nível, com info como padrão.
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// New cria um logger JSON com correlação e redação.
func New(w io.Writer, level string, service string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       ParseLevel(level),
		ReplaceAttr: redact,
	})
	return slog.New(contextHandler{h}).With(slog.String("service", service))
}
