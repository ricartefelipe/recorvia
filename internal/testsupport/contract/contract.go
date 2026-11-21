// Package contract valida as respostas HTTP contra o contrato OpenAPI
// (api/openapi.yaml) nos testes. O middleware grava cada resposta do
// roteador, localiza a operação do contrato pelo método e pelo caminho e
// confere status, Content-Type, headers declarados e corpo com o schema da
// resposta.
//
// Regras para status não declarados na operação: o contrato estabelece que
// todo erro segue a RFC 9457, então um status 4xx não listado é aceito
// somente como application/problem+json válido contra
// components/schemas/Problem; um status 5xx não listado é divergência, como
// um status de sucesso ou redirecionamento não listado (ADR-084): a operação
// que devolve 503 por regra própria o declara. A falha provocada de propósito
// num teste (por exemplo, 500 internal_error) é aceita somente com a exceção
// explícita AllowUndeclared, restrita ao teste, ao método, ao caminho e ao
// status. Toda resposta problem+json deve repetir no campo status o status
// HTTP. Redirecionamentos declarados exigem Location e não têm o corpo
// conferido (a nota de hipertexto da RFC 9110). Requisições a caminhos fora
// do contrato (rota inexistente, método não permitido) só podem responder
// com 4xx em problem+json, salvo os caminhos ignorados explicitamente.
package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"sync"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/ricartefelipe/recorvia/api"
)

const problemMIME = "application/problem+json"

var registerDecoders sync.Once

