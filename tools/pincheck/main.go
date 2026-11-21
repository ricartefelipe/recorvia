// Comando pincheck verifica se as versões de dependências, ferramentas e
// imagens estão fixadas (RNF-10) e falha (código 1) ao encontrar uma
// regressão:
//
//   - action dos workflows sem SHA de 40 caracteres (uses: dono/repo@sha);
//   - dependência do package.json sem versão exata (sem ^, ~, intervalos,
//     tags ou URLs);
//   - imagem sem tag fixa (latest ou ausente) no compose, nos Dockerfiles e
//     nos Testcontainers dos testes Go;
//   - go.mod sem a diretiva toolchain com versão completa.
//
// Uso: go run ./tools/pincheck [-root .]
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	root := flag.String("root", ".", "raiz do repositório")
	flag.Parse()
	problems, err := Check(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "erro:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "%d versão(ões) sem fixação (RNF-10)\n", len(problems))
		os.Exit(1)
	}
	fmt.Println("versões fixadas: workflows, package.json, imagens (compose, Dockerfiles, Testcontainers) e toolchain do go.mod")
}

// Check devolve as violações encontradas a partir da raiz, no formato
// "arquivo:linha: motivo", em ordem.
func Check(root string) ([]string, error) {
	var problems []string
	add := func(ps []string, err error) error {
		problems = append(problems, ps...)
		return err
	}
	err := errors.Join(
		add(checkWorkflows(root)),
		add(checkPackageJSON(root, filepath.Join("web", "package.json"))),
		add(checkComposeFiles(root)),
		add(checkDockerfiles(root)),
		add(checkTestcontainers(root)),
		add(checkGoMod(root)),
	)
	sort.Strings(problems)
	return problems, err
}

// skipDir são diretórios que não pertencem ao código do projeto.
func skipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == "dist" || name == "playwright-report" || name == "test-results"
}

// walk visita os arquivos regulares sob root aceitos por match.
func walk(root string, match func(rel string, d fs.DirEntry) bool, visit func(rel string, data []byte) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !match(rel, d) {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // arquivo do próprio repositório, encontrado ao percorrer a raiz
		if err != nil {
			return err
		}
		return visit(rel, data)
	})
}

func lines(data []byte, fn func(n int, line string)) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		fn(n, sc.Text())
	}
}

// unquote remove aspas simples ou duplas e um comentário YAML ao final.
func unquote(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return v
}

var (
	usesLine = regexp.MustCompile(`^\s*(?:-\s*)?uses:\s*(.+)$`)
	fullSHA  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// checkWorkflows exige SHA completo em toda action de terceiros dos
// workflows e das actions locais.
func checkWorkflows(root string) ([]string, error) {
	var problems []string
	dir := filepath.Join(root, ".github")
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	err := walk(dir, func(rel string, _ fs.DirEntry) bool {
		return strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml")
	}, func(rel string, data []byte) error {
		lines(data, func(n int, line string) {
			m := usesLine.FindStringSubmatch(line)
			if m == nil {
				return
			}
			ref := unquote(m[1])
			where := fmt.Sprintf(".github/%s:%d", rel, n)
			switch {
			case strings.HasPrefix(ref, "./"):
				// Action local do próprio repositório.
			case strings.HasPrefix(ref, "docker://"):
				if !strings.Contains(ref, "@sha256:") {
					problems = append(problems, where+": imagem de action sem digest: "+ref)
				}
			default:
				_, version, ok := strings.Cut(ref, "@")
				if !ok || !fullSHA.MatchString(version) {
					problems = append(problems, where+": action sem SHA de 40 caracteres: "+ref)
				}
			}
		})
		return nil
	})
	return problems, err
}

// exactVersion aceita somente versões semver completas, sem operadores.
var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// checkPackageJSON exige versão exata em todas as dependências.
func checkPackageJSON(root, rel string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, rel)) //nolint:gosec // arquivo do próprio repositório
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pkg map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	var problems []string
	for _, section := range []string{"dependencies", "devDependencies", "optionalDependencies", "overrides"} {
		raw, ok := pkg[section]
		if !ok {
			continue
		}
		var deps map[string]any
		if err := json.Unmarshal(raw, &deps); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", rel, section, err)
		}
		for name, v := range deps {
			version, ok := v.(string)
			if !ok || !exactVersion.MatchString(version) {
				problems = append(problems, fmt.Sprintf("%s: %s.%s sem versão exata: %v", rel, section, name, v))
			}
		}
	}
	return problems, nil
}

