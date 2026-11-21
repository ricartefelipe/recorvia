package customers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

type svcEnv struct {
	store    *memStore
	clock    *clock.Manual
	svc      *Service
	tenantID uuid.UUID
	actor    audit.Actor
}

func newSvcEnv() *svcEnv {
	store := newMemStore()
	c := clock.NewManual(testNow)
	return &svcEnv{store: store, clock: c, svc: NewService(store, c), tenantID: uuid.New(), actor: audit.User(uuid.New(), "Operadora")}
}

func (e *svcEnv) st() *memState { return e.store.state(e.tenantID) }

// seed grava um cliente ativo diretamente no armazenamento.
func (e *svcEnv) seed(t *testing.T, in Input) Customer {
	t.Helper()
	c, err := New(uuid.New(), e.tenantID, in, testNow)
	if err != nil {
		t.Fatal(err)
	}
	e.st().customers[c.ID] = c
	return c
}

func (e *svcEnv) seedContacts(customerID uuid.UUID, n int) {
	for i := range n {
		e.st().contacts[customerID] = append(e.st().contacts[customerID],
			Contact{ID: uuid.New(), CustomerID: customerID, Name: fmt.Sprintf("Contato %d", i), Email: fmt.Sprintf("c%d@acme.com", i)})
	}
}

// containsPersonalData informa se algum valor pessoal do cliente aparece em v.
func containsPersonalData(v any, c Customer) bool {
	s := fmt.Sprint(v)
	for _, p := range []string{c.Name, c.Email, c.DocumentNumber, c.Phone} {
		if p != "" && strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func TestCreate(t *testing.T) {
	e := newSvcEnv()
	c, err := e.svc.Create(context.Background(), e.tenantID, e.actor, companyInput())
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == uuid.Nil || c.TenantID != e.tenantID || c.DocumentNumber != validCNPJ || !c.CreatedAt.Equal(testNow) {
		t.Fatalf("cliente inesperado: %+v", c)
	}
	st := e.st()
	if _, ok := st.customers[c.ID]; !ok {
		t.Fatal("cliente não persistido")
	}
	if len(st.events) != 1 || st.events[0].Type != outbox.EventCustomerCreated || st.events[0].AggregateID != c.ID {
		t.Fatalf("evento inesperado: %+v", st.events)
	}
	if len(st.audits) != 1 || st.audits[0].Action != "customer.created" || st.audits[0].Details["kind"] != Company ||
		st.audits[0].Actor != e.actor {
		t.Fatalf("auditoria inesperada: %+v", st.audits)
	}
	if containsPersonalData(st.audits[0].Details, c) || containsPersonalData(st.events[0].Payload, c) {
		t.Fatal("auditoria e eventos não podem conter dados pessoais")
	}
}

func TestCreateRejected(t *testing.T) {
	e := newSvcEnv()
	in := individualInput()
	in.DocumentNumber = "000"
	_, err := e.svc.Create(context.Background(), e.tenantID, e.actor, in)
	assertAppErr(t, err, apperr.KindInvalid, "invalid_customer")
	if e.store.calls["InTenant"] != 0 {
		t.Fatal("validação deve ocorrer antes da transação")
	}
	for _, op := range []string{"Insert", "Emit", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			e.store.fail[op] = errBoom
			if _, err := e.svc.Create(context.Background(), e.tenantID, e.actor, individualInput()); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if len(e.st().customers) != 0 || len(e.st().events) != 0 {
				t.Fatal("nada deveria ser persistido")
			}
		})
	}
}

