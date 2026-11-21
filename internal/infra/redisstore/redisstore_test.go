package redisstore

import (
	"strings"
	"testing"
)

// RNF-12: nenhuma falha de Open (URL malformada ou conexão recusada) repete
// a senha da RECORVIA_REDIS_URL; go-redis devolve o erro de net/url com a
// URL inteira.
func TestOpenErrorsNeverCarryThePassword(t *testing.T) {
	cases := []struct{ url, want string }{
		{"redis://user:s3gr3d0-redis@host:port/0", "URL inválida"},
		{"redis://:s3gr3d0-redis@ho st:6379", "URL inválida"},
		{"redis://u:s3gr3d0%zz-redis@h:1", "URL inválida"},
		// "/" sem codificação: o net/url lê como porta o trecho da senha.
		{"redis://default:s3gr3d0/GhIjK+lm@redis:6379/0", "URL inválida"},
		{"redis://user:s3gr3d0-redis@127.0.0.1:1/0", "conexão"},
	}
	for _, tc := range cases {
		_, err := Open(t.Context(), tc.url)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: erro %v, esperado %q", tc.url, err, tc.want)
		}
		for _, leak := range []string{"s3gr3d0", "%zz", "GhIjK"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: a mensagem %q contém %q", tc.url, err.Error(), leak)
			}
		}
	}
}
