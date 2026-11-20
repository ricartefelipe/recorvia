package webhooks

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

var t0 = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		raw   string
		https bool
		ok    bool
	}{
		{"https://hooks.cliente.test/recorvia", true, true},
		{"https://hooks.cliente.test:8443/a?b=c", true, true},
		{"http://hooks.cliente.test/x", false, true},
		{"http://hooks.cliente.test/x", true, false},
		{"https://[2001:db8::1]/x", true, true},
		{"ftp://hooks.cliente.test/x", false, false},
		{"https://user:pass@hooks.cliente.test/x", false, false},
		{"https://hooks.cliente.test/x#frag", false, false},
		{"https://hooks.cliente.test:0/x", false, false},
		{"https://hooks.cliente.test:99999/x", false, false},
		{"https://hooks..cliente.test/x", false, false},
		{"https://-hooks.cliente.test/x", false, false},
		{"https://hoo_ks.cliente.test/x", false, false},
		{"https:///sem-host", false, false},
		{"/relativa", false, false},
		{"mailto:a@b.test", false, false},
		{"https://hooks.cliente.test/a b", false, false},
		{"", false, false},
		{"https://" + strings.Repeat("a", 2000) + ".test", false, false},
		{"https://" + strings.Repeat("a", 64) + ".test", false, false},
		{"https://" + strings.Repeat("abc.", 64) + "test", false, false},
		// O limite vale para a forma normalizada: caracteres não ASCII do
		// caminho e caracteres reservados viram codificação percentual.
		{"https://example.com/" + strings.Repeat("é", 980), true, false},
		{"https://example.com/" + strings.Repeat("{", 1900), true, false},
		// O valor informado conta caracteres, não bytes.
		{"https://example.com/?q=" + strings.Repeat("é", 1000), true, true},
		{"https://example.com/?q=" + strings.Repeat("é", 1980), true, false},
	}
	for _, c := range cases {
		out, err := ValidateURL(c.raw, URLRules{RequireHTTPS: c.https})
		if (err == nil) != c.ok {
			t.Errorf("ValidateURL(%.60q, https=%v): err=%v, esperado ok=%v", c.raw, c.https, err, c.ok)
		}
		if err == nil && utf8.RuneCountInString(out) > MaxURLLength {
			t.Errorf("ValidateURL(%.60q): forma normalizada com %d caracteres", c.raw, utf8.RuneCountInString(out))
		}
	}
}

func TestValidateInput(t *testing.T) {
	in, err := ValidateInput(Input{URL: " https://hooks.cliente.test/x ", Description: "  ERP  ",
		EventTypes: []string{outbox.EventInvoicePaid, outbox.EventInvoiceIssued, outbox.EventInvoicePaid}}, URLRules{})
	if err != nil {
		t.Fatal(err)
	}
	if in.URL != "https://hooks.cliente.test/x" || in.Description != "ERP" || strings.Join(in.EventTypes, ",") != "invoice.issued,invoice.paid" {
		t.Fatalf("normalização: %+v", in)
	}
	for name, bad := range map[string]Input{
		"sem eventos":         {URL: "https://a.test/x"},
		"evento interno":      {URL: "https://a.test/x", EventTypes: []string{outbox.EventProviderWebhook}},
		"descrição longa":     {URL: "https://a.test/x", EventTypes: []string{outbox.EventInvoicePaid}, Description: strings.Repeat("d", 201)},
		"url inválida":        {URL: "nada", EventTypes: []string{outbox.EventInvoicePaid}},
		"evento desconhecido": {URL: "https://a.test/x", EventTypes: []string{"invoice.nada"}},
	} {
		if _, err := ValidateInput(bad, URLRules{}); !apperr.IsKind(err, apperr.KindInvalid) {
			t.Errorf("%s: esperava erro de validação: %v", name, err)
		}
	}
	if _, err := ParseStatus("paused"); err == nil {
		t.Fatal("situação desconhecida")
	}
}

func TestEventCatalogExcludesInternalEvents(t *testing.T) {
	types := EventTypes()
	for _, internal := range []string{outbox.EventProviderWebhook, outbox.EventMemberInvited, outbox.EventDeadLetterReplay} {
		if Subscribable(internal) {
			t.Errorf("evento interno assinável: %s", internal)
		}
	}
	if !Subscribable(outbox.EventInvoiceOverdue) || len(types) != 21 {
		t.Fatalf("catálogo inesperado: %v", types)
	}
	for i := 1; i < len(types); i++ {
		if types[i-1] >= types[i] {
			t.Fatal("catálogo deve estar ordenado")
		}
	}
}

