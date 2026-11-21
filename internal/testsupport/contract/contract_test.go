package contract

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ricartefelipe/recorvia/api"
)

const spec = `
openapi: 3.0.3
info: { title: teste, version: "1" }
servers: [ { url: / } ]
paths:
  /items:
    get:
      operationId: listItems
      responses:
        "200":
          description: Lista
          content:
            application/json:
              schema:
                type: object
                required: [items]
                properties:
                  items: { type: array, items: { $ref: "#/components/schemas/Item" } }
  /items/export:
    get:
      operationId: exportItems
      responses:
        "200": { description: CSV, content: { text/csv: { schema: { type: string } } } }
  /go:
    get:
      operationId: redirect
      responses:
        "302": { description: Redireciona }
  /items/{id}:
    get:
      operationId: getItem
      parameters: [ { name: id, in: path, required: true, schema: { type: string, format: uuid } } ]
      responses:
        "200":
          description: Item
          headers: { ETag: { required: true, schema: { type: string } } }
          content: { application/json: { schema: { $ref: "#/components/schemas/Item" } } }
        "409": { description: Conflito, content: { application/problem+json: { schema: { $ref: "#/components/schemas/Problem" } } } }
    delete:
      operationId: deleteItem
      parameters: [ { name: id, in: path, required: true, schema: { type: string, format: uuid } } ]
      responses:
        "204": { description: Removido }
components:
  schemas:
    Item:
      type: object
      required: [id, name]
      properties:
        id: { type: string, format: uuid }
        name: { type: string }
        created_at: { type: string, format: date-time }
    Problem:
      type: object
      required: [type, title, status, code]
      properties:
        type: { type: string }
        title: { type: string }
        status: { type: integer }
        code: { type: string }
`

const itemID = "7c1e2f2a-4f63-4a54-9a43-0b6d2f9f7f10"

func problemBody(status int) string {
	return fmt.Sprintf(`{"type":"https://x/p","title":"t","status":%d,"code":"c"}`, status)
}