// localImagePrefix identifica as imagens construídas pelos Dockerfiles do
// repositório (compose com build:): a versão delas é a do próprio código e
// as imagens base são verificadas nos Dockerfiles. Para elas basta uma tag
// explícita diferente de latest (local, dr, ci).
const localImagePrefix = "recorvia-"

// versionedTag exige que a tag comece por uma versão com ao menos dois
// componentes numéricos (18.7, v1.33.1, 4.2.9-management-alpine): tags
// flutuantes como 18, 18-alpine, alpine, lts, stable ou edge são recusadas,
// porque passam a apontar para outra versão a cada publicação.
var versionedTag = regexp.MustCompile(`^v?\d+\.\d+`)

// composeVar encontra ${VAR}, ${VAR:-padrão} e ${VAR-padrão}.
var composeVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}`)

// resolveDefaults substitui as variáveis do Compose pelo valor padrão; as
// sem padrão permanecem (e são recusadas por imageProblem).
func resolveDefaults(ref string) string {
	return composeVar.ReplaceAllStringFunc(ref, func(v string) string {
		m := composeVar.FindStringSubmatch(v)
		if strings.Contains(v, "-") {
			return m[2]
		}
		return v
	})
}

// imageProblem descreve por que a referência de imagem não está fixada, ou
// devolve "" quando está (digest sha256 ou tag de versão; para as imagens
// locais, qualquer tag explícita diferente de latest).
func imageProblem(ref string) string {
	switch {
	case ref == "":
		return "imagem vazia"
	case strings.Contains(ref, "$"):
		return "imagem com variável não resolvida: " + ref
	case strings.Contains(ref, "@sha256:"):
		return ""
	}
	name := ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		name = ref[i+1:]
	}
	repo, tag, ok := strings.Cut(name, ":")
	switch {
	case !ok || tag == "":
		return "imagem sem tag fixa: " + ref
	case tag == "latest":
		return "imagem com tag latest: " + ref
	case strings.HasPrefix(repo, localImagePrefix) && name == ref:
		return ""
	case !versionedTag.MatchString(tag):
		return "imagem com tag flutuante (use versão com ao menos major.minor): " + ref
	}
	return ""
}

var (
	imageLine = regexp.MustCompile(`^\s*image:\s*(.+)$`)
	// compose.yaml, docker-compose.yml, compose.override.yaml e arquivos
	// com prefixo (deploy/backup/dr.compose.yaml).
	composeFile = regexp.MustCompile(`(?:^|[._-])compose(?:\.[\w-]+)?\.ya?ml$`)
)

// checkComposeFiles verifica as imagens dos arquivos do Docker Compose.
func checkComposeFiles(root string) ([]string, error) {
	var problems []string
	err := walk(root, func(rel string, _ fs.DirEntry) bool {
		return composeFile.MatchString(filepath.Base(rel))
	}, func(rel string, data []byte) error {
		lines(data, func(n int, line string) {
			if m := imageLine.FindStringSubmatch(line); m != nil {
				if p := imageProblem(resolveDefaults(unquote(m[1]))); p != "" {
					problems = append(problems, fmt.Sprintf("%s:%d: %s", rel, n, p))
				}
			}
		})
		return nil
	})
	return problems, err
}

var (
	fromLine = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--platform=\S+\s+)?(\S+)(?:\s+AS\s+(\S+))?`)
	argLine  = regexp.MustCompile(`(?i)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)(?:=(\S*))?`)
	argRef   = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
)

// checkDockerfiles verifica as imagens base (FROM) de todos os
// Dockerfiles; estágios anteriores e scratch não são imagens externas.
func checkDockerfiles(root string) ([]string, error) {
	var problems []string
	err := walk(root, func(rel string, _ fs.DirEntry) bool {
		base := filepath.Base(rel)
		return base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile")
	}, func(rel string, data []byte) error {
		stages := map[string]bool{"scratch": true}
		args := map[string]string{}
		lines(data, func(n int, line string) {
			if m := argLine.FindStringSubmatch(line); m != nil {
				args[m[1]] = m[2]
				return
			}
			m := fromLine.FindStringSubmatch(line)
			if m == nil {
				return
			}
			image := argRef.ReplaceAllStringFunc(m[1], func(ref string) string {
				if v, ok := args[argRef.FindStringSubmatch(ref)[1]]; ok && v != "" {
					return v
				}
				return ref
			})
			if !stages[strings.ToLower(image)] {
				if p := imageProblem(image); p != "" {
					problems = append(problems, fmt.Sprintf("%s:%d: %s", rel, n, p))
				}
			}
			if m[2] != "" {
				stages[strings.ToLower(m[2])] = true
			}
		})
		return nil
	})
	return problems, err
}

