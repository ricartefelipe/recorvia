package postgres

import (
	"strings"
	"testing"
)

// RNF-12: nenhuma falha de Open (configuração malformada ou conexão
// recusada) repete a senha da URL do banco.
func TestOpenErrorsNeverCarryThePassword(t *testing.T) {
	cases := []struct{ url, want string }{
		{"postgres://user:s3gr3d0-pg@host:port/db", "configuração inválida"},
		{"postgres://u:s3gr3d0%zz@h/db", "configuração inválida"},
		{"host=db user=app password=s3gr3d0-pg port=abc", "configuração inválida"},
		{"postgres://user:s3gr3d0-pg@127.0.0.1:1/db?connect_timeout=2", "conexão"},
	}
	for _, tc := range cases {
		_, err := Open(t.Context(), tc.url, Options{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: erro %v, esperado %q", tc.url, err, tc.want)
		}
		if strings.Contains(err.Error(), "s3gr3d0") {
			t.Fatalf("%s: a mensagem %q contém a senha", tc.url, err.Error())
		}
	}
}
