package apicredentials

import (
	"bytes"
	"crypto/subtle"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

var testNow = time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)

func TestGeneratedTokenFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		keyID, secret := newKeyID(), newSecret()
		if len(keyID) != KeyIDLength || len(secret) != SecretLength {
			t.Fatalf("tamanhos inesperados: %d e %d", len(keyID), len(secret))
		}
		token := FormatToken(keyID, secret)
		if len(token) != TokenLength || !strings.HasPrefix(token, "rcv_") {
			t.Fatalf("token fora do formato: %q", token)
		}
		k, s, ok := ParseToken(token)
		if !ok || k != keyID || s != secret {
			t.Fatalf("ParseToken(%q) = %q %q %v", token, k, s, ok)
		}
		if seen[secret] || seen[keyID] {
			t.Fatal("valor aleatório repetido")
		}
		seen[secret], seen[keyID] = true, true
	}
}

func TestParseTokenRejectsMalformed(t *testing.T) {
	keyID, secret := strings.Repeat("a", KeyIDLength), strings.Repeat("b", SecretLength)
	valid := FormatToken(keyID, secret)
	cases := map[string]string{
		"vazio":                  "",
		"sem prefixo":            "xyz_" + keyID + "_" + secret,
		"prefixo maiúsculo":      "RCV_" + keyID + "_" + secret,
		"curto":                  valid[:len(valid)-1],
		"longo":                  valid + "a",
		"sem separador":          "rcv_" + keyID + "a" + secret,
		"identificador inválido": "rcv_" + strings.Repeat("A", KeyIDLength) + "_" + secret,
		"segredo com dígito 0":   "rcv_" + keyID + "_" + strings.Repeat("0", SecretLength),
		"segredo com sublinhado": "rcv_" + keyID + "_" + strings.Repeat("_", SecretLength),
		"segredo com espaço":     "rcv_" + keyID + "_" + strings.Repeat("b", SecretLength-1) + " ",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, ok := ParseToken(token); ok {
				t.Fatalf("token aceito: %q", token)
			}
		})
	}
	if _, _, ok := ParseToken(valid); !ok {
		t.Fatal("token válido recusado")
	}
}

func TestHashSecret(t *testing.T) {
	a, b := HashSecret("segredo"), HashSecret("segredo")
	if len(a) != 32 || !bytes.Equal(a, b) || bytes.Equal(a, HashSecret("segredO")) {
		t.Fatalf("hash inesperado: %x %x", a, b)
	}
}

func credentialWithSecret(secret string) Credential {
	return Credential{ID: uuid.New(), TenantID: uuid.New(), Name: "ERP", KeyID: newKeyID(), SecretHash: HashSecret(secret),
		Scopes: []tenancy.Permission{tenancy.PermCustomersRead}, CreatedBy: uuid.New(), CreatedAt: testNow, Version: 1}
}

func TestMatchSecret(t *testing.T) {
	grace := testNow.Add(time.Hour)
	rotated := credentialWithSecret("novo")
	rotated.PreviousSecretHash, rotated.PreviousSecretExpiresAt = HashSecret("antigo"), &grace
	cases := []struct {
		name   string
		c      Credential
		secret string
		at     time.Time
		want   bool
	}{
		{"segredo vigente", credentialWithSecret("vigente"), "vigente", testNow, true},
		{"segredo errado", credentialWithSecret("vigente"), "outro", testNow, false},
		{"vigente após rotação", rotated, "novo", testNow, true},
		{"anterior dentro da janela", rotated, "antigo", testNow, true},
		{"anterior no fim da janela", rotated, "antigo", grace, false},
		{"anterior depois da janela", rotated, "antigo", grace.Add(time.Second), false},
		{"hash vigente corrompido", Credential{SecretHash: []byte{1, 2, 3}}, "x", testNow, false},
		{"anterior sem validade", Credential{SecretHash: HashSecret("a"), PreviousSecretHash: HashSecret("b")}, "b", testNow, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.MatchSecret(tc.secret, tc.at); got != tc.want {
				t.Fatalf("MatchSecret = %v, esperado %v", got, tc.want)
			}
		})
	}
}