func TestGet(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.seedContacts(c.ID, 2)

	d, err := e.svc.Get(context.Background(), e.tenantID, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != c.ID || len(d.Contacts) != 2 || d.Address != nil {
		t.Fatalf("detalhes inesperados: %+v", d)
	}

	e.st().addresses[c.ID] = validAddress()
	d, err = e.svc.Get(context.Background(), e.tenantID, c.ID)
	if err != nil || d.Address == nil || d.Address.City != "São Paulo" {
		t.Fatalf("endereço deveria ser incluído: %+v, %v", d.Address, err)
	}

	_, err = e.svc.Get(context.Background(), e.tenantID, uuid.New())
	assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")

	for _, op := range []string{"Contacts", "Address"} {
		e.store.fail = map[string]error{op: errBoom}
		if _, err := e.svc.Get(context.Background(), e.tenantID, c.ID); !errors.Is(err, errBoom) {
			t.Errorf("falha em %s deveria ser propagada: %v", op, err)
		}
	}
}

func TestList(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.st().page = Page{Items: []Customer{c}, NextCursor: "proximo"}
	f := ListFilter{Status: Active, Search: strings.Repeat("ç", 100), Sort: "-created_at", Limit: 50, Cursor: "abc"}

	p, err := e.svc.List(context.Background(), e.tenantID, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 1 || p.NextCursor != "proximo" {
		t.Fatalf("página inesperada: %+v", p)
	}
	if e.st().lastFilter != f {
		t.Fatalf("filtro repassado com alterações: %+v", e.st().lastFilter)
	}

	e.store.fail["List"] = errBoom
	if _, err := e.svc.List(context.Background(), e.tenantID, ListFilter{}); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
}

func TestListRejected(t *testing.T) {
	cases := []struct {
		name   string
		filter ListFilter
		code   string
		field  string
	}{
		{"ordenação por e-mail", ListFilter{Sort: "email"}, "invalid_sort", "sort"},
		{"ordenação com SQL", ListFilter{Sort: "name; DROP TABLE customers"}, "invalid_sort", "sort"},
		{"situação desconhecida", ListFilter{Status: "deleted"}, "invalid_filter", "status"},
		{"busca acima de 100 caracteres", ListFilter{Search: strings.Repeat("a", 101)}, "invalid_filter", "q"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSvcEnv()
			_, err := e.svc.List(context.Background(), e.tenantID, tc.filter)
			fields := assertAppErr(t, err, apperr.KindInvalid, tc.code)
			if names := fields; !slices.Equal(names, []string{tc.field}) {
				t.Fatalf("campos = %v, esperado [%s]", names, tc.field)
			}
			if e.store.calls["InTenant"] != 0 {
				t.Fatal("filtro inválido não deve chegar ao banco")
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, individualInput())
	in := individualInput()
	in.Name, in.Phone = "Ana Lima", "11987654321"

	got, err := e.svc.Update(context.Background(), e.tenantID, e.actor, c.ID, in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ana Lima" || got.Phone != "11987654321" || got.Version != 2 {
		t.Fatalf("cliente inesperado: %+v", got)
	}
	if stored := e.st().customers[c.ID]; stored.Version != 2 || stored.Name != "Ana Lima" {
		t.Fatalf("alteração não persistida: %+v", stored)
	}
	st := e.st()
	if len(st.events) != 1 || st.events[0].Type != outbox.EventCustomerUpdated {
		t.Fatalf("evento inesperado: %+v", st.events)
	}
	payload, _ := st.events[0].Payload.(map[string]any)
	if fields, _ := payload["fields"].([]string); !slices.Equal(fields, []string{"name", "phone"}) {
		t.Fatalf("evento deve listar os campos alterados: %+v", payload)
	}
	if len(st.audits) != 1 || st.audits[0].Action != "customer.updated" {
		t.Fatalf("auditoria inesperada: %+v", st.audits)
	}
	if fields, _ := st.audits[0].Details["fields"].([]string); !slices.Equal(fields, []string{"name", "phone"}) {
		t.Fatalf("auditoria deve listar os campos alterados: %+v", st.audits[0].Details)
	}
	if containsPersonalData(st.audits[0].Details, got) || containsPersonalData(payload, got) {
		t.Fatal("auditoria e eventos não podem conter valores pessoais")
	}
}

func TestUpdateWithoutChanges(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, individualInput())
	got, err := e.svc.Update(context.Background(), e.tenantID, e.actor, c.ID, individualInput(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || e.store.calls["Update"] != 0 || len(e.st().events) != 0 || len(e.st().audits) != 0 {
		t.Fatalf("sem alterações não deve gravar, emitir nem auditar: %+v", got)
	}
}

func TestUpdateRejected(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*svcEnv, Customer) (uuid.UUID, Input, int)
		kind    apperr.Kind
		code    string
		errFrom error
	}{
		{"versão divergente", func(_ *svcEnv, c Customer) (uuid.UUID, Input, int) {
			in := individualInput()
			in.Name = "Outra"
			return c.ID, in, 3
		}, apperr.KindPreconditionFailed, "version_mismatch", nil},
		{"cliente arquivado", func(e *svcEnv, c Customer) (uuid.UUID, Input, int) {
			c.Status = Archived
			e.st().customers[c.ID] = c
			in := individualInput()
			in.Name = "Outra"
			return c.ID, in, 1
		}, apperr.KindConflict, "customer_archived", nil},
		{"dados inválidos", func(_ *svcEnv, c Customer) (uuid.UUID, Input, int) {
			in := individualInput()
			in.Email = "invalido"
			return c.ID, in, 1
		}, apperr.KindInvalid, "invalid_customer", nil},
		{"cliente inexistente", func(*svcEnv, Customer) (uuid.UUID, Input, int) {
			return uuid.New(), individualInput(), 1
		}, apperr.KindNotFound, "customer_not_found", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, individualInput())
			id, in, version := tc.setup(e, c)
			before := e.st().customers[c.ID]
			_, err := e.svc.Update(context.Background(), e.tenantID, e.actor, id, in, version)
			assertAppErr(t, err, tc.kind, tc.code)
			if e.st().customers[c.ID] != before || len(e.st().events) != 0 {
				t.Fatal("cliente não pode ser alterado")
			}
		})
	}
	for _, op := range []string{"Get", "Update", "Emit", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, individualInput())
			e.store.fail[op] = errBoom
			in := individualInput()
			in.Name = "Ana Lima"
			if _, err := e.svc.Update(context.Background(), e.tenantID, e.actor, c.ID, in, 1); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.st().customers[c.ID].Name != "Ana Souza" {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

func TestArchive(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	got, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Archived || got.Version != 2 || e.st().customers[c.ID].Status != Archived {
		t.Fatalf("cliente deveria ser arquivado: %+v", got)
	}
	if len(e.st().audits) != 1 || e.st().audits[0].Action != "customer.archived" || e.st().audits[0].ResourceID != c.ID.String() {
		t.Fatalf("auditoria inesperada: %+v", e.st().audits)
	}

	// Arquivar novamente não tem efeito.
	again, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 2)
	if err != nil || again.Version != 2 || len(e.st().audits) != 1 || e.store.calls["HasOpenObligations"] != 1 {
		t.Fatalf("segundo arquivamento deveria ser neutro: %+v, %v", again, err)
	}
	// Já arquivado, a versão do If-Match ainda é conferida.
	_, err = e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 1)
	assertAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
}

// A versão do If-Match vai na condição da gravação: divergente, o cliente
// não é arquivado nem auditado.
func TestArchiveVersionMismatch(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	_, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 5)
	assertAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if e.store.calls["Update"] != 1 || e.st().customers[c.ID].Status != Active || len(e.st().audits) != 0 {
		t.Fatal("cliente com versão divergente não pode ser arquivado")
	}
}

