package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
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
