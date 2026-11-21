package redact

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestURLMasksPasswordEvenWhenMalformed(t *testing.T) {
	cases := []struct{ in, want string }{
		{"redis://user:s3gr3d0@host:6379/0", "redis://user:xxxxx@host:6379/0"},
		{"redis://:s3gr3d0@host:port/0", "redis://:xxxxx@host:port/0"},
		{"amqp://user:p@ss@ho st/", "amqp://user:xxxxx@ho st/"},
		{"postgres://u:s3%zz@h/db?sslmode=disable", "postgres://u:xxxxx@h/db?sslmode=disable"},
		{"rediss://u:s3gr3d0@[::1", "rediss://u:xxxxx@[::1"},
		{"redis://host:6379/0", "redis://host:6379/0"},
		{"redis://user@host:6379/0", "redis://user@host:6379/0"},
		{"amqp://guest:guest@rabbitmq:5672/?x=a@b", "amqp://guest:xxxxx@rabbitmq:5672/?x=a@b"},
		{"host=db user=app password=s3gr3d0 dbname=x", "host=db user=app password=xxxxx dbname=x"},
		{"host=db password='s3 gr3d0' dbname=x", "host=db password=xxxxx dbname=x"},
		{"redis://u:ab/cd@host:6379", "redis://u:xxxxx@host:6379"},
		{"redis://host/?x=a@b", "redis://host/?x=a@b"},
		// Senha com / sem codificação: o trecho depois dele não é porta.
		{"redis://default:Ab3dEf/GhIjK+lm@redis:6379/0", "redis://default:xxxxx@redis:6379/0"},
		{"amqp://recorvia:Ab3dEf?Gh#Ij@rabbit:5672/", "amqp://recorvia:xxxxx@rabbit:5672/"},
		{"redis://u:pa ss@h:1/0", "redis://u:xxxxx@h:1/0"},
		{`redis://u:a"b'c@h:1/0`, "redis://u:xxxxx@h:1/0"},
		// Sem credenciais: porta numérica e @ no caminho ou na consulta.
		{"https://api.provider.test:443/v1/charges?email=a@b.com", "https://api.provider.test:443/v1/charges?email=a@b.com"},
		{"http://[::1]:8080/x@y", "http://[::1]:8080/x@y"},
		{"https://app.test/clientes/a@b.com", "https://app.test/clientes/a@b.com"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := URL(tc.in); got != tc.want {
			t.Errorf("URL(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

func TestStringRemovesSecretsAndEmbeddedCredentials(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		secrets []string
		leaked  []string
	}{
		{"URL informada e senha decodificada", `parse "redis://u:p%40ssw0rd@h:x": invalid port; senha p@ssw0rd`, []string{"redis://u:p%40ssw0rd@h:x"},
			[]string{"p%40ssw0rd", "p@ssw0rd"}},
		{"URL não informada no texto", `falha em amqp://guest:segredo-forte@rabbit:5672/vhost`, nil, []string{"segredo-forte"}},
		{"senha literal", "535 autenticação recusada para s3nh4-smtp", []string{"s3nh4-smtp"}, []string{"s3nh4-smtp"}},
		{"string de conexão chave=valor", "cannot parse host=db password=s3gr3d0 port=x", nil, []string{"s3gr3d0"}},
		{"segredo vazio é ignorado", "nada a esconder", []string{""}, nil},
		{"senha com @ sem codificação no texto", `dial amqp://guest:p@ss@w0rd@rabbit:5672/: refused`, nil, []string{"p@ss", "ss@w0rd", "w0rd"}},
		{"senha com espaço no texto", `parse "redis://u:minha senha@h:1": invalid`, nil, []string{"minha", "senha"}},
		{"senha com aspas no texto", `parse "redis://u:abc"def@h:1": invalid`, nil, []string{"abc", "def"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in, tc.secrets...)
			for _, l := range tc.leaked {
				if strings.Contains(got, l) {
					t.Fatalf("%q ainda contém %q", got, l)
				}
			}
			if len(tc.leaked) == 0 && got != tc.in {
				t.Fatalf("texto sem segredos alterado: %q", got)
			}
		})
	}
}

func TestErrorKeepsCauseAndSanitizesMessage(t *testing.T) {
	if Error(nil, "x") != nil {
		t.Fatal("Error(nil) deve ser nil")
	}
	cause := fmt.Errorf("redis: conexão a redis://u:s3gr3d0@h:6379: %w", context.DeadlineExceeded)
	err := Error(cause, "redis://u:s3gr3d0@h:6379")
	if strings.Contains(err.Error(), "s3gr3d0") || !strings.Contains(err.Error(), "redis://u:xxxxx@h:6379") {
		t.Fatalf("mensagem %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a causa deve continuar acessível por errors.Is")
	}
	if wrapped := fmt.Errorf("api: %w", err); strings.Contains(wrapped.Error(), "s3gr3d0") {
		t.Fatalf("mensagem encadeada %q", wrapped.Error())
	}
}

// Segredos literais curtos não são substituídos fora da posição da URL: a
// senha "p" não corrompe "amqps://"; URLs sem credenciais ficam intactas.
func TestStringKeepsTextWithoutCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"falha em amqps://rabbit:5671/ (senha p)", "falha em amqps://rabbit:5671/ (senha p)"},
		{"POST https://api.provider.test:443/v1/charges?email=a@b.com: 503", "POST https://api.provider.test:443/v1/charges?email=a@b.com: 503"},
		{"amqps://u:p@rabbit:5671/ recusou", "amqps://u:xxxxx@rabbit:5671/ recusou"},
	}
	for _, tc := range cases {
		if got := String(tc.in, "p", "amqps://u:p@rabbit:5671/"); got != tc.want {
			t.Errorf("String(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

// ParseError não repete a mensagem da biblioteca, que pode trazer trechos
// da senha (escape inválido), e mantém o motivo útil quando ele é seguro.
func TestParseErrorNeverRepeatsLibraryText(t *testing.T) {
	cases := []struct {
		raw     string
		want    string
		secrets []string
	}{
		{"redis://u:s3%zzgr3d0@h:1", "codifique na senha", []string{"s3%zzgr3d0", "%zz", "gr3d0"}},
		{"redis://user:s3gr3d0@host:port/0", "codifique na senha", []string{"s3gr3d0", "after host"}},
		{"redis://:s3gr3d0@ho st:6379", "codifique na senha", []string{"s3gr3d0"}},
		// O net/url lê como porta o trecho da senha depois do "/".
		{"redis://default:Ab3dEf/GhIjK+lm@redis:6379/0", "codifique na senha", []string{"Ab3dEf", "GhIjK"}},
		{"amqp://recorvia:Ab3dEf?GhIjK@rabbit:5672/", "codifique na senha", []string{"Ab3dEf", "GhIjK"}},
	}
	for _, tc := range cases {
		_, perr := url.Parse(tc.raw)
		if perr == nil {
			t.Fatalf("URL %q deveria ser inválida", tc.raw)
		}
		err := ParseError("redis", tc.raw, fmt.Errorf("embrulhado: %w", perr))
		msg := err.Error()
		if !strings.Contains(msg, ": URL inválida (") || !strings.Contains(msg, tc.want) || !strings.Contains(msg, Mask) {
			t.Fatalf("mensagem %q", msg)
		}
		for _, s := range tc.secrets {
			if strings.Contains(msg, s) {
				t.Fatalf("%q contém %q", msg, s)
			}
		}
		if errors.Unwrap(err) != nil {
			t.Fatal("a mensagem original não pode ficar encadeada")
		}
	}
	if msg := ParseError("rabbit", "amqp://u:segredo@h", errors.New("falha qualquer com segredo")).Error(); strings.Contains(msg, "segredo") {
		t.Fatalf("mensagem %q", msg)
	}
	// Sem credenciais, o motivo do net/url é útil e seguro.
	noCredentials := "redis://host:port/0"
	perr := &url.Error{Op: "parse", URL: noCredentials, Err: errors.New(`invalid port ":port" after host`)}
	if msg := ParseError("redis", noCredentials, perr).Error(); !strings.Contains(msg, `invalid port ":port" after host`) {
		t.Fatalf("mensagem %q", msg)
	}
}
