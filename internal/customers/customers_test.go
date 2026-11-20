package customers

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
)

const (
	validCPF   = "52998224725"
	otherCPF   = "11144477735"
	validCNPJ  = "11222333000181"
	otherCNPJ  = "11444777000161"
	testTenant = "6f1c2a7e-0000-7000-8000-000000000001"
)

var testNow = time.Date(2026, time.November, 16, 15, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// assertAppErr verifica categoria e código do erro e devolve os campos
// inválidos informados.
func assertAppErr(t testing.TB, err error, kind apperr.Kind, code string) []string {
	t.Helper()
	e, ok := apperr.As(err)
	if !ok {
		t.Fatalf("esperado *apperr.Error %s/%s, obtido %v", kind, code, err)
	}
	if e.Kind != kind || e.Code != code {
		t.Fatalf("erro = %s/%s, esperado %s/%s", e.Kind, e.Code, kind, code)
	}
	return fieldNames(e)
}

func fieldNames(e *apperr.Error) []string {
	out := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		out = append(out, f.Field)
	}
	return out
}

func individualInput() Input {
	return Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: "529.982.247-25", Email: "ana@example.com"}
}

func companyInput() Input {
	return Input{Kind: Company, Name: "Acme", LegalName: "Acme Software Ltda", DocumentNumber: "11.222.333/0001-81"}
}

func TestNewCustomer(t *testing.T) {
	id, tenantID := uuid.New(), uuid.MustParse(testTenant)
	cases := []struct {
		name     string
		in       Input
		wantType brdoc.Kind
		wantDoc  string
	}{
		{"pessoa física com CPF formatado", individualInput(), brdoc.CPF, validCPF},
		{"pessoa jurídica com CNPJ formatado", companyInput(), brdoc.CNPJ, validCNPJ},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(id, tenantID, tc.in, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if c.DocumentType != tc.wantType || c.DocumentNumber != tc.wantDoc {
				t.Fatalf("documento = %s %s, esperado %s %s", c.DocumentType, c.DocumentNumber, tc.wantType, tc.wantDoc)
			}
			if c.ID != id || c.TenantID != tenantID || c.Kind != tc.in.Kind || c.Status != Active || c.Version != 1 ||
				!c.CreatedAt.Equal(testNow) || !c.UpdatedAt.Equal(testNow) {
				t.Fatalf("cliente inesperado: %+v", c)
			}
		})
	}
}

