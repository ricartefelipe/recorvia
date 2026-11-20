package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

// listedPackage é o subconjunto da saída de `go list -json` usado aqui.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
}

// typedPackage é o código de produção deste pacote com informação de tipos.
type typedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

// loadTypedPackage verifica os tipos dos arquivos de produção do pacote. As
// dependências vêm dos dados de exportação do cache de compilação, obtidos
// com `go list -export` (sem rede: os módulos já estão no cache local).
func loadTypedPackage(t *testing.T) typedPackage {
	t.Helper()
	self := reflect.TypeFor[API]().PkgPath()
	var stderr bytes.Buffer
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export", ".")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var target listedPackage
	for dec := json.NewDecoder(bytes.NewReader(out)); dec.More(); {
		var p listedPackage
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if p.ImportPath == self {
			target = p
		}
	}
	if len(target.GoFiles) == 0 {
		t.Fatalf("go list não devolveu os arquivos de %s", self)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range target.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(target.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("sem dados de exportação para %s", path)
		}
		return os.Open(file) //nolint:gosec // caminho devolvido pelo go list, dentro do cache de compilação
	})
	return checkFiles(t, fset, self, files, imp)
}

// checkFiles verifica os tipos dos arquivos como o pacote path.
func checkFiles(t *testing.T, fset *token.FileSet, path string, files []*ast.File, imp types.Importer) typedPackage {
	t.Helper()
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{}, Types: map[ast.Expr]types.TypeAndValue{}}
	pkg, err := (&types.Config{Importer: imp}).Check(path, fset, files, info)
	if err != nil {
		t.Fatal(err)
	}
	return typedPackage{fset: fset, files: files, info: info, pkg: pkg}
}

// credentialSafePrincipal lista o que uma rota que aceita credencial de API
// pode ler do principal: os campos preenchidos também para a credencial e os
// métodos que já a distinguem. Todo o restante (User, Session, Role,
// PlatformStaff, UserRef e o que vier a ser acrescentado ao tipo) descreve o
// usuário da sessão e tem valor zero para credenciais: Session.UserID e
// User.ID são uuid.Nil, e Role é vazio.
var credentialSafePrincipal = map[string]bool{
	"TenantID": true, "Credential": true,
	"Can": true, "Actor": true, "UserID": true, "ClientID": true,
}

// userIdentityChecker encontra, a partir de um handler, os pontos do pacote
// que leem do principal algo além de credentialSafePrincipal. Os métodos do
// principal não são percorridos: os permitidos já distinguem a credencial, e
// os demais são apontados na chamada.
type userIdentityChecker struct {
	tp        typedPackage
	decls     map[*types.Func]*ast.FuncDecl
	principal *types.TypeName
	// userFields são os campos do principal fora de credentialSafePrincipal.
	userFields map[*types.Var]bool
}

func newUserIdentityChecker(t *testing.T, tp typedPackage) *userIdentityChecker {
	t.Helper()
	c := &userIdentityChecker{tp: tp, decls: map[*types.Func]*ast.FuncDecl{}, userFields: map[*types.Var]bool{}}
	for _, f := range tp.files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fn, ok := tp.info.Defs[fd.Name].(*types.Func); ok {
					c.decls[fn] = fd
				}
			}
		}
	}
	c.principal, _ = tp.pkg.Scope().Lookup("Principal").(*types.TypeName)
	if c.principal == nil {
		t.Fatal("tipo Principal não encontrado")
	}
	st, ok := c.principal.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatal("Principal deveria ser uma struct")
	}
	for i := range st.NumFields() {
		if f := st.Field(i); !credentialSafePrincipal[f.Name()] {
			c.userFields[f] = true
		}
	}
	for name := range credentialSafePrincipal {
		if obj, _, _ := types.LookupFieldOrMethod(c.principal.Type(), false, tp.pkg, name); obj == nil {
			t.Fatalf("Principal.%s, permitido às rotas que aceitam credencial, não existe; atualize credentialSafePrincipal", name)
		}
	}
	for _, name := range []string{"User", "Session", "Role", "UserRef"} {
		if obj, _, _ := types.LookupFieldOrMethod(c.principal.Type(), false, tp.pkg, name); obj == nil {
			t.Fatalf("Principal.%s não encontrado", name)
		}
	}
	return c
}

