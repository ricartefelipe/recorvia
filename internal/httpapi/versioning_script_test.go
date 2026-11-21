package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// matchesRoute informa se o caminho chamado pelo roteiro (com variáveis do
// shell, sem a query) corresponde ao padrão da rota: cada segmento {param}
// aceita qualquer valor e os demais precisam ser iguais.
func matchesRoute(pattern, path string) bool {
	want, got := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, seg := range want {
		if strings.HasPrefix(seg, "{") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if seg != got[i] {
			return false
		}
	}
	return true
}

// O exercício de recuperação de desastre (deploy/backup/dr-check.sh) altera
// recursos pela API real: toda chamada a uma alteração versionada
// (versionedRoutes) informa a versão em if_match, ou a API a recusa com 428
// e o exercício para no meio.
func TestDisasterRecoveryScriptSendsIfMatch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", "dr-check.sh"))
	if err != nil {
		t.Fatal(err)
	}
	// Junta as linhas continuadas com barra invertida: a atribuição de
	// if_match pode vir na linha anterior à chamada.
	script := strings.ReplaceAll(string(b), "\\\n", " ")
	call := regexp.MustCompile(`\b(?:api|api_call|api_quiet|session_call) (GET|PUT|POST|PATCH|DELETE) "?(/api/[^"\s?]+)`)
	versioned := 0
	for _, line := range strings.Split(script, "\n") {
		// Uma linha pode ter mais de uma chamada (a leitura da versão dentro
		// da atribuição de if_match e a alteração): o prefixo de cada uma é o
		// trecho da linha antes dela.
		for _, m := range call.FindAllStringSubmatchIndex(line, -1) {
			prefix, method, path := line[:m[0]], line[m[2]:m[3]], line[m[4]:m[5]]
			for key := range versionedRoutes {
				routeMethod, pattern, _ := strings.Cut(key, " ")
				if routeMethod != method || !matchesRoute(pattern, path) {
					continue
				}
				versioned++
				if !strings.Contains(prefix, "if_match=") {
					t.Errorf("%s %s (rota versionada %s) sem if_match no roteiro", method, path, key)
				}
			}
		}
	}
	if versioned == 0 {
		t.Fatal("nenhuma alteração versionada encontrada no roteiro: o padrão das chamadas mudou?")
	}
}

func TestMatchesRoute(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"/api/v1/platform/plan", "/api/v1/platform/plan", true},
		{"/api/v1/payment-providers/{provider}", "/api/v1/payment-providers/simulator", true},
		{"/api/v1/reconciliation/discrepancies/{id}/resolve", "/api/v1/reconciliation/discrepancies/$id/resolve", true},
		{"/api/v1/reconciliation/discrepancies/{id}/resolve", "/api/v1/reconciliation/discrepancies/$id", false},
		{"/api/v1/members/{user_id}", "/api/v1/members/", false},
		{"/api/v1/platform/plan", "/api/v1/platform/plans", false},
	} {
		if got := matchesRoute(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchesRoute(%q, %q) = %v, esperado %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