func TestArchiveWithOpenObligations(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.st().obligations[c.ID] = true
	_, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 1)
	assertAppErr(t, err, apperr.KindConflict, "customer_has_obligations")
	if e.st().customers[c.ID].Status != Active || len(e.st().audits) != 0 {
		t.Fatal("cliente com obrigações em aberto não pode ser arquivado")
	}
}

func TestArchiveErrors(t *testing.T) {
	for _, op := range []string{"Get", "HasOpenObligations", "Update", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.store.fail[op] = errBoom
			if _, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID, 1); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.st().customers[c.ID].Status != Active {
				t.Fatal("arquivamento deveria ser desfeito")
			}
		})
	}
}

func TestAddContact(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	ct, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, c.ID,
		ContactInput{Name: "Carlos", Email: "carlos@acme.com", ReceivesBillingEmail: true})
	if err != nil {
		t.Fatal(err)
	}
	if ct.CustomerID != c.ID || ct.ID == uuid.Nil || !ct.CreatedAt.Equal(testNow) {
		t.Fatalf("contato inesperado: %+v", ct)
	}
	if got := e.st().contacts[c.ID]; len(got) != 1 || got[0].ID != ct.ID {
		t.Fatalf("contato não persistido: %+v", got)
	}
	a := e.st().audits[0]
	if a.Action != "customer.contact_added" || a.Details["contact_id"] != ct.ID || a.Details["receives_billing_email"] != true {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	if strings.Contains(fmt.Sprint(a.Details), "carlos") {
		t.Fatal("auditoria não pode conter dados do contato")
	}
}