func TestSignatureRoundTripAndRotation(t *testing.T) {
	current, previous := NewSecretValue(), NewSecretValue()
	if !strings.HasPrefix(current, SecretPrefix) || current == previous {
		t.Fatal("segredos devem ser aleatórios e prefixados")
	}
	body := []byte(`{"id":"x"}`)
	header, count, err := SignatureHeader([]string{current, previous}, "evt_1", t0, body)
	if err != nil || count != 2 || len(strings.Fields(header)) != 2 {
		t.Fatalf("cabeçalho: %q %d %v", header, count, err)
	}
	ts := "1773154800" // t0 em segundos Unix
	for _, s := range []string{current, previous} {
		if err := Verify(s, "evt_1", ts, header, body, t0, 5*time.Minute); err != nil {
			t.Fatalf("assinatura deveria conferir com os dois segredos: %v", err)
		}
	}
	if err := Verify(NewSecretValue(), "evt_1", ts, header, body, t0, 5*time.Minute); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("segredo diferente: %v", err)
	}
	if err := Verify(current, "evt_2", ts, header, body, t0, 5*time.Minute); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("id diferente (id estável assinado): %v", err)
	}
	if err := Verify(current, "evt_1", ts, header, []byte(`{"id":"y"}`), t0, 5*time.Minute); !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("corpo adulterado: %v", err)
	}
	if err := Verify(current, "evt_1", ts, header, body, t0.Add(10*time.Minute), 5*time.Minute); !errors.Is(err, ErrTimestampTooOld) {
		t.Fatalf("timestamp fora da tolerância: %v", err)
	}
	if err := Verify(current, "evt_1", "abc", header, body, t0, 5*time.Minute); !errors.Is(err, ErrTimestampTooOld) {
		t.Fatalf("timestamp inválido: %v", err)
	}
	if err := Verify("sem-prefixo", "evt_1", ts, header, body, t0, 5*time.Minute); err == nil {
		t.Fatal("segredo inválido")
	}
	many := []string{NewSecretValue(), NewSecretValue(), NewSecretValue(), NewSecretValue(), NewSecretValue(), NewSecretValue()}
	if _, n, _ := SignatureHeader(many, "e", t0, body); n != MaxSignatures {
		t.Fatalf("limite de assinaturas: %d", n)
	}
	if _, _, err := SignatureHeader(nil, "e", t0, body); err == nil {
		t.Fatal("sem segredo deve falhar")
	}
	if _, _, err := SignatureHeader([]string{"whsec_curto"}, "e", t0, body); err == nil {
		t.Fatal("segredo curto deve falhar")
	}
	if _, err := Sign("whsec_%%%", "e", t0, body); err == nil {
		t.Fatal("segredo malformado deve falhar")
	}
}

// Vetor conhecido: HMAC-SHA256 de "msg_1.1700000000.{}" com chave de 32 bytes zero.
func TestSignKnownVector(t *testing.T) {
	secret := SecretPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	sig, err := Sign(secret, "msg_1", time.Unix(1700000000, 0), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sig, "v1,") || len(sig) != 3+44 {
		t.Fatalf("formato inesperado: %s", sig)
	}
	again, _ := Sign(secret, "msg_1", time.Unix(1700000000, 0), []byte("{}"))
	if sig != again {
		t.Fatal("assinatura deve ser determinística")
	}
}

func TestBackoff(t *testing.T) {
	cases := []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0.5, 30 * time.Second}, {2, 0.5, time.Minute}, {3, 0.5, 2 * time.Minute}, {9, 0.5, 128 * time.Minute},
		{11, 0.5, 6 * time.Hour}, {50, 0.5, 6 * time.Hour}, {1, 0, 24 * time.Second}, {1, 1, 36 * time.Second}, {1, 9, 36 * time.Second},
		{0, 0.5, 30 * time.Second}, {1, -1, 24 * time.Second},
	}
	for _, c := range cases {
		if got := Backoff(c.attempt, c.jitter); got != c.want {
			t.Errorf("Backoff(%d, %v) = %v, esperado %v", c.attempt, c.jitter, got, c.want)
		}
	}
}

