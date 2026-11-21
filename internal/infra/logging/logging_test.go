package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestContextAttrsAndRedaction(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "debug", "api")
	ctx := WithAttrs(context.Background(), slog.String("request_id", "r1"), slog.String("tenant_id", "t1"))
	ctx = WithAttrs(ctx, slog.String("request_id", "r2"))
	log.InfoContext(ctx, "teste",
		slog.String("password", "p@ss"),
		slog.String("Authorization", "Bearer x"),
		slog.String("document_number", "12345678901"),
		slog.String("email", "ana@example.com"),
		slog.String("phone", "123"),
		slog.Int("amount", 100),
		slog.Group("g", slog.String("x", "y")),
	)
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	checks := map[string]any{
		"service": "api", "request_id": "r2", "tenant_id": "t1",
		"password": Redacted, "Authorization": Redacted,
		"document_number": "12*******01", "email": "an***********om", "phone": "***",
		"amount": float64(100),
	}
	for k, want := range checks {
		if m[k] != want {
			t.Errorf("%s=%v quero %v", k, m[k], want)
		}
	}
	if AttrValue(ctx, "tenant_id") != "t1" || AttrValue(ctx, "nada") != "" {
		t.Fatal("AttrValue incorreto")
	}
	if len(Attrs(context.Background())) != 0 {
		t.Fatal("contexto vazio não deve ter atributos")
	}
}

func TestWithAttrsAndGroupKeepContext(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info", "worker").With("component", "outbox").WithGroup("detalhes")
	ctx := WithAttrs(context.Background(), slog.String("consumer", "payments"))
	log.DebugContext(ctx, "não deve aparecer")
	log.WarnContext(ctx, "aviso", slog.String("token", "abc"))
	out := buf.String()
	if strings.Contains(out, "não deve aparecer") {
		t.Fatal("nível debug deveria ser filtrado")
	}
	if !strings.Contains(out, `"consumer":"payments"`) || strings.Contains(out, "abc") {
		t.Fatalf("saída inesperada: %s", out)
	}
}

func TestParseLevelAndHelpers(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "WARN": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError, "x": slog.LevelInfo} {
		if ParseLevel(in) != want {
			t.Errorf("ParseLevel(%q)", in)
		}
	}
	if !IsSensitiveKey("webhook_secret") || IsSensitiveKey("amount") {
		t.Fatal("IsSensitiveKey incorreto")
	}
	if Mask("abcd") != "****" || Mask("abcdef") != "ab**ef" {
		t.Fatal("Mask incorreto")
	}
}

// RNF-07: com um span válido no contexto, todo log leva trace_id e span_id
// do span; sem span (ou com contexto de span inválido), os campos não
// aparecem.
func TestLogsCarryTraceAndSpanIDs(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled})
	cases := []struct {
		name      string
		ctx       context.Context
		wantTrace any
		wantSpan  any
	}{
		{"com span", trace.ContextWithSpanContext(context.Background(), sc), "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"},
		{"sem span", context.Background(), nil, nil},
		{"span inválido", trace.ContextWithSpanContext(context.Background(), trace.SpanContext{}), nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			New(&buf, "info", "api").InfoContext(WithAttrs(tc.ctx, slog.String("request_id", "r1")), "teste")
			var m map[string]any
			if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			if m["trace_id"] != tc.wantTrace || m["span_id"] != tc.wantSpan || m["request_id"] != "r1" {
				t.Fatalf("saída %v", m)
			}
		})
	}
}

// O coletor devolve a quem criou o contexto os atributos acrescentados nos
// contextos derivados (o log de acesso HTTP vê o tenant e o usuário que a
// autenticação identificou), sem alterar os atributos do contexto original.
func TestCollectGathersAttrsFromDerivedContexts(t *testing.T) {
	outer := Collect(WithAttrs(context.Background(), slog.String("request_id", "r1")))
	inner := WithAttrs(outer, slog.String("user_id", "u1"))
	inner = WithAttrs(inner, slog.String("tenant_id", "t1"))
	_ = WithAttrs(inner, slog.String("tenant_id", "t2"))
	if AttrValue(outer, "tenant_id") != "" {
		t.Fatal("o contexto original não deve mudar")
	}
	got := Collected(outer)
	for k, want := range map[string]string{"request_id": "r1", "user_id": "u1", "tenant_id": "t2"} {
		if v := AttrValue(got, k); v != want {
			t.Errorf("%s=%q quero %q", k, v, want)
		}
	}
	if len(Attrs(got)) != 3 {
		t.Fatalf("atributos repetidos: %v", Attrs(got))
	}
	plain := WithAttrs(context.Background(), slog.String("request_id", "r2"))
	if Collected(plain) != plain {
		t.Fatal("sem coletor, Collected devolve o próprio contexto")
	}
}

// RNF-12: senhas de URLs contidas em mensagens e erros são mascaradas em
// qualquer chave (a redação por nome de chave não alcança o texto de um
// erro de biblioteca que repete a URL de conexão).
func TestURLCredentialsAreMaskedInAnyValue(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "info", "api")
	log.Error(`conexão a amqp://recorvia:s3gr3d0@rabbit:5672/ falhou`,
		slog.String("error", `parse "redis://:s3gr3d0@redis:port/0": invalid port`),
		slog.Any("cause", errors.New("dial postgres://app:s3gr3d0@db/recorvia: refused")),
		slog.String("url", "https://app.recorvia.test/portal"),
		slog.String("consulta", "https://api.provider.test:443/v1/charges?email=a@b.com"),
		slog.String("arroba", "dial amqp://guest:s3gr3d0@x@rabbit:5672/"),
		slog.String("espaco", `parse "redis://u:s3gr3d0 x@h:1": invalid`),
		slog.String("aspas", `parse "redis://u:s3gr3d0"x@h:1": invalid`),
		slog.Any("stringer", &url.URL{Scheme: "redis", User: url.UserPassword("u", "s3gr3d0"), Host: "h:1"}),
		slog.Any("lista", []string{"amqp://u:s3gr3d0@a:5672/", "sem url"}),
		slog.Int("attempt", 2))
	out := buf.String()
	if strings.Contains(out, "s3gr3d0") {
		t.Fatalf("senha no log: %s", out)
	}
	for _, want := range []string{"amqp://recorvia:xxxxx@rabbit:5672/", `redis://:xxxxx@redis:port/0`, "postgres://app:xxxxx@db/recorvia",
		`"url":"https://app.recorvia.test/portal"`, `"consulta":"https://api.provider.test:443/v1/charges?email=a@b.com"`,
		`"arroba":"dial amqp://guest:xxxxx@rabbit:5672/"`, `"stringer":"redis://u:xxxxx@h:1"`,
		`"lista":["amqp://u:xxxxx@a:5672/","sem url"]`, `"attempt":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("saída sem %q: %s", want, out)
		}
	}
}