// testcontainersPrefix identifica os pacotes do Testcontainers para Go.
const testcontainersPrefix = "github.com/testcontainers/testcontainers-go"

// goFile é um arquivo Go analisado.
type goFile struct {
	rel  string
	pkg  string
	file *ast.File
}

// checkTestcontainers verifica as imagens passadas a <pacote>.Run(ctx,
// imagem, ...) do Testcontainers e aos campos Image de requisições de
// contêiner. Constantes de texto são resolvidas pelo nome (no mesmo pacote
// ou qualificadas pelo nome do pacote); imagem vazia indica construção a
// partir de um Dockerfile (verificado à parte).
func checkTestcontainers(root string) ([]string, error) {
	fset := token.NewFileSet()
	var files []goFile
	consts := map[string]string{}
	err := walk(root, func(rel string, _ fs.DirEntry) bool {
		return strings.HasSuffix(rel, ".go") && !strings.HasPrefix(rel, "internal/dbgen/")
	}, func(rel string, data []byte) error {
		f, err := parser.ParseFile(fset, rel, data, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := f.Name.Name
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if s, ok := stringLit(vs.Values[i]); ok {
							consts[filepath.ToSlash(filepath.Dir(rel))+"."+name.Name] = s
							consts[pkg+"."+name.Name] = s
						}
					}
				}
			}
		}
		files = append(files, goFile{rel: rel, pkg: pkg, file: f})
		return nil
	})
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, gf := range files {
		tcNames := map[string]bool{}
		for _, imp := range gf.file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(path, testcontainersPrefix) {
				continue
			}
			name := filepath.Base(path)
			if path == testcontainersPrefix {
				name = "testcontainers" // nome do pacote raiz do módulo
			}
			if imp.Name != nil {
				name = imp.Name.Name
			}
			tcNames[name] = true
		}
		if len(tcNames) == 0 {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(gf.rel))
		resolve := func(e ast.Expr) (string, bool) {
			if s, ok := stringLit(e); ok {
				return s, true
			}
			switch x := e.(type) {
			case *ast.Ident:
				s, ok := consts[dir+"."+x.Name]
				return s, ok
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok {
					s, ok := consts[id.Name+"."+x.Sel.Name]
					return s, ok
				}
			}
			return "", false
		}
		report := func(e ast.Expr) {
			image, ok := resolve(e)
			pos := fset.Position(e.Pos())
			where := fmt.Sprintf("%s:%d", gf.rel, pos.Line)
			switch {
			case !ok:
				problems = append(problems, where+": imagem do Testcontainers não verificável (use texto ou constante)")
			case image == "":
				// Imagem construída a partir de um Dockerfile do repositório.
			default:
				if p := imageProblem(image); p != "" {
					problems = append(problems, where+": "+p)
				}
			}
		}
		ast.Inspect(gf.file, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Run" || len(x.Args) < 2 {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && tcNames[id.Name] {
					report(x.Args[1])
				}
			case *ast.KeyValueExpr:
				if k, ok := x.Key.(*ast.Ident); ok && k.Name == "Image" {
					report(x.Value)
				}
			}
			return true
		})
	}
	return problems, nil
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

var toolchainLine = regexp.MustCompile(`^toolchain\s+go\d+\.\d+\.\d+\s*$`)

// checkGoMod exige a diretiva toolchain com versão completa (goX.Y.Z).
func checkGoMod(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod")) //nolint:gosec // arquivo do próprio repositório
	if err != nil {
		return nil, err
	}
	found := false
	lines(data, func(_ int, line string) {
		found = found || toolchainLine.MatchString(strings.TrimSpace(line))
	})
	if !found {
		return []string{"go.mod: sem diretiva toolchain com versão completa (toolchain goX.Y.Z)"}, nil
	}
	return nil, nil
}
