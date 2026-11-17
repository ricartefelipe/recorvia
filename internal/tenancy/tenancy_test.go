package tenancy

import (
	"bytes"
	"crypto/sha256"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

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

func validTenantInput() TenantInput {
	return TenantInput{Name: "Acme Software", Slug: "acme"}
}

func TestNewTenantDefaults(t *testing.T) {
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	id := uuid.New()
	tn, err := NewTenant(id, TenantInput{Name: "  Acme Software  ", Slug: "  ACME-Ltda "}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := Tenant{
		ID: id, Slug: "acme-ltda", Name: "Acme Software", Timezone: "America/Sao_Paulo", Currency: "BRL",
		InvoicePrefix: "FAT", PaymentTermsDays: 5, Status: TenantActive, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if tn != want {
		t.Fatalf("organização = %+v\nesperado     %+v", tn, want)
	}
}

func TestNewTenantNormalizesFields(t *testing.T) {
	tn, err := NewTenant(uuid.New(), TenantInput{
		Name: "Acme", Slug: "acme", LegalName: "  Acme Software Ltda ", DocumentNumber: "11.222.333/0001-81",
		Timezone: "America/Manaus", InvoicePrefix: " nf2026 ", PaymentTermsDays: ptr(0),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tn.LegalName != "Acme Software Ltda" || tn.DocumentNumber != "11222333000181" || tn.Timezone != "America/Manaus" ||
		tn.InvoicePrefix != "NF2026" || tn.PaymentTermsDays != 0 {
		t.Fatalf("normalização inesperada: %+v", tn)
	}
}

func TestNewTenantValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*TenantInput)
		field  string
	}{
		{"nome curto", func(in *TenantInput) { in.Name = " A " }, "name"},
		{"nome longo", func(in *TenantInput) { in.Name = strings.Repeat("á", 201) }, "name"},
		{"slug vazio", func(in *TenantInput) { in.Slug = "" }, "slug"},
		{"slug de um caractere", func(in *TenantInput) { in.Slug = "a" }, "slug"},
		{"slug iniciando com hífen", func(in *TenantInput) { in.Slug = "-acme" }, "slug"},
		{"slug com espaço interno", func(in *TenantInput) { in.Slug = "acme ltda" }, "slug"},
		{"slug com sublinhado", func(in *TenantInput) { in.Slug = "acme_ltda" }, "slug"},
		{"slug com 64 caracteres", func(in *TenantInput) { in.Slug = strings.Repeat("a", 64) }, "slug"},
		{"razão social longa", func(in *TenantInput) { in.LegalName = strings.Repeat("x", 201) }, "legal_name"},
		{"CNPJ com dígito inválido", func(in *TenantInput) { in.DocumentNumber = "11222333000182" }, "document_number"},
		{"CPF no lugar do CNPJ", func(in *TenantInput) { in.DocumentNumber = "52998224725" }, "document_number"},
		{"fuso sem região", func(in *TenantInput) { in.Timezone = "UTC" }, "timezone"},
		{"fuso inexistente", func(in *TenantInput) { in.Timezone = "America/Atlantida" }, "timezone"},
		{"prefixo com hífen", func(in *TenantInput) { in.InvoicePrefix = "FAT-1" }, "invoice_prefix"},
		{"prefixo longo", func(in *TenantInput) { in.InvoicePrefix = "ABCDEFGHIJK" }, "invoice_prefix"},
		{"prazo negativo", func(in *TenantInput) { in.PaymentTermsDays = ptr(-1) }, "payment_terms_days"},
		{"prazo acima de 120 dias", func(in *TenantInput) { in.PaymentTermsDays = ptr(121) }, "payment_terms_days"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validTenantInput()
			tc.mutate(&in)
			_, err := NewTenant(uuid.New(), in, time.Now())
			fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
			if got := fields; !slices.Equal(got, []string{tc.field}) {
				t.Fatalf("campos = %v, esperado [%s]", got, tc.field)
			}
		})
	}
}

func TestNewTenantLimitsAccepted(t *testing.T) {
	in := TenantInput{Name: strings.Repeat("á", 200), Slug: "a" + strings.Repeat("-", 62), LegalName: strings.Repeat("x", 200),
		InvoicePrefix: "ABCDEFGHIJ", PaymentTermsDays: ptr(120)}
	if _, err := NewTenant(uuid.New(), in, time.Now()); err != nil {
		t.Fatalf("valores no limite deveriam ser aceitos: %v", err)
	}
}

func TestNewTenantReportsAllFields(t *testing.T) {
	_, err := NewTenant(uuid.New(), TenantInput{Name: "", Slug: "", Timezone: "Lua", PaymentTermsDays: ptr(500)}, time.Now())
	fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
	if got := fields; !slices.Equal(got, []string{"name", "slug", "timezone", "payment_terms_days"}) {
		t.Fatalf("todos os campos inválidos devem ser informados, obtido %v", got)
	}
}

func TestApplyUpdate(t *testing.T) {
	base, err := NewTenant(uuid.New(), TenantInput{Name: "Acme", Slug: "acme", PaymentTermsDays: ptr(10)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("slug é permanente e prazo omitido é mantido", func(t *testing.T) {
		tn := base
		err := tn.ApplyUpdate(TenantInput{Name: "Acme Brasil", Slug: "outro slug inválido", Timezone: "America/Recife", InvoicePrefix: "nfe"})
		if err != nil {
			t.Fatal(err)
		}
		if tn.Slug != "acme" || tn.Name != "Acme Brasil" || tn.Timezone != "America/Recife" || tn.InvoicePrefix != "NFE" || tn.PaymentTermsDays != 10 {
			t.Fatalf("alteração inesperada: %+v", tn)
		}
	})
	t.Run("prazo informado substitui o anterior", func(t *testing.T) {
		tn := base
		if err := tn.ApplyUpdate(TenantInput{Name: "Acme", PaymentTermsDays: ptr(30)}); err != nil {
			t.Fatal(err)
		}
		if tn.PaymentTermsDays != 30 {
			t.Fatalf("prazo = %d, esperado 30", tn.PaymentTermsDays)
		}
	})
	t.Run("dados inválidos não alteram a organização", func(t *testing.T) {
		tn := base
		err := tn.ApplyUpdate(TenantInput{Name: "X", PaymentTermsDays: ptr(1)})
		assertAppErr(t, err, apperr.KindInvalid, "invalid_organization")
		if tn != base {
			t.Fatalf("organização alterada apesar do erro: %+v", tn)
		}
	})
}

func TestTenantLocation(t *testing.T) {
	loc, err := Tenant{Timezone: "America/Sao_Paulo"}.Location()
	if err != nil || loc.String() != "America/Sao_Paulo" {
		t.Fatalf("Location = %v, %v", loc, err)
	}
	if _, err := (Tenant{Timezone: "Nada/Nenhum"}).Location(); err == nil {
		t.Fatal("fuso inexistente deveria falhar")
	}
}

func TestInvitationState(t *testing.T) {
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	cases := []struct {
		name string
		inv  Invitation
		want InvitationState
	}{
		{"pendente antes da expiração", Invitation{ExpiresAt: future}, InvitationPending},
		{"expirado no instante exato da expiração", Invitation{ExpiresAt: now}, InvitationExpired},
		{"expirado depois do prazo", Invitation{ExpiresAt: past}, InvitationExpired},
		{"revogado", Invitation{ExpiresAt: future, RevokedAt: &past}, InvitationRevoked},
		{"revogado e expirado continua revogado", Invitation{ExpiresAt: past, RevokedAt: &past}, InvitationRevoked},
		{"aceito", Invitation{ExpiresAt: future, AcceptedAt: &past}, InvitationAccepted},
		{"aceito prevalece sobre expiração", Invitation{ExpiresAt: past, AcceptedAt: &past}, InvitationAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.State(now); got != tc.want {
				t.Fatalf("State = %s, esperado %s", got, tc.want)
			}
		})
	}
}

func TestNewToken(t *testing.T) {
	token, hash := NewToken()
	if len(token) != 43 {
		t.Errorf("token deve ter 256 bits em base64url sem preenchimento (43 caracteres), obtido %d", len(token))
	}
	if strings.ContainsAny(token, "+/=") {
		t.Errorf("token deve usar alfabeto base64url: %q", token)
	}
	sum := sha256.Sum256([]byte(token))
	if !bytes.Equal(hash, sum[:]) || !bytes.Equal(HashToken(token), hash) {
		t.Fatal("hash deve ser o SHA-256 do token")
	}
	if bytes.Contains(hash, []byte(token)) {
		t.Fatal("hash não pode conter o token em claro")
	}
	other, otherHash := NewToken()
	if other == token || bytes.Equal(otherHash, hash) {
		t.Fatal("tokens consecutivos devem ser distintos")
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"ana@example.com", "ana@example.com", true},
		{"  Ana.Souza@Example.COM ", "ana.souza@example.com", true},
		{"ana+cobranca@empresa.com.br", "ana+cobranca@empresa.com.br", true},
		{"", "", false},
		{"ana", "", false},
		{"ana@localhost", "", false},
		{"Ana <ana@example.com>", "", false},
		{"ana@example.com, bia@example.com", "", false},
		{"ana@@example.com", "", false},
		{strings.Repeat("a", 64) + "@" + strings.Repeat("b.", 130) + "com", "", false}, // 328 caracteres
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeEmail(tc.in)
			if !tc.ok {
				fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_email")
				if got := fields; !slices.Equal(got, []string{"email"}) {
					t.Errorf("campo do erro = %v", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NormalizeEmail(%q) = %q, %v; esperado %q", tc.in, got, err, tc.want)
			}
		})
	}
}