func TestDeliveryLifecycle(t *testing.T) {
	d := NewDelivery(ids.New(), ids.New(), ids.New(), outbox.EventInvoicePaid, t0)
	d.MaxAttempts = 2
	if !d.Due(t0) || d.Due(t0.Add(-time.Second)) {
		t.Fatal("pendente vence em next_attempt_at")
	}
	if err := d.Record(t0, Outcome{StatusCode: 200}, 0.5); err == nil {
		t.Fatal("somente em andamento registra resultado")
	}
	if err := d.Claim(t0.Add(-time.Hour), t0); err == nil {
		t.Fatal("não vencida não é reivindicada")
	}
	_ = d.Claim(t0, t0.Add(time.Minute))
	if d.Status != Delivering || d.Attempts != 1 || d.Due(t0) || !d.Due(t0.Add(2*time.Minute)) {
		t.Fatalf("reivindicação: %+v", d)
	}
	_ = d.Record(t0, Outcome{StatusCode: 503}, 0.5)
	if d.Status != Retrying || !d.NextAttemptAt.Equal(t0.Add(30*time.Second)) || *d.LastStatusCode != 503 || d.LastError != "resposta sem sucesso do destino" {
		t.Fatalf("retentativa: %+v", d)
	}
	_ = d.Claim(t0.Add(time.Minute), t0.Add(2*time.Minute))
	_ = d.Record(t0, Outcome{StatusCode: 302}, 0.5)
	if d.Status != Exhausted || d.LastError != "redirecionamento não seguido" {
		t.Fatalf("esgotada: %+v", d)
	}
	if err := d.ScheduleManualRetry(t0); err != nil || d.Status != Pending || d.MaxAttempts != 3 {
		t.Fatalf("reenvio manual: %+v %v", d, err)
	}
	if err := d.ScheduleManualRetry(t0); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("reenvio já agendado: %v", err)
	}
	_ = d.Claim(t0, t0.Add(time.Minute))
	_ = d.Record(t0, Outcome{StatusCode: 204}, 0.5)
	if d.Status != Succeeded || d.DeliveredAt == nil || d.LastError != "" {
		t.Fatalf("sucesso: %+v", d)
	}
	if err := d.ScheduleManualRetry(t0); err != nil || d.DeliveredAt != nil || d.MaxAttempts != 4 {
		t.Fatalf("reenvio de entrega concluída: %+v", d)
	}
	p := NewDelivery(ids.New(), ids.New(), ids.New(), outbox.EventInvoicePaid, t0)
	_ = p.Claim(t0, t0.Add(time.Minute))
	_ = p.Record(t0, Outcome{Err: "destino\ninterno bloqueado", Permanent: true}, 0.5)
	if p.Status != Exhausted || p.LastStatusCode != nil || p.LastError != "destino interno bloqueado" {
		t.Fatalf("falha permanente: %+v", p)
	}
	r := NewDelivery(ids.New(), ids.New(), ids.New(), outbox.EventInvoicePaid, t0)
	_ = r.Claim(t0, t0.Add(time.Minute))
	_ = r.Record(t0, Outcome{Err: "timeout"}, 0.5)
	if err := r.ScheduleManualRetry(t0); err != nil || r.Status != Pending || r.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("reenvio antecipa a retentativa: %+v %v", r, err)
	}
	x := NewDelivery(ids.New(), ids.New(), ids.New(), outbox.EventInvoicePaid, t0)
	x.Exhaust(t0, ReasonEndpointDisabled)
	if x.Status != Exhausted || x.Due(t0) || (Delivery{Status: Succeeded}).Due(t0) {
		t.Fatalf("encerramento: %+v", x)
	}
}

// Reenvios manuais não ultrapassam o limite total de tentativas (o mesmo da
// coluna max_attempts).
func TestManualRetryRespectsTotalAttemptsLimit(t *testing.T) {
	for _, c := range []struct {
		attempts int
		ok       bool
	}{{MaxTotalAttempts - 1, true}, {MaxTotalAttempts, false}} {
		for _, st := range []DeliveryStatus{Succeeded, Exhausted} {
			d := Delivery{Status: st, Attempts: c.attempts, MaxAttempts: c.attempts}
			err := d.ScheduleManualRetry(t0)
			if c.ok && (err != nil || d.MaxAttempts != MaxTotalAttempts) {
				t.Fatalf("%s com %d tentativas: %+v %v", st, c.attempts, d, err)
			}
			if !c.ok {
				if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindConflict || ae.Code != "delivery_attempts_limit" || d.Status != st {
					t.Fatalf("%s com %d tentativas deve recusar: %+v %v", st, c.attempts, d, err)
				}
			}
		}
	}
}

// Uma entrega reivindicada e não tentada volta à fila sem consumir a
// tentativa.
func TestDeliveryRelease(t *testing.T) {
	d := NewDelivery(ids.New(), ids.New(), ids.New(), outbox.EventInvoicePaid, t0)
	if err := d.Release(0, t0, t0); !apperr.IsKind(err, apperr.KindConflict) {
		t.Fatalf("somente em andamento volta à fila: %v", err)
	}
	_ = d.Claim(t0, t0.Add(time.Minute))
	next := t0.Add(5 * time.Minute)
	if err := d.Release(0, t0, next); err != nil || d.Status != Pending || d.Attempts != 0 || d.LeaseUntil != nil || !d.NextAttemptAt.Equal(next) {
		t.Fatalf("devolução de pendente: %+v %v", d, err)
	}
	r := Delivery{Status: Retrying, Attempts: 2, MaxAttempts: DefaultMaxAttempts, NextAttemptAt: &t0}
	_ = r.Claim(t0, t0.Add(time.Minute))
	if err := r.Release(2, t0, t0); err != nil || r.Status != Retrying || r.Attempts != 2 || !r.Due(t0) {
		t.Fatalf("devolução de retentativa: %+v %v", r, err)
	}
}

func TestEventBody(t *testing.T) {
	ev := Event{ID: ids.New(), Type: outbox.EventInvoicePaid, Payload: json.RawMessage(`{"invoice_id":"x"}`), OccurredAt: t0}
	b, err := ev.Body()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != ev.ID.String() || got["type"] != "invoice.paid" || got["created_at"] != "2026-03-10T15:00:00Z" ||
		got["data"].(map[string]any)["invoice_id"] != "x" {
		t.Fatalf("corpo inesperado: %s", b)
	}
	empty, _ := Event{ID: ev.ID, Type: "x.y"}.Body()
	if !strings.Contains(string(empty), `"data":{}`) {
		t.Fatalf("payload vazio: %s", empty)
	}
}
