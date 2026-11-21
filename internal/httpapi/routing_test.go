package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// RF-15: rota inexistente e método não permitido respondem no formato RFC
// 9457 (application/problem+json) com o request_id da requisição; o 405
// informa em Allow os métodos aceitos pela rota.
func TestUnmatchedRoutesAnswerWithProblemDetails(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		path      string
		status    int
		code      string
		wantAllow []string
	}{
		{"rota inexistente", http.MethodGet, "/api/v1/inexistente", http.StatusNotFound, "route_not_found", nil},
		{"rota inexistente fora da API", http.MethodPost, "/qualquer/coisa", http.StatusNotFound, "route_not_found", nil},
		{"método desconhecido em rota inexistente", "PROPFIND", "/nao/existe", http.StatusNotFound, "route_not_found", nil},
		{"método desconhecido em rota existente", "PROPFIND", "/livez", http.StatusMethodNotAllowed, "method_not_allowed",
			[]string{http.MethodGet, http.MethodHead}},
		{"método não permitido em rota pública", http.MethodDelete, "/livez", http.StatusMethodNotAllowed, "method_not_allowed",
			[]string{http.MethodGet, http.MethodHead}},
		{"método não permitido em coleção", http.MethodDelete, "/api/v1/customers", http.StatusMethodNotAllowed, "method_not_allowed",
			[]string{http.MethodGet, http.MethodHead, http.MethodPost}},
		{"método não permitido em recurso com parâmetro", http.MethodPut, "/api/v1/customers/7c1e2f2a-4f63-4a54-9a43-0b6d2f9f7f10",
			http.StatusMethodNotAllowed, "method_not_allowed", []string{http.MethodGet, http.MethodHead, http.MethodPatch}},
		{"método não permitido em rota só de escrita", http.MethodGet, "/auth/logout", http.StatusMethodNotAllowed, "method_not_allowed",
			[]string{http.MethodPost}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			_, router := NewRouter(Deps{Log: logging.New(&logs, "info", "api"), Clock: clock.System{}})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("X-Request-ID", "req-rota-0001")
			router.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, esperado %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type %q", ct)
			}
			var p problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("corpo não é JSON: %v (%s)", err, rec.Body.String())
			}
			if p.Status != tc.status || p.Code != tc.code || p.Title == "" || p.Detail == "" ||
				p.Type != "https://recorvia.dev/problems/"+strings.ReplaceAll(tc.code, "_", "-") {
				t.Fatalf("problema %+v", p)
			}
			if p.RequestID != "req-rota-0001" {
				t.Fatalf("request_id %q", p.RequestID)
			}
			allow := rec.Header().Values("Allow")
			slices.Sort(allow)
			want := slices.Clone(tc.wantAllow)
			slices.Sort(want)
			if !slices.Equal(allow, want) {
				t.Fatalf("Allow %v, esperado %v", allow, want)
			}
			// O log de acesso registra a resposta com o mesmo request_id.
			if !strings.Contains(logs.String(), `"request_id":"req-rota-0001"`) {
				t.Fatalf("log sem request_id: %s", logs.String())
			}
		})
	}
}

// RFC 9110: HEAD em rota GET responde como o GET (status e headers), sem
// corpo; em rota sem GET, é método não permitido.
func TestHeadAnswersLikeGetWithoutBody(t *testing.T) {
	_, router := NewRouter(Deps{Log: logging.New(io.Discard, "info", "api"), Clock: clock.System{}})
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	cases := []struct {
		path   string
		status int
		ct     string
	}{
		{"/livez", http.StatusOK, "application/json"},
		{"/api/v1/customers", http.StatusUnauthorized, "application/problem+json"},
		{"/auth/logout", http.StatusMethodNotAllowed, "application/problem+json"},
	}
	for _, tc := range cases {
		get, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		getBody, _ := io.ReadAll(get.Body)
		_ = get.Body.Close()
		head, err := http.Head(srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(head.Body)
		_ = head.Body.Close()
		if head.StatusCode != tc.status || head.Header.Get("Content-Type") != tc.ct || len(body) != 0 {
			t.Fatalf("HEAD %s: %d %q corpo %q", tc.path, head.StatusCode, head.Header.Get("Content-Type"), body)
		}
		if tc.status != http.StatusMethodNotAllowed && (get.StatusCode != head.StatusCode || len(getBody) == 0) {
			t.Fatalf("GET %s: %d, HEAD %d", tc.path, get.StatusCode, head.StatusCode)
		}
	}
}
