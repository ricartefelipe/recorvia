package pgstore_test

import (
	"bytes"
	"encoding/base64"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
)

var (
	shellBlock    = regexp.MustCompile("(?s)```bash\n(.*?)```")
	shellVariable = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`)
)

// Os comandos dos guias e runbooks de operação podem ser executados como
// estão escritos: toda variável de shell usada num bloco bash é definida num
// bloco bash do mesmo documento, com um valor de exemplo e a explicação do
// que ela é (por exemplo, o endereço da API e a credencial da organização
// usados na conciliação da janela perdida).
func TestOperationsDocsShellVariablesAreDefined(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "operacao")
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbooks, err := filepath.Glob(filepath.Join(root, "runbooks", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := 0
	for _, f := range append(files, runbooks...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var shell strings.Builder
		for _, block := range shellBlock.FindAllStringSubmatch(string(b), -1) {
			blocks++
			shell.WriteString(block[1])
		}
		seen := map[string]bool{}
		for _, m := range shellVariable.FindAllStringSubmatch(shell.String(), -1) {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			if !regexp.MustCompile(`(?m)^\s*(export\s+)?` + name + `=\S`).MatchString(shell.String()) {
				t.Errorf("%s: a variável $%s é usada num comando sem ser definida em nenhum bloco bash do documento", filepath.Base(f), name)
			}
		}
	}
	if blocks == 0 {
		t.Fatal("nenhum bloco bash encontrado em docs/operacao")
	}
}

// O runbook de recuperação de desastre diz com qual credencial a conciliação
// da janela perdida é feita: as rotas são de tenant e a equipe da plataforma
// não tem acesso aos dados das organizações.
func TestDisasterRecoveryRunbookNamesReconciliationCredential(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "operacao", "runbooks", "recuperacao-de-desastre.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbook := string(b)
	start := strings.Index(runbook, "5. Concilie a janela perdida")
	end := strings.Index(runbook, "6. Com as divergências resolvidas")
	if start < 0 || end < start {
		t.Fatal("passo da conciliação da janela perdida não encontrado no runbook")
	}
	step := runbook[start:end]
	for _, frag := range []string{"credencial de API da organização", "reconciliation.run", "reconciliation.resolve", "sem credencial da organização",
		"endereço restrito", "TOKEN="} {
		if !strings.Contains(step, frag) {
			t.Errorf("o passo da conciliação não trata de %q", frag)
		}
	}
}

func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// docSection devolve o trecho de doc entre os marcadores from e to.
func docSection(t *testing.T, doc, from, to string) string {
	t.Helper()
	start := strings.Index(doc, from)
	if start < 0 {
		t.Fatalf("trecho %q não encontrado", from)
	}
	end := strings.Index(doc[start:], to)
	if end < 0 {
		t.Fatalf("trecho %q não encontrado depois de %q", to, from)
	}
	return doc[start : start+end]
}

// O passo de guarda da rotação em duas fases diz como conferir as chaves de
// cada processo em execução: o keyring-check dentro do container (exec), que
// é o que o roteiro de recuperação de desastre faz. A forma com "run" cria um
// container novo com a configuração atual e não prova o que um processo em
// execução tem; o procedimento precisa dizer isso.
func TestKeyRotationProcedureChecksRunningProcesses(t *testing.T) {
	guide := readRepoFile(t, "docs", "operacao", "backup-e-restauracao.md")
	rotation := docSection(t, guide, "### Rotação\n", "### Restauração das chaves\n")
	guard := docSection(t, rotation, "3. Conferir que", "4. **Fase 2, ativar.**")
	for _, frag := range []string{
		"docker compose exec -T api /app keyring-check",
		"docker compose exec -T worker /app keyring-check",
		"docker compose exec -T --index 2 worker /app keyring-check",
		"docker compose ps api worker",
		"réplica",
		"`docker compose run --rm --no-deps worker keyring-check` **não serve a esta conferência**",
		"(ativa: k1)",
	} {
		if !strings.Contains(guard, frag) {
			t.Errorf("o passo de guarda da rotação não traz %q", frag)
		}
	}
	if !strings.Contains(rotation, "[Chave perdida](#chave-perdida)") {
		t.Error("a rotação não remete à chave perdida para o caso em que a chave ativa não existe mais")
	}
	// O roteiro confere do mesmo jeito: dentro do container em execução.
	script := readRepoFile(t, "deploy", "backup", "dr-check.sh")
	if !strings.Contains(script, `dc exec -T "$1" /app keyring-check`) {
		t.Error("o roteiro dr-check.sh deixou de conferir o keyring dentro do container em execução")
	}
	// O README resume o procedimento com a mesma forma.
	if readme := readRepoFile(t, "README.md"); !strings.Contains(readme, "docker compose exec -T <serviço> /app keyring-check") {
		t.Error("o README não cita a conferência de cada processo em execução na rotação")
	}
}

// Quando a chave perdida é a ativa, a rotação em duas fases não pode ser
// seguida: a fase 1 mantém a chave ativa, que não existe mais, e a
// configuração é recusada. O runbook manda trocar tudo num único passo, com
// os processos parados, declarar a perda antes de qualquer início e conferir
// com o keyring-check; a mensagem de recusa citada é a que a aplicação emite.
func TestLostKeyRunbookReplacesActiveKeyInOneStep(t *testing.T) {
	runbook := readRepoFile(t, "docs", "operacao", "runbooks", "recuperacao-de-desastre.md")
	section := docSection(t, runbook, "## Chave perdida\n", "### Verificação\n")
	actions := section[strings.Index(section, "### Ações\n"):]

	// A ordem dos passos: parar, configurar (com a declaração da perda),
	// conferir e só então iniciar.
	order := []string{
		"2. Pare a API e o worker, **todas as réplicas**",
		"3. Grave a configuração nova",
		"`RECORVIA_ENCRYPTION_KEYS`",
		"`RECORVIA_ENCRYPTION_ACTIVE_KEY`",
		"`RECORVIA_ENCRYPTION_LOST_KEYS`",
		"4. Confira antes de iniciar",
		"docker compose run --rm --no-deps worker keyring-check",
		"5. Inicie a API e o worker",
	}
	pos := 0
	for _, frag := range order {
		i := strings.Index(actions[pos:], frag)
		if i < 0 {
			t.Fatalf("as ações da chave perdida não trazem %q depois do trecho anterior", frag)
		}
		pos += i + len(frag)
	}
	for _, frag := range []string{"num único passo", "identificador novo", "**não se aplica**"} {
		if !strings.Contains(actions, frag) {
			t.Errorf("as ações da chave perdida não trazem %q", frag)
		}
	}
	if strings.Contains(actions, "seguindo a [rotação em duas fases]") {
		t.Error("o runbook manda seguir a rotação em duas fases com a chave ativa perdida")
	}

	// A fase 1 da rotação com a chave ativa perdida (dr2) é recusada pela
	// aplicação com a mensagem que o runbook e o guia citam.
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	base := map[string]string{"RECORVIA_ENV": string(config.Test), "RECORVIA_DATABASE_URL": "postgres://app@localhost/recorvia"}
	validate := func(extra map[string]string) error {
		vars := maps.Clone(base)
		maps.Copy(vars, extra)
		cfg, err := config.Load(func(k string) string { return vars[k] })
		if err != nil {
			return err
		}
		return cfg.ValidateStore()
	}
	err := validate(map[string]string{"RECORVIA_ENCRYPTION_KEYS": "dr1:" + key + ",dr3:" + key, "RECORVIA_ENCRYPTION_ACTIVE_KEY": "dr2"})
	if err == nil {
		t.Fatal("a fase 1 da rotação com a chave ativa perdida deveria ser recusada")
	}
	guide := readRepoFile(t, "docs", "operacao", "backup-e-restauracao.md")
	lost := docSection(t, guide, "### Chave perdida\n", "## RPO e RTO observados\n")
	for name, text := range map[string]string{"runbook": actions, "guia": lost} {
		text = strings.Join(strings.Fields(text), " ")
		if !strings.Contains(text, "`"+err.Error()+"`") {
			t.Errorf("o %s não cita a recusa da configuração como a aplicação a emite: %q", name, err.Error())
		}
		if !strings.Contains(text, "num único passo") {
			t.Errorf("o %s não descreve a troca num único passo", name)
		}
	}
	// A configuração do exemplo do runbook (troca num único passo) é aceita.
	if err := validate(map[string]string{"RECORVIA_ENCRYPTION_KEYS": "dr1:" + key + ",dr3:" + key,
		"RECORVIA_ENCRYPTION_ACTIVE_KEY": "dr3", "RECORVIA_ENCRYPTION_LOST_KEYS": "dr2"}); err != nil {
		t.Fatalf("a troca num único passo deveria ser aceita: %v", err)
	}
}

var (
	markdownLink   = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	markdownHeader = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	explicitAnchor = regexp.MustCompile(`<a id="([^"]+)">`)
	adrReference   = regexp.MustCompile(`ADR-(\d{3})`)
	adrHeading     = regexp.MustCompile(`(?m)^## ADR-(\d{3}) `)
	slugDropped    = regexp.MustCompile(`[^\p{L}\p{N}\s_-]`)
	fencedBlock    = regexp.MustCompile("(?s)```.*?```")
)