func TestAddContactLimit(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.seedContacts(c.ID, 19)
	if _, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, c.ID, ContactInput{Name: "Vigésimo", Email: "v@acme.com"}); err != nil {
		t.Fatalf("o 20º contato deveria ser aceito: %v", err)
	}
	_, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, c.ID, ContactInput{Name: "Excedente", Email: "x@acme.com"})
	assertAppErr(t, err, apperr.KindConflict, "contact_limit")
	if n := len(e.st().contacts[c.ID]); n != 20 {
		t.Fatalf("cliente deveria manter 20 contatos, tem %d", n)
	}
}

func TestAddContactRejected(t *testing.T) {
	t.Run("contato inválido", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, companyInput())
		_, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, c.ID, ContactInput{Name: "Sem e-mail"})
		assertAppErr(t, err, apperr.KindInvalid, "invalid_contact")
		if e.store.calls["InTenant"] != 0 {
			t.Fatal("validação deve ocorrer antes da transação")
		}
	})
	t.Run("cliente inexistente", func(t *testing.T) {
		e := newSvcEnv()
		_, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, uuid.New(), ContactInput{Name: "C", Email: "c@acme.com"})
		assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")
	})
	for _, op := range []string{"Contacts", "InsertContact", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.store.fail[op] = errBoom
			if _, err := e.svc.AddContact(context.Background(), e.tenantID, e.actor, c.ID, ContactInput{Name: "C", Email: "c@acme.com"}); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if len(e.st().contacts[c.ID]) != 0 {
				t.Fatal("contato deveria ser desfeito")
			}
		})
	}
}

func TestRemoveContact(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.seedContacts(c.ID, 2)
	target := e.st().contacts[c.ID][0].ID

	if err := e.svc.RemoveContact(context.Background(), e.tenantID, e.actor, c.ID, target); err != nil {
		t.Fatal(err)
	}
	if got := e.st().contacts[c.ID]; len(got) != 1 || got[0].ID == target {
		t.Fatalf("contato não removido: %+v", got)
	}
	if a := e.st().audits[0]; a.Action != "customer.contact_removed" || a.Details["contact_id"] != target {
		t.Fatalf("auditoria inesperada: %+v", a)
	}

	err := e.svc.RemoveContact(context.Background(), e.tenantID, e.actor, c.ID, uuid.New())
	assertAppErr(t, err, apperr.KindNotFound, "contact_not_found")

	e.store.fail["Audit"] = errBoom
	remaining := e.st().contacts[c.ID][0].ID
	if err := e.svc.RemoveContact(context.Background(), e.tenantID, e.actor, c.ID, remaining); !errors.Is(err, errBoom) {
		t.Fatalf("esperado erro propagado, obtido %v", err)
	}
	if len(e.st().contacts[c.ID]) != 1 {
		t.Fatal("remoção deveria ser desfeita")
	}
}

