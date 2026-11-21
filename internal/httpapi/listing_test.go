package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func rulesAPI() *API { return &API{log: slog.New(slog.NewTextHandler(io.Discard, nil))} }

type problemBody struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Errors []struct {
		Field string `json:"field"`
	} `json:"errors"`
}

func serveRules(t *testing.T, method, pattern, target string, header http.Header) (*httptest.ResponseRecorder, problemBody, bool) {
	t.Helper()
	reached := false
	h := rulesAPI().requestRules(method, pattern, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var p problemBody
	if w.Code != http.StatusOK {
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Fatalf("Content-Type = %q, esperado application/problem+json", ct)
		}
		if err := json.NewDecoder(w.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
	}
	return w, p, reached
}

// A ordenação das listagens é uma allowlist: sort fora dela, repetido ou em
// listagem que não ordena por parâmetro é recusado com 422 invalid_sort antes
// do handler, em vez de ignorado; rotas que não são listagens não são afetadas.
func TestListSortAllowlist(t *testing.T) {
	cases := []struct {
		name, pattern, target string
		want                  int
	}{
		{"valor da allowlist", "/api/v1/customers", "/api/v1/customers?sort=-created_at", http.StatusOK},
		{"sem ordenação", "/api/v1/customers", "/api/v1/customers?limit=10", http.StatusOK},
		{"fora da allowlist", "/api/v1/customers", "/api/v1/customers?sort=email", http.StatusUnprocessableEntity},
		{"vazio", "/api/v1/customers", "/api/v1/customers?sort=", http.StatusUnprocessableEntity},
		{"repetido", "/api/v1/customers", "/api/v1/customers?sort=name&sort=-name", http.StatusUnprocessableEntity},
		{"listagem sem ordenação", "/api/v1/disputes", "/api/v1/disputes?sort=opened_at", http.StatusUnprocessableEntity},
		{"listagem sem ordenação, sem sort", "/api/v1/disputes", "/api/v1/disputes?cursor=x", http.StatusOK},
		{"rota que não é listagem", "/api/v1/disputes/{id}", "/api/v1/disputes/1?sort=x", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, p, reached := serveRules(t, http.MethodGet, tc.pattern, tc.target, nil)
			if w.Code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Fatalf("status %d (handler chamado: %v), esperado %d", w.Code, reached, tc.want)
			}
			if tc.want != http.StatusOK && (p.Code != "invalid_sort" || p.Status != 422 || len(p.Errors) != 1 || p.Errors[0].Field != "sort") {
				t.Fatalf("problema inesperado: %+v", p)
			}
		})
	}
}

// As alterações versionadas sem If-Match são recusadas com 428
// if_match_required antes do handler; as demais rotas não exigem o header.
func TestVersionedRoutesRequireIfMatch(t *testing.T) {
	cases := []struct {
		name, method, pattern, ifMatch string
		want                           int
	}{
		{"sem If-Match", http.MethodPost, "/api/v1/coupons/{id}/archive", "", http.StatusPreconditionRequired},
		{"If-Match em branco", http.MethodDelete, "/api/v1/members/{user_id}", "  ", http.StatusPreconditionRequired},
		{"com If-Match", http.MethodPut, "/api/v1/customers/{id}/billing-address", `"3"`, http.StatusOK},
		{"rota sem versão", http.MethodPost, "/api/v1/customers/{id}/contacts", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.ifMatch != "" {
				h.Set("If-Match", tc.ifMatch)
			}
			w, p, reached := serveRules(t, tc.method, tc.pattern, "/x", h)
			if w.Code != tc.want || reached != (tc.want == http.StatusOK) {
				t.Fatalf("status %d (handler chamado: %v), esperado %d", w.Code, reached, tc.want)
			}
			if tc.want != http.StatusOK && (p.Code != "if_match_required" || p.Status != 428) {
				t.Fatalf("problema inesperado: %+v", p)
			}
		})
	}
}

// "0" é a versão do template padrão e do provedor ainda não configurado; os
// demais valores seguem ifMatch (que recusa 0 nos recursos já gravados).
func TestIfMatchOrDefault(t *testing.T) {
	cases := []struct {
		header string
		want   int
		code   string
	}{
		{`"0"`, 0, ""}, {`W/"0"`, 0, ""}, {"0", 0, ""}, {`"4"`, 4, ""}, {`"-1"`, 0, "invalid_if_match"}, {`"x"`, 0, "invalid_if_match"},
		{"", 0, "if_match_required"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPut, "/x", nil)
		if tc.header != "" {
			req.Header.Set("If-Match", tc.header)
		}
		got, err := ifMatchOrDefault(req)
		code := ""
		if err != nil {
			var p problemBody
			w := httptest.NewRecorder()
			rulesAPI().writeError(w, req, err)
			_ = json.NewDecoder(w.Body).Decode(&p)
			code = p.Code
		}
		if got != tc.want || code != tc.code {
			t.Errorf("If-Match %q: versão %d, erro %q; esperado %d, %q", tc.header, got, code, tc.want, tc.code)
		}
	}
	// Nos recursos já gravados, "0" não é versão válida.
	req := httptest.NewRequest(http.MethodPatch, "/x", nil)
	req.Header.Set("If-Match", `"0"`)
	if _, err := ifMatch(req); err == nil {
		t.Fatal(`ifMatch deveria recusar "0"`)
	}
}