func TestCheckFollowsTheContract(t *testing.T) {
	v, err := New([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	json := http.Header{"Content-Type": {"application/json"}}
	jsonETag := http.Header{"Content-Type": {"application/json"}, "Etag": {`"1"`}}
	problem := http.Header{"Content-Type": {"application/problem+json"}}
	text := http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
	cases := []struct {
		name    string
		method  string
		path    string
		status  int
		header  http.Header
		body    string
		wantErr string
	}{
		{"lista válida", http.MethodGet, "/items", 200, json, `{"items":[{"id":"` + itemID + `","name":"a"}]}`, ""},
		{"campo obrigatório ausente", http.MethodGet, "/items", 200, json, `{"items":[{"id":"` + itemID + `"}]}`, "name"},
		{"uuid malformado", http.MethodGet, "/items", 200, json, `{"items":[{"id":"x","name":"a"}]}`, "uuid"},
		{"data-hora malformada", http.MethodGet, "/items", 200, json, `{"items":[{"id":"` + itemID + `","name":"a","created_at":"ontem"}]}`, "date-time"},
		{"Content-Type divergente", http.MethodGet, "/items", 200, text, `{"items":[]}`, "Content-Type"},
		{"caminho literal antes do parâmetro", http.MethodGet, "/items/export", 200, http.Header{"Content-Type": {"text/csv"}}, "id\n1\n", ""},
		{"CSV com ponto e vírgula e aspas", http.MethodGet, "/items/export", 200, http.Header{"Content-Type": {"text/csv; charset=utf-8"}},
			"numero;cliente\nACME-1;\"'=HIPERLINK(\"\"http://x.test\"\")\"\n", ""},
		{"header obrigatório ausente", http.MethodGet, "/items/" + itemID, 200, json, `{"id":"` + itemID + `","name":"a"}`, "ETag"},
		{"recurso com header", http.MethodGet, "/items/" + itemID, 200, jsonETag, `{"id":"` + itemID + `","name":"a"}`, ""},
		{"erro declarado", http.MethodGet, "/items/" + itemID, 409, problem, problemBody(409), ""},
		{"erro declarado fora do schema", http.MethodGet, "/items/" + itemID, 409, problem, `{"status":409}`, "doesn't match"},
		{"erro não declarado em problem+json", http.MethodGet, "/items/" + itemID, 404, problem, problemBody(404), ""},
		{"erro não declarado em texto", http.MethodGet, "/items/" + itemID, 404, text, "404 page not found", "problem+json"},
		{"erro não declarado fora do schema", http.MethodGet, "/items", 422, problem, `{"code":"x","status":422}`, "Problem"},
		{"status do corpo diferente do HTTP", http.MethodGet, "/items/" + itemID, 404, problem, problemBody(400), "status 400"},
		{"sucesso não declarado", http.MethodGet, "/items", 201, json, `{"items":[]}`, "não declarado"},
		{"sem corpo declarado", http.MethodDelete, "/items/" + itemID, 204, http.Header{}, "", ""},
		{"corpo onde o contrato não declara", http.MethodDelete, "/items/" + itemID, 204, json, `{}`, "sem corpo"},
		{"redirecionamento com nota de hipertexto", http.MethodGet, "/go", 302,
			http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Location": {"/destino"}}, `<a href="/destino">Found</a>.`, ""},
		{"redirecionamento sem destino", http.MethodGet, "/go", 302, http.Header{}, "", "Location"},
		{"rota inexistente com problema", http.MethodGet, "/nada", 404, problem, problemBody(404), ""},
		{"método fora do contrato com problema", http.MethodPost, "/items", 405, problem, problemBody(405), ""},
		{"rota inexistente com sucesso", http.MethodGet, "/nada", 200, json, `{}`, "fora do contrato"},
		{"rota inexistente em texto", http.MethodGet, "/nada", 404, text, "404 page not found", "problem+json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			err := v.Check(req, tc.status, tc.header, []byte(tc.body))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("erro inesperado: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("divergência não detectada (esperado %q)", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("erro %q sem %q", err, tc.wantErr)
			}
		})
	}
}

func TestIgnoredPathIsNotChecked(t *testing.T) {
	v, err := New([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	v.Ignore(http.MethodGet, "/spec.yaml")
	req := httptest.NewRequest(http.MethodGet, "/spec.yaml", nil)
	if err := v.Check(req, 200, http.Header{"Content-Type": {"application/yaml"}}, []byte("a: 1")); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsInvalidDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"yaml inválido":    "openapi: [",
		"sem Problem":      "openapi: 3.0.3\ninfo: { title: t, version: \"1\" }\npaths: {}\n",
		"documento falho":  "openapi: 3.0.3\ninfo: { title: t }\npaths: {}\n",
		"referência falha": "openapi: 3.0.3\ninfo: { title: t, version: \"1\" }\npaths:\n  /a:\n    get:\n      responses:\n        \"200\": { $ref: \"#/components/responses/X\" }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New([]byte(doc)); err == nil {
				t.Fatal("documento deveria ser recusado")
			}
		})
	}
}

// recordingTB guarda as falhas reportadas pelo middleware.
type recordingTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) reported() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

func TestMiddlewareReportsDivergencesAndKeepsResponse(t *testing.T) {
	v, err := New([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	tb := &recordingTB{TB: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	mux.HandleFunc("GET /items/export", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("id\n"))
		http.NewResponseController(w).Flush() //nolint:errcheck // o gravador repassa ao ResponseWriter do teste
		_, _ = w.Write([]byte("1\n"))
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	h := Middleware(tb, v, mux)
	for _, path := range []string{"/items", "/items/export", "/items/" + itemID, "/nada"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/items/export" && rec.Body.String() != "id\n1\n" {
			t.Fatalf("corpo repassado %q", rec.Body.String())
		}
	}
	got := tb.reported()
	if len(got) != 2 {
		t.Fatalf("divergências %d, esperado 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "GET /items/"+itemID+" respondeu 200") || !strings.Contains(got[1], "GET /nada respondeu 404") {
		t.Fatalf("mensagens sem método, caminho e status: %v", got)
	}
}

// O contrato do projeto carrega no validador.
func TestProjectContractLoads(t *testing.T) {
	tb := &recordingTB{TB: t}
	h := Wrap(tb, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/openapi.yaml", nil))
	if len(tb.reported()) != 0 {
		t.Fatal(tb.reported())
	}
}
