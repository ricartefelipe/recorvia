package apicredentials

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// A lista de permissões não delegáveis aparece também na interface (que só
// oferece os escopos permitidos) e no exercício de recuperação de desastre
// (que cria uma credencial com todos os escopos possíveis). Uma permissão
// nova não delegável que fique só no backend faz a interface oferecer um
// escopo recusado e o exercício falhar; este teste aponta a divergência.
func TestNonDelegableListsStayInSync(t *testing.T) {
	want := make([]string, 0, len(NonDelegable()))
	for _, p := range NonDelegable() {
		want = append(want, string(p))
	}
	slices.Sort(want)
	root := filepath.Join("..", "..")
	cases := []struct {
		file    string
		pattern *regexp.Regexp
	}{
		{"web/src/features/integrations/CredentialsTab.tsx", regexp.MustCompile(`(?s)NON_DELEGABLE_SCOPES: readonly string\[\] = \[(.*?)\]`)},
		{"deploy/backup/dr-check.sh", regexp.MustCompile(`non_delegable='\[(.*?)\]'`)},
	}
	item := regexp.MustCompile(`["']([a-z_]+\.[a-z_]+)["']`)
	for _, tc := range cases {
		b, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		m := tc.pattern.FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s: lista de permissões não delegáveis não encontrada", tc.file)
		}
		var got []string
		for _, it := range item.FindAllSubmatch(m[1], -1) {
			got = append(got, string(it[1]))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: permissões não delegáveis %v, esperado %v (apicredentials.NonDelegable)", tc.file, got, want)
		}
	}
}