func TestSetAddress(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	in := validAddress()
	in.PostalCode, in.State = "01310-100", "sp"

	got, version, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, in, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.PostalCode != "01310100" || got.State != "SP" || got.Country != "BR" {
		t.Fatalf("endereço não normalizado: %+v", got)
	}
	// O endereço faz parte da ficha: a gravação gera a versão seguinte do cliente.
	if version != 2 || e.st().customers[c.ID].Version != 2 {
		t.Fatalf("versão do cliente depois do endereço: devolvida %d, gravada %d", version, e.st().customers[c.ID].Version)
	}
	if stored := e.st().addresses[c.ID]; stored != got {
		t.Fatalf("endereço persistido difere do devolvido: %+v", stored)
	}
	a := e.st().audits[0]
	all := []string{"street", "number", "district", "city", "state", "postal_code", "country"}
	if a.Action != "customer.address_updated" || !slices.Equal(a.Details["fields"].([]string), all) || a.Details["created"] != true {
		t.Fatalf("auditoria do cadastro do endereço: %+v", a)
	}

	// Alteração: somente os nomes dos campos que mudaram, nunca os valores.
	in.City, in.Complement = "Campinas", "Sala 5"
	if _, _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, in, 2); err != nil {
		t.Fatal(err)
	}
	a = e.st().audits[1]
	if !slices.Equal(a.Details["fields"].([]string), []string{"complement", "city"}) || a.Details["created"] != false {
		t.Fatalf("auditoria da alteração do endereço: %+v", a)
	}
	if strings.Contains(fmt.Sprint(a.Details), "Campinas") || strings.Contains(fmt.Sprint(a.Details), "Sala 5") {
		t.Fatalf("auditoria não pode conter valores do endereço: %+v", a.Details)
	}

	// Sem mudança (inclusive depois da normalização), nada é gravado nem auditado.
	upserts := e.store.calls["UpsertAddress"]
	in.PostalCode = "01310100"
	if _, version, err = e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, in, 3); err != nil || version != 3 {
		t.Fatalf("endereço igual: versão %d, %v", version, err)
	}
	if len(e.st().audits) != 2 || e.store.calls["UpsertAddress"] != upserts || e.st().customers[c.ID].Version != 3 {
		t.Fatalf("endereço igual não deveria ser regravado nem auditado: %d auditorias", len(e.st().audits))
	}
	// Mesmo sem mudança, a versão do If-Match é conferida.
	_, _, err = e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, in, 2)
	assertAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
}

// A versão do If-Match vai na condição da gravação da nova versão do cliente:
// divergente, o endereço não é gravado nem auditado.
func TestSetAddressVersionMismatch(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	_, _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, validAddress(), 4)
	assertAppErr(t, err, apperr.KindPreconditionFailed, "version_mismatch")
	if _, ok := e.st().addresses[c.ID]; ok || e.store.calls["BumpVersion"] != 1 || len(e.st().audits) != 0 {
		t.Fatal("endereço com versão divergente não pode ser gravado")
	}
}

func TestAddressChangedFields(t *testing.T) {
	base := Address{Street: "Rua A", Number: "1", District: "Centro", City: "São Paulo", State: "SP", PostalCode: "01310100", Country: "BR"}
	cases := []struct {
		name string
		prev *Address
		next func(Address) Address
		want []string
	}{
		{"sem endereço anterior", nil, func(a Address) Address { return a },
			[]string{"street", "number", "district", "city", "state", "postal_code", "country"}},
		{"igual", &base, func(a Address) Address { return a }, nil},
		{"rua e número", &base, func(a Address) Address { a.Street, a.Number = "Rua B", "2"; return a }, []string{"street", "number"}},
		{"bairro, UF e CEP", &base, func(a Address) Address { a.District, a.State, a.PostalCode = "Sé", "RJ", "20000000"; return a },
			[]string{"district", "state", "postal_code"}},
		{"país", &base, func(a Address) Address { a.Country = "PT"; return a }, []string{"country"}},
		{"complemento removido", &Address{Street: "Rua A", Number: "1", Complement: "Fundos", District: "Centro", City: "São Paulo",
			State: "SP", PostalCode: "01310100", Country: "BR"}, func(a Address) Address { return a }, []string{"complement"}},
	}
	for _, tc := range cases {
		if got := tc.next(base).ChangedFields(tc.prev); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, esperado %v", tc.name, got, tc.want)
		}
	}
}