// A verificação usa subtle.ConstantTimeCompare e sempre faz as duas
// comparações, com entradas de 32 bytes, qualquer que seja o resultado: o
// tempo de resposta não depende de qual hash coincide nem de quantos bytes.
func TestMatchSecretUsesConstantTimeComparison(t *testing.T) {
	if reflect.ValueOf(constantTimeCompare).Pointer() != reflect.ValueOf(subtle.ConstantTimeCompare).Pointer() {
		t.Fatal("a comparação do segredo deve ser subtle.ConstantTimeCompare")
	}
	original := constantTimeCompare
	t.Cleanup(func() { constantTimeCompare = original })
	var calls int
	var sizes []int
	constantTimeCompare = func(x, y []byte) int {
		calls++
		sizes = append(sizes, len(x), len(y))
		return original(x, y)
	}
	grace := testNow.Add(time.Hour)
	rotated := credentialWithSecret("novo")
	rotated.PreviousSecretHash, rotated.PreviousSecretExpiresAt = HashSecret("antigo"), &grace
	for _, tc := range []struct {
		c      Credential
		secret string
	}{
		{credentialWithSecret("vigente"), "vigente"},
		{credentialWithSecret("vigente"), "errado"},
		{rotated, "novo"},
		{rotated, "antigo"},
		{rotated, "errado"},
		{Credential{}, "sem hash"},
	} {
		calls, sizes = 0, nil
		tc.c.MatchSecret(tc.secret, testNow)
		if calls != 2 {
			t.Fatalf("esperava 2 comparações, houve %d", calls)
		}
		for _, n := range sizes {
			if n != 32 {
				t.Fatalf("comparação com tamanho %d (esperado 32)", n)
			}
		}
	}
}

func TestCredentialStateIssuerAndPrefix(t *testing.T) {
	c := credentialWithSecret("s")
	if c.State(testNow) != StateActive || c.Prefix() != "rcv_"+c.KeyID || c.Issuer() != c.CreatedBy {
		t.Fatalf("credencial nova: %s %s", c.State(testNow), c.Prefix())
	}
	exp := testNow.Add(time.Hour)
	c.ExpiresAt = &exp
	if c.State(exp.Add(-time.Nanosecond)) != StateActive || c.State(exp) != StateExpired {
		t.Fatal("expiração deve valer a partir do instante exato")
	}
	rotator := uuid.New()
	c.RotatedBy = &rotator
	if c.Issuer() != rotator {
		t.Fatal("após rotação, o responsável é quem rotacionou")
	}
	c.RevokedAt = &testNow
	if c.State(testNow.Add(-time.Hour)) != StateRevoked {
		t.Fatal("revogação prevalece")
	}
}

func TestDelegationRules(t *testing.T) {
	for _, p := range []tenancy.Permission{tenancy.PermMembersManage, tenancy.PermPlatformManage, tenancy.PermAPICredentials,
		tenancy.PermCustomersAnonymize} {
		if Delegable(p) || !slices.Contains(NonDelegable(), p) {
			t.Fatalf("%s não deveria ser delegável", p)
		}
	}
	if !Delegable(tenancy.PermCustomersWrite) {
		t.Fatal("customers.write deveria ser delegável")
	}
	nd := NonDelegable()
	nd[0] = "alterado"
	if slices.Contains(NonDelegable(), tenancy.Permission("alterado")) {
		t.Fatal("NonDelegable deve devolver cópia")
	}
	scopes := []tenancy.Permission{tenancy.PermCustomersRead, tenancy.PermCustomersWrite, tenancy.PermInvoicesWrite, tenancy.PermMembersManage, "desconhecida"}
	got := EffectiveScopes(scopes, tenancy.RoleOperator)
	want := []tenancy.Permission{tenancy.PermCustomersRead, tenancy.PermCustomersWrite}
	if !slices.Equal(got, want) {
		t.Fatalf("escopos efetivos do operador = %v, esperado %v", got, want)
	}
	if got := EffectiveScopes(scopes, tenancy.RoleOwner); slices.Contains(got, tenancy.PermMembersManage) || len(got) != 3 {
		t.Fatalf("escopos efetivos do proprietário = %v", got)
	}
	if got := EffectiveScopes(scopes, ""); len(got) != 0 {
		t.Fatalf("sem papel, nenhum escopo vale: %v", got)
	}
}

