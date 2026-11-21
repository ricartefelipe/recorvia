package identity

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestSafeReturnPath(t *testing.T) {
	long := "/" + strings.Repeat("a", 511)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"vazio vai para a raiz", "", "/"},
		{"raiz", "/", "/"},
		{"caminho com consulta e fragmento", "/faturas?status=open#topo", "/faturas?status=open#topo"},
		{"caminho com barras codificadas permanece relativo", "/%2F%2Fevil.example", "/%2F%2Fevil.example"},
		{"caminho no limite de 512 bytes", long, long},
		{"caminho acima de 512 bytes", long + "a", "/"},
		{"protocolo relativo", "//evil.example", "/"},
		{"protocolo relativo com três barras", "///evil.example/x", "/"},
		{"barra seguida de barra invertida", "/\\evil.example", "/"},
		{"barra invertida no meio do caminho", "/faturas\\..\\admin", "/"},
		{"URL absoluta", "https://evil.example/login", "/"},
		{"URL absoluta sem barra", "evil.example", "/"},
		{"esquema javascript", "javascript:alert(1)", "/"},
		{"quebra de linha CRLF", "/ok\r\nSet-Cookie: sessao=1", "/"},
		{"somente LF", "/ok\nLocation: //evil.example", "/"},
		{"somente CR", "/ok\revil", "/"},
		{"caminho relativo sem barra inicial", "faturas", "/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SafeReturnPath(tc.in); got != tc.want {
				t.Fatalf("SafeReturnPath(%q) = %q, esperado %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSafeReturnPathRejectsTab(t *testing.T) {
	for _, in := range []string{"/\t/evil.example", "/\t\t/evil.example", "/ok\tx"} {
		if got := SafeReturnPath(in); got != "/" {
			t.Errorf("SafeReturnPath(%q) = %q, esperado \"/\"", in, got)
		}
	}
}

func FuzzSafeReturnPath(f *testing.F) {
	for _, s := range []string{"", "/", "/faturas", "//evil", "/\\evil", "https://x", "/a\r\nb", "/a\\b", strings.Repeat("/a", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := SafeReturnPath(in)
		if !strings.HasPrefix(got, "/") {
			t.Fatalf("resultado %q não começa com /", got)
		}
		if strings.HasPrefix(got, "//") || strings.HasPrefix(got, "/\\") {
			t.Fatalf("resultado %q permite redirecionamento para outro host", got)
		}
		if strings.ContainsAny(got, "\r\n\\") {
			t.Fatalf("resultado %q contém caracteres proibidos", got)
		}
		if len(got) > 512 {
			t.Fatalf("resultado com %d bytes excede o limite", len(got))
		}
		if got != "/" && got != in {
			t.Fatalf("caminho aceito deve ser devolvido sem alterações: %q -> %q", in, got)
		}
	})
}

func TestValidCSRF(t *testing.T) {
	token := RandomToken(32)
	cases := []struct {
		name    string
		session string
		got     string
		want    bool
	}{
		{"token igual", token, token, true},
		{"token vazio", token, "", false},
		{"sessão sem token", "", "", false},
		{"sessão sem token e token informado", "", token, false},
		{"token diferente", token, RandomToken(32), false},
		{"prefixo do token", token, token[:len(token)-1], false},
		{"token com sufixo extra", token, token + "x", false},
		{"diferença de caixa", "abcDEF", "ABCdef", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Session{CSRFToken: tc.session}).ValidCSRF(tc.got); got != tc.want {
				t.Fatalf("ValidCSRF = %v, esperado %v", got, tc.want)
			}
		})
	}
}

func TestRandomToken(t *testing.T) {
	for _, n := range []int{16, 32, 48} {
		tok := RandomToken(n)
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != n {
			t.Fatalf("RandomToken(%d) = %q (%d bytes, %v)", n, tok, len(raw), err)
		}
	}
	seen := map[string]bool{}
	for range 200 {
		tok := RandomToken(32)
		if seen[tok] {
			t.Fatalf("token repetido: %s", tok)
		}
		seen[tok] = true
	}
}

func TestDefaultPolicy(t *testing.T) {
	p := DefaultPolicy()
	want := Policy{Absolute: 12 * time.Hour, Idle: 2 * time.Hour, LoginFlowTTL: 10 * time.Minute, TouchInterval: time.Minute, RequireVerifiedEmail: true,
		RevokeTimeout: 5 * time.Second}
	if p != want {
		t.Fatalf("DefaultPolicy = %+v, esperado %+v", p, want)
	}
	if p.Idle >= p.Absolute || p.TouchInterval >= p.Idle {
		t.Fatal("inatividade deve ser menor que a duração absoluta e o intervalo de toque menor que a inatividade")
	}
}