// docAnchors devolve as âncoras de um documento Markdown: as explícitas
// (<a id>) e as geradas dos títulos (minúsculas, sem pontuação e com hífens
// no lugar dos espaços).
func docAnchors(doc string) map[string]bool {
	anchors := map[string]bool{}
	for _, m := range explicitAnchor.FindAllStringSubmatch(doc, -1) {
		anchors[m[1]] = true
	}
	for _, m := range markdownHeader.FindAllStringSubmatch(fencedBlock.ReplaceAllString(doc, ""), -1) {
		slug := slugDropped.ReplaceAllString(strings.ToLower(m[1]), "")
		anchors[strings.ReplaceAll(slug, " ", "-")] = true
	}
	return anchors
}

// As referências cruzadas dos guias e runbooks de operação têm destino: os
// links relativos apontam para arquivos e seções que existem, e as decisões
// citadas (ADR-NNN) estão em docs/DECISOES.md. Uma limitação citada como
// registrada em outro documento precisa estar lá.
func TestOperationsDocsCrossReferencesResolve(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "operacao")
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbooks, err := filepath.Glob(filepath.Join(root, "runbooks", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	decisions := readRepoFile(t, "docs", "DECISOES.md")
	adrs := map[string]bool{}
	for _, m := range adrHeading.FindAllStringSubmatch(decisions, -1) {
		adrs[m[1]] = true
	}
	if len(adrs) == 0 {
		t.Fatal("nenhuma decisão encontrada em docs/DECISOES.md")
	}
	anchorsOf := map[string]map[string]bool{}
	links := 0
	for _, f := range append(files, runbooks...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(b)
		name := filepath.Base(f)
		for _, m := range adrReference.FindAllStringSubmatch(doc, -1) {
			if !adrs[m[1]] {
				t.Errorf("%s: cita a ADR-%s, que não está em docs/DECISOES.md", name, m[1])
			}
		}
		for _, m := range markdownLink.FindAllStringSubmatch(fencedBlock.ReplaceAllString(doc, ""), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			links++
			path, anchor, _ := strings.Cut(target, "#")
			dest := f
			if path != "" {
				dest = filepath.Join(filepath.Dir(f), path)
			}
			info, err := os.Stat(dest)
			if err != nil {
				t.Errorf("%s: o link %q aponta para um arquivo que não existe", name, target)
				continue
			}
			if anchor == "" || info.IsDir() {
				continue
			}
			if anchorsOf[dest] == nil {
				content, err := os.ReadFile(dest)
				if err != nil {
					t.Fatal(err)
				}
				anchorsOf[dest] = docAnchors(string(content))
			}
			if !anchorsOf[dest][anchor] {
				t.Errorf("%s: o link %q aponta para uma seção que não existe", name, target)
			}
		}
	}
	if links == 0 {
		t.Fatal("nenhum link relativo encontrado em docs/operacao")
	}

	// A falta de um meio de plataforma para conciliar todas as organizações
	// é citada, no guia e no runbook, como registrada na ADR-064: o registro
	// precisa existir lá e na seção "Não exercitado" do guia.
	guide := readRepoFile(t, "docs", "operacao", "backup-e-restauracao.md")
	runbook := readRepoFile(t, "docs", "operacao", "runbooks", "recuperacao-de-desastre.md")
	flat := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	adr064 := flat(docSection(t, decisions+"\n## fim", "## ADR-064 ", "\n## "))
	if !strings.Contains(adr064, "falta um meio de plataforma para conciliar todas") {
		t.Error("a ADR-064 não registra a falta de um meio de plataforma para conciliar todas as organizações")
	}
	notExercised := flat(docSection(t, guide, "### Não exercitado\n", "## Recuperação de desastre\n"))
	if !strings.Contains(notExercised, "não existe um meio de plataforma para conciliar todas") {
		t.Error("a seção Não exercitado do guia não registra a falta de um meio de plataforma de conciliação")
	}
	for name, doc := range map[string]string{"guia": guide, "runbook": runbook} {
		text := flat(doc)
		i := strings.Index(text, "meio de plataforma para conciliar todas as organizações")
		if i < 0 {
			t.Errorf("o %s não cita a falta de um meio de plataforma de conciliação", name)
			continue
		}
		sentence := text[i:min(len(text), i+260)]
		if !strings.Contains(sentence, "ADR-064") {
			t.Errorf("o %s não diz onde a limitação da conciliação por plataforma está registrada: %q", name, sentence)
		}
	}
}
