package migrate_test

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/db"
)

// Documento com o modelo de isolamento, que lista as funções que atravessam
// o RLS.
const multitenancyDoc = "../../../docs/arquitetura/multitenancy.md"

var (
	// Cabeçalho da função até o corpo: nome e opções (LANGUAGE, SECURITY
	// DEFINER, search_path).
	createFunction = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+([a-z_][a-z0-9_]*)\s*\((.*?)\bAS\s+\$`)
	dropFunction   = regexp.MustCompile(`(?i)DROP\s+FUNCTION\s+(?:IF\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
)

// securityDefinerFunctions aplica, na ordem, a seção Up das migrations e
// devolve as funções SECURITY DEFINER existentes ao fim, com a migration que
// criou cada uma.
func securityDefinerFunctions(t *testing.T) map[string]string {
	t.Helper()
	entries, err := db.Migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := map[string]string{}
	for _, name := range names {
		raw, err := db.Migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(raw), "-- +goose Down")
		for _, m := range dropFunction.FindAllStringSubmatch(up, -1) {
			delete(out, strings.ToLower(m[1]))
		}
		for _, m := range createFunction.FindAllStringSubmatch(up, -1) {
			fn := strings.ToLower(m[1])
			if !strings.Contains(strings.ToUpper(m[2]), "SECURITY DEFINER") {
				delete(out, fn) // recriada sem o privilégio do dono
				continue
			}
			if _, ok := out[fn]; !ok {
				out[fn] = name
			}
		}
	}
	return out
}

// Toda função SECURITY DEFINER criada por migration atravessa o RLS com o
// privilégio do dono das tabelas e precisa constar no inventário do modelo
// de isolamento (docs/arquitetura/multitenancy.md), que é a referência de
// quem audita o acesso entre tenants.
func TestSecurityDefinerFunctionsAreDocumented(t *testing.T) {
	functions := securityDefinerFunctions(t)
	// O levantamento precisa enxergar as funções: sem estas, a expressão
	// deixou de reconhecer as migrations e o teste passaria sem conferir nada.
	for _, known := range []string{"app_outbox_claim", "app_tenants_with_due_billing", "app_claimable_reconciliation_runs",
		"app_reconciliation_run_stats"} {
		if _, ok := functions[known]; !ok {
			t.Fatalf("função %s não encontrada nas migrations: o levantamento está incompleto", known)
		}
	}
	doc, err := os.ReadFile(multitenancyDoc)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for fn, migration := range functions {
		if !strings.Contains(string(doc), "`"+fn+"`") {
			missing = append(missing, fn+" ("+migration+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("funções SECURITY DEFINER fora do inventário de %s:\n  %s", multitenancyDoc, strings.Join(missing, "\n  "))
	}
}