func TestValidateScopes(t *testing.T) {
	tooMany := make([]string, MaxScopes+1)
	for i := range tooMany {
		tooMany[i] = string(tenancy.PermCustomersRead)
	}
	cases := []struct {
		name string
		raw  []string
		role tenancy.Role
		kind apperr.Kind
		code string
	}{
		{"vazio", nil, tenancy.RoleOwner, apperr.KindInvalid, "invalid_api_credential"},
		{"excesso", tooMany, tenancy.RoleOwner, apperr.KindInvalid, "invalid_api_credential"},
		{"desconhecido", []string{"customers.read", "tudo.liberado"}, tenancy.RoleOwner, apperr.KindInvalid, "invalid_api_credential"},
		{"não delegável", []string{"api_credentials.manage"}, tenancy.RoleOwner, apperr.KindInvalid, "invalid_api_credential"},
		{"além do papel", []string{"customers.write"}, tenancy.RoleFinance, apperr.KindForbidden, "scope_exceeds_permissions"},
		{"auditor sem escrita", []string{"invoices.read", "invoices.void"}, tenancy.RoleAuditor, apperr.KindForbidden, "scope_exceeds_permissions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateScopes(tc.raw, tc.role)
			e, ok := apperr.As(err)
			if !ok || e.Kind != tc.kind || e.Code != tc.code {
				t.Fatalf("erro = %v, esperado %v/%s", err, tc.kind, tc.code)
			}
		})
	}
	long := strings.Repeat("x", 200)
	_, err := ValidateScopes([]string{long}, tenancy.RoleOwner)
	if e, _ := apperr.As(err); e == nil || len(e.Fields) != 1 || len(e.Fields[0].Message) > 120 {
		t.Fatalf("mensagem de escopo desconhecido deve ser truncada: %v", err)
	}
	got, err := ValidateScopes([]string{"invoices.read", "customers.read", "invoices.read"}, tenancy.RoleAuditor)
	if err != nil || !slices.Equal(got, []tenancy.Permission{tenancy.PermCustomersRead, tenancy.PermInvoicesRead}) {
		t.Fatalf("escopos normalizados = %v, %v", got, err)
	}
}

func TestPrincipal(t *testing.T) {
	p := Principal{CredentialID: uuid.New(), TenantID: uuid.New(), Name: "ERP", Scopes: []tenancy.Permission{tenancy.PermCustomersRead}}
	if !p.Can(tenancy.PermCustomersRead) || p.Can(tenancy.PermCustomersWrite) {
		t.Fatal("permissões do principal pelos escopos")
	}
	if a := p.Actor(); a.Type != audit.ActorAPICredential || a.ID != p.CredentialID.String() || a.Label != "ERP" {
		t.Fatalf("ator: %+v", a)
	}
	if p.ClientID() != "api_credential:"+p.CredentialID.String() {
		t.Fatalf("cliente de idempotência: %s", p.ClientID())
	}
}

func FuzzParseToken(f *testing.F) {
	f.Add(FormatToken(newKeyID(), newSecret()))
	f.Add("rcv_")
	f.Add("rcv_aaaaaaaaaaaaaaaa_")
	f.Add("Bearer rcv_x")
	f.Fuzz(func(t *testing.T, token string) {
		keyID, secret, ok := ParseToken(token)
		if !ok {
			return
		}
		if len(keyID) != KeyIDLength || len(secret) != SecretLength || FormatToken(keyID, secret) != token {
			t.Fatalf("token aceito fora do formato: %q", token)
		}
		if !base32Lower(keyID) || !base32Lower(secret) {
			t.Fatalf("alfabeto inválido aceito: %q", token)
		}
	})
}
