// Package logging configura log/slog com saída JSON, correlação por contexto
// e redação de informações sensíveis: por nome de chave (senhas, tokens,
// documentos) e, em qualquer chave, das senhas de URLs contidas no texto.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/trace"

	"github.com/ricartefelipe/recorvia/internal/infra/redact"
)

type ctxKey int

const (
	attrsKey ctxKey = iota
	collectorKey
)

// WithAttrs anexa atributos de correlação ao contexto (request_id, tenant_id,
// user_id, consumer, job). Eles são incluídos em todos os logs emitidos com
// esse contexto; trace_id e span_id vêm do span ativo (ver New). Se o
// contexto tiver um coletor (Collect), os atributos também ficam registrados
// nele.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	prev, _ := ctx.Value(attrsKey).([]slog.Attr)
	if c, _ := ctx.Value(collectorKey).(*collector); c != nil {
		c.add(attrs)
	}
	return context.WithValue(ctx, attrsKey, merge(prev, attrs))
}

// merge devolve prev acrescido de attrs; a mesma chave fica com o valor mais
// recente.
func merge(prev, attrs []slog.Attr) []slog.Attr {
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
	return merged
}

// collector guarda os atributos de correlação acrescentados nos contextos
// derivados, que o contexto original não enxerga.
type collector struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

func (c *collector) add(attrs []slog.Attr) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attrs = merge(c.attrs, attrs)
}

func (c *collector) snapshot() []slog.Attr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]slog.Attr(nil), c.attrs...)
}

// Collect devolve um contexto que registra os atributos de correlação
// acrescentados com WithAttrs em qualquer contexto derivado dele. Serve aos
// logs emitidos por quem iniciou o contexto depois que as etapas internas o
// enriqueceram (por exemplo, o log de acesso HTTP com o tenant e o usuário
// identificados pela autenticação).
func Collect(ctx context.Context) context.Context {
	return context.WithValue(ctx, collectorKey, &collector{})
}

// Collected devolve ctx com os atributos registrados pelo coletor de ctx
// (Collect) somados aos seus. Sem coletor, devolve ctx.
func Collected(ctx context.Context) context.Context {
	c, _ := ctx.Value(collectorKey).(*collector)
	if c == nil {
		return ctx
	}
	prev, _ := ctx.Value(attrsKey).([]slog.Attr)
	return context.WithValue(ctx, attrsKey, merge(prev, c.snapshot()))
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

// Handle acrescenta ao registro os atributos de correlação do contexto e, com
// um span válido no contexto, trace_id e span_id (o log leva ao rastro).
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs := Attrs(ctx); len(attrs) > 0 {
		r.AddAttrs(attrs...)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
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

// correlationKeys são os identificadores de correlação: nunca são redigidos,
// mesmo contendo um termo sensível ("pan" em span_id).
var correlationKeys = map[string]bool{"trace_id": true, "span_id": true}

// IsSensitiveKey informa se uma chave deve ser redigida integralmente.
func IsSensitiveKey(key string) bool {
	k := strings.ToLower(key)
	if correlationKeys[k] {
		return false
	}
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

func replaceAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	switch {
	case IsSensitiveKey(a.Key):
		return slog.String(a.Key, Redacted)
	case shouldMask(a.Key) && a.Value.Kind() == slog.KindString:
		return slog.String(a.Key, Mask(a.Value.String()))
	}
	return withoutURLCredentials(a)
}

// withoutURLCredentials mascara senhas de URLs contidas em textos, erros,
// valores com String() e listas de textos (por exemplo, a mensagem de uma
// biblioteca que repete a URL de conexão), qualquer que seja a chave.
func withoutURLCredentials(a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		if s := a.Value.String(); strings.Contains(s, "://") {
			return slog.String(a.Key, redact.String(s))
		}
	case slog.KindAny:
		switch v := a.Value.Any().(type) {
		case error:
			if v != nil && strings.Contains(v.Error(), "://") {
				return slog.String(a.Key, redact.String(v.Error()))
			}
		case fmt.Stringer:
			if s := v.String(); strings.Contains(s, "://") {
				return slog.String(a.Key, redact.String(s))
			}
		case []string:
			out := make([]string, len(v))
			changed := false
			for i, s := range v {
				out[i] = s
				if strings.Contains(s, "://") {
					out[i], changed = redact.String(s), true
				}
			}
			if changed {
				return slog.Any(a.Key, out)
			}
		}
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
		ReplaceAttr: replaceAttr,
	})
	return slog.New(contextHandler{h}).With(slog.String("service", service))
}