func TestUpdateContact(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.seedContacts(c.ID, 2)
	target := e.st().contacts[c.ID][1]
	ct, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, target.ID,
		ContactPatch{Name: ptr(" Carla Souza "), Email: ptr("Carla@ACME.com"), Phone: ptr("(11) 99999-0000"), Role: ptr("Financeiro"),
			ReceivesBillingEmail: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if ct.ID != target.ID || ct.CustomerID != c.ID || ct.Name != "Carla Souza" || ct.Email != "carla@acme.com" || ct.Phone != "11999990000" ||
		ct.Role != "Financeiro" || !ct.ReceivesBillingEmail || !ct.CreatedAt.Equal(target.CreatedAt) {
		t.Fatalf("contato alterado: %+v", ct)
	}
	if stored := e.st().contacts[c.ID]; stored[1] != ct || stored[0].ID == ct.ID || stored[0].Name != "Contato 0" {
		t.Fatalf("somente o contato alterado deveria mudar: %+v", stored)
	}
	a := e.st().audits[0]
	want := []string{"name", "email", "phone", "role", "receives_billing_email"}
	if a.Action != "customer.contact_updated" || a.ResourceID != c.ID.String() || a.Details["contact_id"] != ct.ID ||
		!slices.Equal(a.Details["fields"].([]string), want) {
		t.Fatalf("auditoria inesperada: %+v", a)
	}
	if s := fmt.Sprint(a.Details); strings.Contains(s, "carla") || strings.Contains(s, "Carla") || strings.Contains(s, "99999") {
		t.Fatalf("auditoria não pode conter dados do contato: %v", s)
	}

	// Sem mudança, nada é gravado nem auditado.
	if _, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, target.ID,
		ContactPatch{Name: ptr("Carla Souza"), Email: ptr("carla@acme.com"), Phone: ptr("11999990000"), Role: ptr("Financeiro"),
			ReceivesBillingEmail: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	if len(e.st().audits) != 1 || e.store.calls["UpdateContact"] != 1 {
		t.Fatalf("contato igual não deveria ser regravado: %d auditorias, %d gravações", len(e.st().audits), e.store.calls["UpdateContact"])
	}
	// Alteração vazia não muda nada.
	if got, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, target.ID, ContactPatch{}); err != nil || got != ct {
		t.Fatalf("alteração vazia: %+v %v", got, err)
	}
}

// Campos omitidos na alteração mantêm o valor atual: a preferência de e-mail
// de cobrança desligada (por exemplo, pelo cliente no portal) não volta ao
// padrão quando só o cargo muda.
func TestUpdateContactKeepsOmittedFields(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	current := Contact{ID: uuid.New(), CustomerID: c.ID, Name: "Ana", Email: "ana@acme.com", Phone: "11999990000", Role: "Compras",
		ReceivesBillingEmail: false, CreatedAt: testNow}
	e.st().contacts[c.ID] = []Contact{current}
	got, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, current.ID, ContactPatch{Role: ptr("Financeiro")})
	if err != nil {
		t.Fatal(err)
	}
	want := current
	want.Role = "Financeiro"
	if got != want || e.st().contacts[c.ID][0] != want {
		t.Fatalf("somente o cargo deveria mudar:\n%+v\n%+v", got, want)
	}
	if f := e.st().audits[0].Details["fields"].([]string); !slices.Equal(f, []string{"role"}) {
		t.Fatalf("campos auditados: %v", f)
	}
	// Telefone informado vazio remove o telefone.
	got, err = e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, current.ID, ContactPatch{Phone: ptr("")})
	if err != nil || got.Phone != "" || got.ReceivesBillingEmail || got.Role != "Financeiro" {
		t.Fatalf("remoção do telefone: %+v %v", got, err)
	}
}

