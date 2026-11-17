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
	got, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID)
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
	again, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID)
	if err != nil || again.Version != 2 || len(e.st().audits) != 1 || e.store.calls["HasOpenObligations"] != 1 {
		t.Fatalf("segundo arquivamento deveria ser neutro: %+v, %v", again, err)
	}
}

func TestArchiveWithOpenObligations(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	e.st().obligations[c.ID] = true
	_, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID)
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
			if _, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID); !errors.Is(err, errBoom) {
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

	got, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.PostalCode != "01310100" || got.State != "SP" || got.Country != "BR" {
		t.Fatalf("endereço não normalizado: %+v", got)
	}
	if stored := e.st().addresses[c.ID]; stored != got {
		t.Fatalf("endereço persistido difere do devolvido: %+v", stored)
	}
	if a := e.st().audits[0]; a.Action != "customer.address_updated" || len(a.Details) != 0 {
		t.Fatalf("auditoria inesperada (sem dados do endereço): %+v", a)
	}
}

func TestSetAddressRejected(t *testing.T) {
	t.Run("endereço inválido", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, companyInput())
		_, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, Address{})
		assertAppErr(t, err, apperr.KindInvalid, "invalid_address")
		if e.store.calls["InTenant"] != 0 {
			t.Fatal("validação deve ocorrer antes da transação")
		}
	})
	t.Run("cliente inexistente", func(t *testing.T) {
		e := newSvcEnv()
		_, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, uuid.New(), validAddress())
		assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")
	})
	for _, op := range []string{"UpsertAddress", "Audit"} {
		t.Run("falha em "+op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, companyInput())
			e.store.fail[op] = errBoom
			if _, err := e.svc.SetAddress(context.Background(), e.tenantID, e.actor, c.ID, validAddress()); !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if _, ok := e.st().addresses[c.ID]; ok {
				t.Fatal("endereço deveria ser desfeito")
			}
		})
	}
}