func TestNewCustomerNormalizes(t *testing.T) {
	in := Input{Kind: Company, Name: "  Acme  ", LegalName: " Acme Ltda ", DocumentNumber: validCNPJ,
		Email: " Financeiro@ACME.com.br ", Phone: "+55 (11) 98765-4321", ExternalID: "  erp:CLI-001_a.b  ", PaymentTermsDays: ptr(30)}
	c, err := New(uuid.New(), uuid.New(), in, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Acme" || c.LegalName != "Acme Ltda" || c.Email != "financeiro@acme.com.br" || c.Phone != "+5511987654321" ||
		c.ExternalID != "erp:CLI-001_a.b" || c.PaymentTermsDays == nil || *c.PaymentTermsDays != 30 {
		t.Fatalf("normalização inesperada: %+v", c)
	}
}

func TestNewCustomerOptionalFields(t *testing.T) {
	c, err := New(uuid.New(), uuid.New(), Input{Kind: Individual, Name: "Bia", DocumentNumber: validCPF}, testNow)
	if err != nil {
		t.Fatalf("e-mail, telefone, identificador externo e prazo são opcionais: %v", err)
	}
	if c.Email != "" || c.Phone != "" || c.ExternalID != "" || c.PaymentTermsDays != nil {
		t.Fatalf("campos opcionais deveriam ficar vazios: %+v", c)
	}
}

func TestNewCustomerValidation(t *testing.T) {
	cases := []struct {
		name   string
		base   func() Input
		mutate func(*Input)
		fields []string
	}{
		{"tipo desconhecido não valida documento", individualInput, func(in *Input) { in.Kind = "mei" }, []string{"kind"}},
		{"nome curto", individualInput, func(in *Input) { in.Name = " A " }, []string{"name"}},
		{"nome longo", individualInput, func(in *Input) { in.Name = strings.Repeat("é", 201) }, []string{"name"}},
		{"razão social longa", companyInput, func(in *Input) { in.LegalName = strings.Repeat("x", 201) }, []string{"legal_name"}},
		{"CPF ausente", individualInput, func(in *Input) { in.DocumentNumber = "" }, []string{"document_number"}},
		{"CPF com dígito errado", individualInput, func(in *Input) { in.DocumentNumber = "52998224724" }, []string{"document_number"}},
		{"CPF com dígitos repetidos", individualInput, func(in *Input) { in.DocumentNumber = "111.111.111-11" }, []string{"document_number"}},
		{"CNPJ para pessoa física", individualInput, func(in *Input) { in.DocumentNumber = validCNPJ }, []string{"document_number"}},
		{"CPF para pessoa jurídica", companyInput, func(in *Input) { in.DocumentNumber = validCPF }, []string{"document_number"}},
		{"CNPJ com dígito errado", companyInput, func(in *Input) { in.DocumentNumber = "11222333000182" }, []string{"document_number"}},
		{"e-mail inválido", individualInput, func(in *Input) { in.Email = "ana@" }, []string{"email"}},
		{"telefone curto", individualInput, func(in *Input) { in.Phone = "1234-567" }, []string{"phone"}},
		{"telefone longo", individualInput, func(in *Input) { in.Phone = "+1234567890123456" }, []string{"phone"}},
		{"telefone com letras", individualInput, func(in *Input) { in.Phone = "11 9876-ABCD" }, []string{"phone"}},
		{"identificador externo com espaço", individualInput, func(in *Input) { in.ExternalID = "cli 001" }, []string{"external_id"}},
		{"identificador externo longo", individualInput, func(in *Input) { in.ExternalID = strings.Repeat("a", 129) }, []string{"external_id"}},
		{"identificador externo com barra", individualInput, func(in *Input) { in.ExternalID = "erp/001" }, []string{"external_id"}},
		{"prazo negativo", individualInput, func(in *Input) { in.PaymentTermsDays = ptr(-1) }, []string{"payment_terms_days"}},
		{"prazo acima de 120 dias", individualInput, func(in *Input) { in.PaymentTermsDays = ptr(121) }, []string{"payment_terms_days"}},
		{"vários campos de uma vez", individualInput, func(in *Input) {
			in.Name, in.DocumentNumber, in.Email, in.Phone = "", "123", "x", "1"
		}, []string{"name", "document_number", "email", "phone"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.base()
			tc.mutate(&in)
			_, err := New(uuid.New(), uuid.New(), in, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_customer")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
		})
	}
}

func TestNewCustomerLimitsAccepted(t *testing.T) {
	in := individualInput()
	in.Name = strings.Repeat("é", 200)
	in.Phone = "12345678"
	in.ExternalID = strings.Repeat("a", 128)
	in.PaymentTermsDays = ptr(0)
	if _, err := New(uuid.New(), uuid.New(), in, testNow); err != nil {
		t.Fatalf("valores no limite inferior deveriam ser aceitos: %v", err)
	}
	in.Phone = "+123456789012345"
	in.PaymentTermsDays = ptr(120)
	if _, err := New(uuid.New(), uuid.New(), in, testNow); err != nil {
		t.Fatalf("valores no limite superior deveriam ser aceitos: %v", err)
	}
}

var auditableFields = []string{"kind", "name", "legal_name", "document_number", "email", "phone", "external_id", "payment_terms_days"}

func TestApplyReturnsOnlyChangedFields(t *testing.T) {
	base, err := New(uuid.New(), uuid.New(), Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: validCPF,
		Email: "ana@example.com", PaymentTermsDays: ptr(10)}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		in     Input
		want   []string
		verify func(*testing.T, Customer)
	}{
		{
			name: "mesmos dados após normalização",
			in:   Input{Kind: Individual, Name: " Ana Souza ", DocumentNumber: "529.982.247-25", Email: "ANA@example.com", PaymentTermsDays: ptr(10)},
		},
		{
			name: "nome e e-mail",
			in:   Input{Kind: Individual, Name: "Ana S. Lima", DocumentNumber: validCPF, Email: "ana.lima@example.com", PaymentTermsDays: ptr(10)},
			want: []string{"name", "email"},
			verify: func(t *testing.T, c Customer) {
				if c.Name != "Ana S. Lima" || c.Email != "ana.lima@example.com" {
					t.Fatalf("valores não aplicados: %+v", c)
				}
			},
		},
		{
			name: "troca de pessoa física para jurídica",
			in:   Input{Kind: Company, Name: "Ana Souza", DocumentNumber: validCNPJ, Email: "ana@example.com", PaymentTermsDays: ptr(10)},
			want: []string{"kind", "document_number"},
			verify: func(t *testing.T, c Customer) {
				if c.Kind != Company || c.DocumentType != brdoc.CNPJ || c.DocumentNumber != validCNPJ {
					t.Fatalf("tipo e documento não atualizados: %+v", c)
				}
			},
		},
		{
			name: "novo CPF",
			in:   Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: otherCPF, Email: "ana@example.com", PaymentTermsDays: ptr(10)},
			want: []string{"document_number"},
		},
		{
			name: "prazo removido",
			in:   Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: validCPF, Email: "ana@example.com"},
			want: []string{"payment_terms_days"},
			verify: func(t *testing.T, c Customer) {
				if c.PaymentTermsDays != nil {
					t.Fatal("prazo deveria ser removido")
				}
			},
		},
		{
			name: "prazo alterado",
			in:   Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: validCPF, Email: "ana@example.com", PaymentTermsDays: ptr(15)},
			want: []string{"payment_terms_days"},
		},
		{
			name: "telefone, identificador externo e razão social incluídos; e-mail removido",
			in: Input{Kind: Individual, Name: "Ana Souza", LegalName: "Ana Souza ME", DocumentNumber: validCPF,
				Phone: "11 98765 4321", ExternalID: "crm-9", PaymentTermsDays: ptr(10)},
			want: []string{"legal_name", "email", "phone", "external_id"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			changed, err := c.Apply(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(changed, tc.want) {
				t.Fatalf("campos alterados = %v, esperado %v", changed, tc.want)
			}
			for _, f := range changed {
				if !slices.Contains(auditableFields, f) {
					t.Fatalf("campo alterado deve ser um nome conhecido, sem valores pessoais: %q", f)
				}
			}
			if tc.verify != nil {
				tc.verify(t, c)
			}
		})
	}
}