func (c *userIdentityChecker) principalMethod(f *types.Func) bool {
	recv := f.Type().(*types.Signature).Recv()
	if recv == nil {
		return false
	}
	rt := recv.Type()
	if p, ok := rt.(*types.Pointer); ok {
		rt = p.Elem()
	}
	named, ok := rt.(*types.Named)
	return ok && named.Obj() == c.principal
}

// reach percorre a expressão do handler e todas as funções do pacote que ela
// alcança (chamadas ou referenciadas) e devolve os pontos que leem a
// identidade do usuário.
func (c *userIdentityChecker) reach(root ast.Node) []string {
	seen := map[*types.Func]bool{}
	var queue []ast.Node
	var hits []string
	visit := func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		switch obj := c.tp.info.Uses[id].(type) {
		case *types.Var:
			if c.userFields[obj.Origin()] {
				hits = append(hits, c.tp.fset.Position(id.Pos()).String()+": Principal."+obj.Name())
			}
		case *types.Func:
			fn := obj.Origin()
			if c.principalMethod(fn) {
				if !credentialSafePrincipal[fn.Name()] {
					hits = append(hits, c.tp.fset.Position(id.Pos()).String()+": Principal."+fn.Name())
				}
				return true
			}
			if fd := c.decls[fn]; fd != nil && !seen[fn] {
				seen[fn] = true
				if fd.Body != nil {
					queue = append(queue, fd.Body)
				}
			}
		}
		return true
	}
	queue = append(queue, root)
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		ast.Inspect(n, visit)
	}
	sort.Strings(hits)
	return hits
}

// tenantRouteHandlers extrai de registerTenantRoutes as chamadas
// t(método, padrão, permissão, handler, opções...) e devolve a expressão do
// handler por "MÉTODO padrão".
func tenantRouteHandlers(t *testing.T, tp typedPackage) map[string]ast.Expr {
	t.Helper()
	out := map[string]ast.Expr{}
	for _, f := range tp.files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "registerTenantRoutes" || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 4 {
					return true
				}
				fun, ok := call.Fun.(*ast.Ident)
				if !ok || fun.Name != "t" {
					return true
				}
				method, pattern := tp.info.Types[call.Args[0]].Value, tp.info.Types[call.Args[1]].Value
				if method == nil || pattern == nil || method.Kind() != constant.String || pattern.Kind() != constant.String {
					t.Errorf("%s: método e padrão da rota precisam ser constantes", tp.fset.Position(call.Pos()))
					return true
				}
				out[constant.StringVal(method)+" "+constant.StringVal(pattern)] = call.Args[3]
				return true
			})
		}
	}
	return out
}

// Toda rota de tenant que aceita credencial de API passa pela decisão da
// ADR-059: grava a autoria por Principal.UserID() (nula para credenciais) e
// nunca lê a identidade do usuário (Principal.User, Session, Role,
// PlatformStaff ou UserRef), cujo valor é zero para credenciais: uuid.Nil na
// autoria (violação de FK ou autoria falsa) ou regras aplicadas a um papel
// vazio. As rotas cujo ato exige uma pessoa identificada declaram
// userSession() e recusam a credencial antes do handler.
func TestTenantRoutesDecideAuthorshipForAPICredentials(t *testing.T) {
	a, _ := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	tp := loadTypedPackage(t)
	checker := newUserIdentityChecker(t, tp)
	handlers := tenantRouteHandlers(t, tp)

	writes, credentialWrites := 0, 0
	for _, r := range a.Routes() {
		if r.Access != TenantScoped {
			continue
		}
		key := r.Method + " " + r.Pattern
		h, ok := handlers[key]
		if !ok {
			t.Errorf("%s: rota de tenant registrada fora de registerTenantRoutes; inclua-a nesta análise", key)
			continue
		}
		if !safeMethod(r.Method) {
			writes++
		}
		hits := checker.reach(h)
		if r.UserSession {
			continue
		}
		if !safeMethod(r.Method) {
			credentialWrites++
		}
		for _, hit := range hits {
			t.Errorf("%s aceita credencial de API, mas lê a identidade do usuário em %s; grave a autoria com p.UserID() "+
				"ou declare a rota com userSession() e registre a decisão na ADR-059", key, hit)
		}
	}
	if writes == 0 || credentialWrites == 0 {
		t.Fatalf("nenhuma rota de escrita analisada (%d, %d)", writes, credentialWrites)
	}

	// A análise reconhece a leitura da identidade do usuário nas rotas
	// restritas à sessão: o pedido de serviço adicional grava requested_by
	// (NOT NULL) a partir de Principal.User; o convite usa Principal.UserRef; a
	// mudança de papel aplica as regras pelo Principal.Role de quem executa.
	for key, want := range map[string]string{
		"POST /api/v1/platform/addon-orders": "Principal.User",
		"POST /api/v1/invitations":           "Principal.UserRef",
		"PATCH /api/v1/members/{user_id}":    "Principal.Role",
	} {
		if hits := checker.reach(handlers[key]); !containsHit(hits, want) {
			t.Errorf("a análise não encontrou %s em %s: %v", want, key, hits)
		}
	}
}