func TestUpdateContactRejected(t *testing.T) {
	valid := ContactPatch{Name: ptr("Nome"), Email: ptr("nome@acme.com")}
	cases := []struct {
		name  string
		setup func(e *svcEnv, c Customer) (customerID, contactID uuid.UUID)
		in    ContactPatch
		kind  apperr.Kind
		code  string
	}{
		{"contato inválido", func(e *svcEnv, c Customer) (uuid.UUID, uuid.UUID) { return c.ID, e.st().contacts[c.ID][0].ID },
			ContactPatch{Email: ptr("sem-arroba")}, apperr.KindInvalid, "invalid_contact"},
		{"contato inexistente", func(_ *svcEnv, c Customer) (uuid.UUID, uuid.UUID) { return c.ID, uuid.New() }, valid, apperr.KindNotFound,
			"contact_not_found"},
		{"contato de outro cliente", func(e *svcEnv, c Customer) (uuid.UUID, uuid.UUID) {
			other := e.seed(t, companyInput())
			return other.ID, e.st().contacts[c.ID][0].ID
		}, valid, apperr.KindNotFound, "contact_not_found"},
		{"cliente inexistente", func(e *svcEnv, c Customer) (uuid.UUID, uuid.UUID) { return uuid.New(), e.st().contacts[c.ID][0].ID }, valid,
			apperr.KindNotFound, "customer_not_found"},
		{"cliente anonimizado", func(e *svcEnv, c Customer) (uuid.UUID, uuid.UUID) {
			at := testNow
			c.AnonymizedAt = &at
			e.st().customers[c.ID] = c
			return c.ID, e.st().contacts[c.ID][0].ID
		}, valid, apperr.KindConflict, "customer_anonymized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.seedContacts(c.ID, 1)
			before := e.st().contacts[c.ID][0]
			customerID, contactID := tc.setup(e, c)
			_, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, customerID, contactID, tc.in)
			assertAppErr(t, err, tc.kind, tc.code)
			if e.st().contacts[c.ID][0] != before || len(e.st().audits) != 0 {
				t.Fatal("contato não deveria mudar")
			}
		})
	}
	for _, op := range []string{"Get", "Contacts", "UpdateContact", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.seedContacts(c.ID, 1)
			before := e.st().contacts[c.ID][0]
			e.store.fail[op] = errBoom
			if _, err := e.svc.UpdateContact(context.Background(), e.tenantID, e.actor, c.ID, before.ID, valid); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if e.st().contacts[c.ID][0] != before {
				t.Fatal("alteração deveria ser desfeita")
			}
		})
	}
}

func TestSetAddressRejected(t *testing.T) {
	t.Run("endereço inválido", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, companyInput())
		_, _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, Address{}, 1)
		assertAppErr(t, err, apperr.KindInvalid, "invalid_address")
		if e.store.calls["InTenant"] != 0 {
			t.Fatal("validação deve ocorrer antes da transação")
		}
	})
	t.Run("cliente inexistente", func(t *testing.T) {
		e := newSvcEnv()
		_, _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, uuid.New(), validAddress(), 1)
		assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")
	})
	for _, op := range []string{"Address", "BumpVersion", "UpsertAddress", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.store.fail[op] = errBoom
			if _, _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, validAddress(), 1); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if _, ok := e.st().addresses[c.ID]; ok {
				t.Fatal("endereço deveria ser desfeito")
			}
		})
	}
}