func TestApplyInvalidKeepsCustomer(t *testing.T) {
	c, err := New(uuid.New(), uuid.New(), companyInput(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	before := c
	in := companyInput()
	in.DocumentNumber = otherCNPJ[:13]
	changed, err := c.Apply(in)
	assertAppErr(t, err, apperr.KindInvalid, "invalid_customer")
	if changed != nil || c != before {
		t.Fatalf("cliente não pode mudar com dados inválidos: %+v", c)
	}
}

func TestNewContact(t *testing.T) {
	id, customerID := uuid.New(), uuid.New()
	ct, err := NewContact(id, customerID, ContactInput{Name: " Carlos ", Email: " Carlos@Acme.com ", Phone: "(11) 3333-4444",
		Role: " Financeiro ", ReceivesBillingEmail: true}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := Contact{ID: id, CustomerID: customerID, Name: "Carlos", Email: "carlos@acme.com", Phone: "1133334444",
		Role: "Financeiro", ReceivesBillingEmail: true, CreatedAt: testNow}
	if ct != want {
		t.Fatalf("contato = %+v\nesperado %+v", ct, want)
	}
}

func TestNewContactValidation(t *testing.T) {
	valid := ContactInput{Name: "Carlos", Email: "carlos@acme.com"}
	cases := []struct {
		name   string
		mutate func(*ContactInput)
		fields []string
	}{
		{"nome vazio", func(in *ContactInput) { in.Name = "  " }, []string{"name"}},
		{"nome longo", func(in *ContactInput) { in.Name = strings.Repeat("a", 201) }, []string{"name"}},
		{"e-mail obrigatório", func(in *ContactInput) { in.Email = "" }, []string{"email"}},
		{"e-mail inválido", func(in *ContactInput) { in.Email = "carlos" }, []string{"email"}},
		{"telefone inválido", func(in *ContactInput) { in.Phone = "12" }, []string{"phone"}},
		{"cargo longo", func(in *ContactInput) { in.Role = strings.Repeat("c", 101) }, []string{"role"}},
		{"todos inválidos", func(in *ContactInput) { *in = ContactInput{Phone: "x", Role: strings.Repeat("c", 101)} },
			[]string{"name", "email", "phone", "role"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)
			_, err := NewContact(uuid.New(), uuid.New(), in, testNow)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_contact")
			if got := fields; !slices.Equal(got, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", got, tc.fields)
			}
		})
	}
}

func validAddress() Address {
	return Address{Street: "Av. Paulista", Number: "1000", District: "Bela Vista", City: "São Paulo", State: "SP", PostalCode: "01310100"}
}

func TestValidateAddress(t *testing.T) {
	a, err := ValidateAddress(Address{Street: " Av. Paulista ", Number: " 1000 ", Complement: " 10º andar ", District: " Bela Vista ",
		City: " São Paulo ", State: " sp ", PostalCode: "01310-100", Country: ""})
	if err != nil {
		t.Fatal(err)
	}
	want := Address{Street: "Av. Paulista", Number: "1000", Complement: "10º andar", District: "Bela Vista",
		City: "São Paulo", State: "SP", PostalCode: "01310100", Country: "BR"}
	if a != want {
		t.Fatalf("endereço = %+v\nesperado %+v", a, want)
	}

	other := validAddress()
	other.PostalCode, other.Country = "01.310 100", " pt "
	got, err := ValidateAddress(other)
	if err != nil || got.PostalCode != "01310100" || got.Country != "PT" {
		t.Fatalf("CEP com pontuação e país em minúsculas deveriam ser aceitos: %+v, %v", got, err)
	}
}

func TestValidateAddressRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Address)
		fields []string
	}{
		{"logradouro ausente", func(a *Address) { a.Street = " " }, []string{"street"}},
		{"número ausente", func(a *Address) { a.Number = "" }, []string{"number"}},
		{"número longo", func(a *Address) { a.Number = strings.Repeat("1", 21) }, []string{"number"}},
		{"bairro ausente", func(a *Address) { a.District = "" }, []string{"district"}},
		{"cidade longa", func(a *Address) { a.City = strings.Repeat("c", 101) }, []string{"city"}},
		{"complemento longo", func(a *Address) { a.Complement = strings.Repeat("c", 101) }, []string{"complement"}},
		{"UF por extenso", func(a *Address) { a.State = "São Paulo" }, []string{"state"}},
		{"UF com uma letra", func(a *Address) { a.State = "S" }, []string{"state"}},
		{"CEP com sete dígitos", func(a *Address) { a.PostalCode = "0131010" }, []string{"postal_code"}},
		{"CEP com letras", func(a *Address) { a.PostalCode = "0131010A" }, []string{"postal_code"}},
		{"país com três letras", func(a *Address) { a.Country = "BRA" }, []string{"country"}},
		{"endereço vazio", func(a *Address) { *a = Address{} }, []string{"street", "number", "district", "city", "state", "postal_code"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validAddress()
			tc.mutate(&a)
			got, err := ValidateAddress(a)
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_address")
			if got != (Address{}) {
				t.Errorf("endereço inválido não deve ser devolvido: %+v", got)
			}
			if names := fields; !slices.Equal(names, tc.fields) {
				t.Fatalf("campos = %v, esperado %v", names, tc.fields)
			}
		})
	}
}

func TestSortAllowed(t *testing.T) {
	for _, s := range []string{"", "name", "-name", "created_at", "-created_at"} {
		if !SortAllowed(s) {
			t.Errorf("ordenação %q deveria ser permitida", s)
		}
	}
	for _, s := range []string{"email", "-email", "document_number", "name desc", "name;drop table customers", "+name", "NAME"} {
		if SortAllowed(s) {
			t.Errorf("ordenação %q não deveria ser permitida", s)
		}
	}
}

func TestSnapshotOf(t *testing.T) {
	c, err := New(uuid.New(), uuid.New(), companyInput(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	c.Email = "financeiro@acme.com"
	addr := validAddress()
	at := testNow.Add(time.Hour)

	s := SnapshotOf(c, &addr, at)
	want := Snapshot{CustomerID: c.ID, Name: "Acme", LegalName: "Acme Software Ltda", DocumentType: "cnpj",
		DocumentNumber: validCNPJ, Email: "financeiro@acme.com", Address: &addr, CapturedAt: at}
	if s != want {
		t.Fatalf("snapshot = %+v\nesperado %+v", s, want)
	}
	if SnapshotOf(c, nil, at).Address != nil {
		t.Fatal("snapshot sem endereço deve manter o endereço nulo")
	}
}