func containsHit(hits []string, want string) bool {
	for _, h := range hits {
		if strings.HasSuffix(h, ": "+want) {
			return true
		}
	}
	return false
}

// principalFixture reproduz o formato do Principal para exercitar a análise
// em handlers que leem cada parte da identidade do usuário, diretamente ou
// por uma função auxiliar.
const principalFixture = `package fixture

type User struct{ ID [16]byte }

type Session struct {
	ID     string
	UserID [16]byte
}

type Credential struct{ ID [16]byte }

type Principal struct {
	User          User
	Session       Session
	Credential    *Credential
	TenantID      [16]byte
	Role          string
	PlatformStaff bool
}

func (p Principal) Can(perm string) bool  { return p.Role == perm }
func (p Principal) Actor() string         { return p.Session.ID }
func (p Principal) UserID() *[16]byte     { return &p.User.ID }
func (p Principal) ClientID() string      { return p.Session.ID }
func (p Principal) UserRef() User         { return p.User }
func (p Principal) Email() string         { return "" }

func authorFromSession(p Principal) *[16]byte {
	uid := p.Session.UserID
	return &uid
}

func viaSession(p Principal) *[16]byte    { uid := p.Session.UserID; return &uid }
func viaHelper(p Principal) *[16]byte     { return authorFromSession(p) }
func viaRole(p Principal) bool            { return p.Role == "owner" }
func viaUser(p Principal) [16]byte        { return p.User.ID }
func viaUserRef(p Principal) User         { return p.UserRef() }
func viaStaff(p Principal) bool           { return p.PlatformStaff }
func viaNewMethod(p Principal) string     { return p.Email() }
func viaCredentialSafe(p Principal) (*[16]byte, string, string, [16]byte, bool) {
	return p.UserID(), p.Actor(), p.ClientID(), p.TenantID, p.Credential != nil && p.Can("customers.read")
}
`

// A análise aponta toda leitura da identidade do usuário a partir do
// principal, e não apenas Principal.User e UserRef: um handler que tomasse a
// autoria de Principal.Session.UserID (uuid.Nil para credenciais) violaria a
// FK para users como antes da ADR-059, e um que aplicasse regras pelo
// Principal.Role trataria a credencial como um papel vazio. Membros novos do
// principal ficam fora do permitido até que se decida o contrário.
func TestUserIdentityCheckerFlagsEveryUserField(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", principalFixture, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	tp := checkFiles(t, fset, "fixture", []*ast.File{f}, nil)
	checker := newUserIdentityChecker(t, tp)
	funcs := map[string]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			funcs[fd.Name.Name] = fd
		}
	}
	cases := []struct {
		fn   string
		want []string
	}{
		{"viaSession", []string{"Principal.Session"}},
		{"viaHelper", []string{"Principal.Session"}},
		{"viaRole", []string{"Principal.Role"}},
		{"viaUser", []string{"Principal.User"}},
		{"viaUserRef", []string{"Principal.UserRef"}},
		{"viaStaff", []string{"Principal.PlatformStaff"}},
		{"viaNewMethod", []string{"Principal.Email"}},
		{"viaCredentialSafe", nil},
	}
	for _, tc := range cases {
		t.Run(tc.fn, func(t *testing.T) {
			hits := checker.reach(funcs[tc.fn].Body)
			if len(hits) != len(tc.want) {
				t.Fatalf("pontos encontrados: %v, esperado %v", hits, tc.want)
			}
			for _, w := range tc.want {
				if !containsHit(hits, w) {
					t.Fatalf("pontos encontrados: %v, esperado %v", hits, tc.want)
				}
			}
		})
	}
}