// uuidFormat confere o formato uuid (o kin-openapi só valida formatos com
// validador registrado).
var uuidFormat = openapi3.NewRegexpFormatValidator(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validator confere respostas contra um documento OpenAPI.
type Validator struct {
	doc     *openapi3.T
	router  routers.Router
	problem *openapi3.Schema
	ignored map[string]bool
}

// New carrega e valida o documento.
func New(spec []byte) (*Validator, error) {
	registerDecoders.Do(func() {
		// Documentos HTML (fatura para impressão) e exportações CSV são texto;
		// o schema do contrato, quando houver, é do tipo string. O decodificador
		// padrão do CSV lê vírgulas e recusaria a exportação de faturas, que
		// usa ponto e vírgula e campos entre aspas.
		openapi3filter.RegisterBodyDecoder("text/html", openapi3filter.PlainBodyDecoder)
		openapi3filter.RegisterBodyDecoder("text/csv", openapi3filter.PlainBodyDecoder)
	})
	doc, err := openapi3.NewLoader().LoadFromData(spec)
	if err != nil {
		return nil, fmt.Errorf("contrato: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("contrato inválido: %w", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("contrato: %w", err)
	}
	var ref *openapi3.SchemaRef
	if doc.Components != nil {
		ref = doc.Components.Schemas["Problem"]
	}
	if ref == nil || ref.Value == nil {
		return nil, errors.New("contrato: components/schemas/Problem ausente")
	}
	return &Validator{doc: doc, router: router, problem: ref.Value, ignored: map[string]bool{}}, nil
}

// Ignore exclui da validação um caminho fora do contrato ("GET /api/openapi.yaml").
func (v *Validator) Ignore(method, path string) *Validator {
	v.ignored[method+" "+path] = true
	return v
}

// Check confere uma resposta à requisição informada.
func (v *Validator) Check(req *http.Request, status int, header http.Header, body []byte) error {
	if v.ignored[req.Method+" "+req.URL.Path] {
		return nil
	}
	if err := checkProblemStatus(status, header, body); err != nil {
		return err
	}
	route, params, err := v.router.FindRoute(req)
	if err != nil {
		// Caminho ou método fora do contrato: apenas erros 4xx estruturados.
		if status < 400 || status >= 500 {
			return fmt.Errorf("resposta %d a uma operação fora do contrato", status)
		}
		return v.checkProblem(header, body)
	}
	responses := route.Operation.Responses
	declared := responses.Status(status)
	if declared == nil {
		declared = responses.Default()
	}
	if declared == nil {
		if status < 400 || status >= 500 {
			return fmt.Errorf("status %d não declarado na operação %s", status, route.Operation.OperationID)
		}
		return v.checkProblem(header, body)
	}
	if status >= 300 && status < 400 {
		// Redirecionamento: o corpo é a nota de hipertexto opcional da RFC
		// 9110 (http.Redirect); o que importa é o destino.
		if header.Get("Location") == "" {
			return fmt.Errorf("redirecionamento %d sem Location", status)
		}
		return nil
	}
	if declared.Value != nil && len(declared.Value.Content) == 0 && len(body) > 0 && req.Method != http.MethodHead {
		return fmt.Errorf("status %d declarado sem corpo, mas a resposta tem corpo %s", status, header.Get("Content-Type"))
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 status,
		Header:                 header,
		Options: &openapi3filter.Options{MultiError: true, IncludeResponseStatus: true,
			SchemaValidationOptions: []openapi3.SchemaValidationOption{openapi3.WithStringFormatValidator("uuid", uuidFormat)}},
	}
	in.SetBodyBytes(body)
	return openapi3filter.ValidateResponse(req.Context(), in)
}

// checkAllowedUndeclared confere o 5xx aceito por AllowUndeclared: somente
// problem+json válido, com o status repetido no corpo.
func (v *Validator) checkAllowedUndeclared(_ *http.Request, status int, header http.Header, body []byte) error {
	if err := checkProblemStatus(status, header, body); err != nil {
		return err
	}
	return v.checkProblem(header, body)
}

// checkProblem confere uma resposta de erro não declarada pela operação.
func (v *Validator) checkProblem(header http.Header, body []byte) error {
	if mt := mediaType(header); mt != problemMIME {
		return fmt.Errorf("erro não declarado com Content-Type %q (esperado %s)", header.Get("Content-Type"), problemMIME)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return fmt.Errorf("corpo problem+json inválido: %w", err)
	}
	if err := v.problem.VisitJSON(value, openapi3.MultiErrors(), openapi3.VisitAsResponse()); err != nil {
		return fmt.Errorf("corpo não confere com components/schemas/Problem: %w", err)
	}
	return nil
}

// checkProblemStatus exige que o campo status de um problem+json repita o
// status HTTP da resposta (RFC 9457, seção 3.1.2).
func checkProblemStatus(status int, header http.Header, body []byte) error {
	if mediaType(header) != problemMIME {
		return nil
	}
	var p struct {
		Status *int `json:"status"`
	}
	// Corpo malformado ou sem status é apontado pela validação do schema.
	if json.Unmarshal(body, &p) == nil && p.Status != nil && *p.Status != status {
		return fmt.Errorf("problem+json com status %d em resposta HTTP %d", *p.Status, status)
	}
	return nil
}

func mediaType(h http.Header) string {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return mt
}

// undeclared guarda as exceções explícitas de status 5xx não declarados,
// por teste (o mesmo testing.TB que montou o servidor com Wrap ou
// Middleware).
var undeclared = struct {
	sync.Mutex
	byTest map[testing.TB]map[string]bool
}{byTest: map[testing.TB]map[string]bool{}}

func undeclaredKey(method, path string, status int) string {
	return fmt.Sprintf("%s %s %d", method, path, status)
}

// AllowUndeclared aceita, no teste t e somente para a requisição method path
// (caminho concreto, sem query), o status 5xx que o contrato não declara. Serve
// à falha provocada de propósito (falha simulada no banco, pânico), que
// nenhuma operação documenta; a resposta continua obrigada a ser
// problem+json válido com o status repetido no corpo. t é o teste que montou
// o servidor; a exceção termina com ele.
func AllowUndeclared(t testing.TB, method, path string, status int) {
	t.Helper()
	if status < 500 || status > 599 {
		t.Fatalf("contrato: AllowUndeclared aceita somente status 5xx, não %d", status)
	}
	undeclared.Lock()
	defer undeclared.Unlock()
	allowed := undeclared.byTest[t]
	if allowed == nil {
		allowed = map[string]bool{}
		undeclared.byTest[t] = allowed
		t.Cleanup(func() {
			undeclared.Lock()
			defer undeclared.Unlock()
			delete(undeclared.byTest, t)
		})
	}
	allowed[undeclaredKey(method, path, status)] = true
}

func allowedUndeclared(t testing.TB, method, path string, status int) bool {
	undeclared.Lock()
	defer undeclared.Unlock()
	return undeclared.byTest[t][undeclaredKey(method, path, status)]
}

// Middleware valida cada resposta de next e reporta as divergências em t.
func Middleware(t testing.TB, v *Validator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &recorder{ResponseWriter: w}
		// O contrato é localizado pela URL recebida; o roteador da aplicação
		// pode alterar a requisição repassada.
		req := r.Clone(r.Context())
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		check := v.Check
		if status >= 500 && allowedUndeclared(t, req.Method, req.URL.Path, status) {
			check = v.checkAllowedUndeclared
		}
		if err := check(req, status, rec.header(), rec.body.Bytes()); err != nil {
			t.Errorf("contrato: %s %s respondeu %d: %s\ncorpo: %s", r.Method, r.URL.Path, status, flatten(err), truncate(rec.body.String(), 2000))
		}
	})
}

var spaces = regexp.MustCompile(`\s+`)

func flatten(err error) string { return spaces.ReplaceAllString(err.Error(), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// recorder repassa a resposta ao cliente e guarda status, headers e corpo.
type recorder struct {
	http.ResponseWriter
	status   int
	snapshot http.Header
	body     bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
		r.snapshot = r.ResponseWriter.Header().Clone()
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// Flush mantém o envio progressivo (exportações).
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap permite o uso de http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) header() http.Header {
	if r.snapshot != nil {
		return r.snapshot
	}
	return r.ResponseWriter.Header().Clone()
}

// Wrap é o atalho dos testes de integração: envolve o handler com a
// validação do contrato api/openapi.yaml (a rota utilitária que serve o
// próprio contrato fica de fora).
func Wrap(t testing.TB, h http.Handler) http.Handler {
	t.Helper()
	v, err := New(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	return Middleware(t, v.Ignore(http.MethodGet, "/api/openapi.yaml"), h)
}
