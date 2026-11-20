package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/collections"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/notifications"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

func decodePolicyBody(t *testing.T, raw string) policyBody {
	t.Helper()
	var b policyBody
	if err := decodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(raw)), &b); err != nil {
		t.Fatalf("corpo %s: %v", raw, err)
	}
	return b
}

const requiredPolicyFields = `"name":"Régua","reminder_offsets_days":[-3,0,5],"send_window_start_hour":0,"send_window_end_hour":20,"max_messages_per_day":2`

// O campo suspend_after_days distingue ausência, nulo e valor; os
// obrigatórios do contrato são exigidos mesmo quando o zero é válido.
func TestPolicyBodyInput(t *testing.T) {
	ten := 10
	base := collections.PolicyInput{Name: "Atual", IsDefault: true, OffsetsDays: []int{1}, WindowStartHour: 8, WindowEndHour: 18,
		MaxMessagesPerDay: 1, SuspendAfterDays: &ten, ReactivateOnPayment: true}
	cases := []struct {
		name        string
		raw         string
		base        *collections.PolicyInput
		wantDefault bool
		wantSuspend *int
		wantReact   bool
	}{
		{"criação com opcionais omitidos", `{` + requiredPolicyFields + `}`, nil, false, nil, false},
		{"criação completa", `{` + requiredPolicyFields + `,"is_default":true,"suspend_after_days":30,"reactivate_on_payment":true}`, nil,
			true, new(30), true},
		{"alteração mantém opcionais omitidos", `{` + requiredPolicyFields + `}`, &base, true, &ten, true},
		{"alteração com nulo remove a suspensão", `{` + requiredPolicyFields + `,"suspend_after_days":null,"is_default":false}`, &base,
			false, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, err := decodePolicyBody(t, c.raw).input(c.base)
			if err != nil {
				t.Fatal(err)
			}
			if in.Name != "Régua" || in.WindowStartHour != 0 || in.WindowEndHour != 20 || in.MaxMessagesPerDay != 2 || len(in.OffsetsDays) != 3 {
				t.Fatalf("campos obrigatórios: %+v", in)
			}
			if in.IsDefault != c.wantDefault || in.ReactivateOnPayment != c.wantReact {
				t.Fatalf("opcionais: %+v", in)
			}
			if (in.SuspendAfterDays == nil) != (c.wantSuspend == nil) || (c.wantSuspend != nil && *in.SuspendAfterDays != *c.wantSuspend) {
				t.Fatalf("suspensão: %v, esperado %v", in.SuspendAfterDays, c.wantSuspend)
			}
		})
	}
	if decodePolicyBody(t, `{`+requiredPolicyFields+`,"is_default":true,"suspend_after_days":null,"reactivate_on_payment":false}`).partial() {
		t.Fatal("corpo com todos os opcionais não é parcial")
	}
	_, err := decodePolicyBody(t, `{"name":"Sem campos"}`).input(nil)
	e, ok := apperr.As(err)
	if !ok || e.Kind != apperr.KindInvalid || e.Code != "invalid_collection_policy" || len(e.Fields) != 4 {
		t.Fatalf("campos obrigatórios ausentes: %v", err)
	}
	var b policyBody
	if err := decodeJSON(httptest.NewRequest("POST", "/", strings.NewReader(`{"suspend_after_days":"dez"}`)), &b); err == nil {
		t.Fatal("suspensão não numérica deve ser rejeitada")
	}
}

func TestNullableIntUnmarshal(t *testing.T) {
	var v struct {
		N nullableInt `json:"n"`
	}
	for raw, want := range map[string]string{`{}`: "ausente", `{"n":null}`: "nulo", `{"n":7}`: "7"} {
		v.N = nullableInt{}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		got := "ausente"
		switch {
		case v.N.Set && v.N.Value == nil:
			got = "nulo"
		case v.N.Set:
			got = "7"
			if *v.N.Value != 7 {
				got = "valor inesperado"
			}
		}
		if got != want {
			t.Errorf("%s: %s, esperado %s", raw, got, want)
		}
	}
}

func TestDecodeOptionalJSON(t *testing.T) {
	var body struct {
		Hours *int `json:"previous_secret_valid_hours"`
	}
	if err := decodeOptionalJSON(httptest.NewRequest("POST", "/", nil), &body); err != nil || body.Hours != nil {
		t.Fatalf("sem corpo: %v %v", body.Hours, err)
	}
	if err := decodeOptionalJSON(httptest.NewRequest("POST", "/", strings.NewReader(`{"previous_secret_valid_hours":2}`)), &body); err != nil ||
		body.Hours == nil || *body.Hours != 2 {
		t.Fatalf("com corpo: %v %v", body.Hours, err)
	}
	if err := decodeOptionalJSON(httptest.NewRequest("POST", "/", strings.NewReader(`{"outro":1}`)), &body); !apperr.IsKind(err, apperr.KindInvalid) {
		t.Fatalf("campo desconhecido: %v", err)
	}
}

// As respostas seguem o contrato: listas nunca nulas, destinatários num
// único texto e endpoint sem o valor dos segredos.
func TestViewsFollowContract(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	pv := policyView(collections.Policy{ID: uuid.New(), Name: "Sem lembretes", Version: 1, CreatedAt: now})
	if pv.ReminderOffsetsDays == nil {
		t.Fatal("passos nulos na resposta")
	}
	tv := templateView(notifications.Template{Key: notifications.KeyReminderAfterDue, Subject: "s", Body: "b", IsDefault: true})
	if tv.Channel != "email" || len(tv.Variables) == 0 || tv.Variables[len(tv.Variables)-1] != notifications.VarDaysOverdue {
		t.Fatalf("template: %+v", tv)
	}
	if v := templateView(notifications.Template{Key: "desconhecido"}); v.Variables == nil {
		t.Fatal("variáveis nulas na resposta")
	}
	nv := notificationView(notifications.Notification{ID: uuid.New(), Recipients: []string{"a@x.test", "b@x.test"}, Status: notifications.Sent})
	if nv.Recipient != "a@x.test, b@x.test" || nv.Channel != "email" {
		t.Fatalf("notificação: %+v", nv)
	}
	ev := endpointView(webhooks.Endpoint{ID: uuid.New(), Status: webhooks.Active, Secrets: []webhooks.SecretInfo{{ID: uuid.New(), CreatedAt: now}}})
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventTypes == nil || len(ev.Secrets) != 1 || strings.Contains(string(raw), "whsec_") || strings.Contains(string(raw), `"value"`) {
		t.Fatalf("endpoint: %s", raw)
	}
	code := 500
	dv := deliveryView(webhooks.Delivery{ID: uuid.New(), Status: webhooks.Retrying, LastStatusCode: &code, Attempts: 2})
	if dv.Status != "retrying" || *dv.LastStatusCode != 500 || dv.Attempts != 2 {
		t.Fatalf("entrega: %+v", dv)
	}
	in := endpointBody{URL: "https://a.test", EventTypes: []string{"invoice.paid"}}.input("atual")
	if in.Description != "atual" {
		t.Fatalf("descrição omitida mantém a atual: %+v", in)
	}
	desc := ""
	if in := (endpointBody{Description: &desc}).input("atual"); in.Description != "" {
		t.Fatalf("descrição vazia informada substitui a atual: %+v", in)
	}
}