// Permissões não delegáveis (ADR-047) correspondem a atos que exigem uma
// pessoa identificada: as rotas que as exigem recusam credenciais com
// user_session_required, e não apenas pela falta do escopo.
func TestNonDelegableRoutesRequireUserSession(t *testing.T) {
	a, _ := NewRouter(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	for _, r := range a.Routes() {
		key := r.Method + " " + r.Pattern
		if r.Access == Authenticated && !r.UserSession {
			t.Errorf("%s: rota de conta sem exigência de sessão de usuário", key)
		}
		if r.Access == TenantScoped && !apicredentials.Delegable(r.Permission) && !r.UserSession {
			t.Errorf("%s exige %s, que não é delegável, mas não declara userSession()", key, r.Permission)
		}
	}
}

// A opção userSession() recusa a credencial de API com 403
// user_session_required antes da verificação de escopos; sem ela, a
// credencial com o escopo chega ao handler. A sessão do usuário passa nas
// duas.
func TestUserSessionOptionOnTenantRoutes(t *testing.T) {
	a := &API{Deps: Deps{Clock: clock.System{}}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := chi.NewRouter()
	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	a.handle(r, http.MethodPost, "/humano", TenantScoped, tenancy.PermMembersRead, reached, userSession())
	a.handle(r, http.MethodPost, "/integracao", TenantScoped, tenancy.PermMembersRead, reached)
	a.handle(r, http.MethodGet, "/conta", Authenticated, "", reached)
	want := map[string]bool{"/humano": true, "/integracao": false, "/conta": true}
	for _, info := range a.Routes() {
		if info.UserSession != want[info.Pattern] {
			t.Errorf("%s: UserSession = %v", info.Pattern, info.UserSession)
		}
	}

	tenantID := uuid.New()
	scoped := Principal{TenantID: tenantID, Credential: &apicredentials.Principal{CredentialID: uuid.New(), TenantID: tenantID,
		Scopes: []tenancy.Permission{tenancy.PermMembersRead}}}
	unscoped := Principal{TenantID: tenantID, Credential: &apicredentials.Principal{CredentialID: uuid.New(), TenantID: tenantID}}
	user := Principal{TenantID: tenantID, User: identity.User{ID: uuid.New()}, Role: tenancy.RoleAuditor}
	cases := []struct {
		name   string
		method string
		path   string
		p      Principal
		status int
		code   string
	}{
		{"credencial com escopo em rota humana", http.MethodPost, "/humano", scoped, http.StatusForbidden, "user_session_required"},
		{"credencial sem escopo em rota humana", http.MethodPost, "/humano", unscoped, http.StatusForbidden, "user_session_required"},
		{"credencial em rota de conta", http.MethodGet, "/conta", scoped, http.StatusForbidden, "user_session_required"},
		{"credencial com escopo em rota de integração", http.MethodPost, "/integracao", scoped, http.StatusNoContent, ""},
		{"credencial sem escopo em rota de integração", http.MethodPost, "/integracao", unscoped, http.StatusForbidden, "forbidden"},
		{"usuário em rota humana", http.MethodPost, "/humano", user, http.StatusNoContent, ""},
		{"usuário em rota de integração", http.MethodPost, "/integracao", user, http.StatusNoContent, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req = req.WithContext(withPrincipal(req.Context(), tc.p))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.status || (tc.code != "" && !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`)) {
				t.Fatalf("%d %s, esperado %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
		})
	}
}
