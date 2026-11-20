// Comando permdoc gera a matriz de permissões (docs/arquitetura/permissoes.md)
// a partir de internal/tenancy. Com -check, falha se o documento estiver
// desatualizado (usado na CI).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

const path = "docs/arquitetura/permissoes.md"

func render() []byte {
	var b bytes.Buffer
	b.WriteString("# Matriz de permissões\n\n")
	b.WriteString("Gerada por `go run ./tools/permdoc` a partir de `internal/tenancy/permissions.go`; a CI verifica\n")
	b.WriteString("que este arquivo está atualizado. Toda rota da API declara a permissão exigida\n")
	b.WriteString("(`internal/httpapi/routes.go`), e o teste de contrato garante que nenhuma rota de tenant\n")
	b.WriteString("fica sem declaração.\n\n")
	b.WriteString("O acesso à organização é validado a cada requisição contra a associação ativa do usuário;\n")
	b.WriteString("o identificador de tenant recebido nunca é aceito isoladamente.\n\n")
	b.WriteString("| Permissão |")
	roles := tenancy.Roles()
	for _, r := range roles {
		fmt.Fprintf(&b, " %s |", r)
	}
	b.WriteString("\n|---|")
	for range roles {
		b.WriteString(":-:|")
	}
	b.WriteString("\n")
	for _, p := range tenancy.AllPermissions() {
		fmt.Fprintf(&b, "| `%s` |", p)
		for _, r := range roles {
			if r.Can(p) {
				b.WriteString(" ✔ |")
			} else {
				b.WriteString(" — |")
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("\nPapéis: `owner` (proprietário), `admin` (administrador), `finance` (financeiro),\n")
	b.WriteString("`operator` (operador) e `auditor` (somente leitura, incluindo auditoria).\n\n")
	b.WriteString("Regras adicionais aplicadas nos casos de uso:\n\n")
	b.WriteString("- Somente proprietários convidam, promovem, rebaixam ou removem proprietários.\n")
	b.WriteString("- A organização mantém ao menos um proprietário ativo.\n")
	b.WriteString("- O plano da plataforma (`platform_billing.manage`) é exclusivo do proprietário.\n")
	b.WriteString("- A equipe da plataforma (`platform_staff`) vê apenas o catálogo de organizações, sem dados\n")
	b.WriteString("  financeiros; acesso a dados de um tenant exige convite do próprio tenant e fica auditado.\n")
	b.WriteString("- Credenciais de API recebem como escopos somente permissões de quem as emite; os escopos\n")
	b.WriteString("  efetivos acompanham o papel atual dessa pessoa.\n")
	b.WriteString("  Não são delegáveis:")
	for i, p := range apicredentials.NonDelegable() {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, " `%s`", p)
	}
	b.WriteString(".\n")
	b.WriteString("- Rotas de conta e de sessão aceitam apenas a sessão de um usuário; o portal do cliente tem\n")
	b.WriteString("  sessão própria, restrita ao cliente e à organização do link.\n")
	return b.Bytes()
}

func main() {
	check := flag.Bool("check", false, "falha se o documento estiver desatualizado")
	flag.Parse()
	want := render()
	if *check {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			fmt.Fprintln(os.Stderr, path+" desatualizado: execute go run ./tools/permdoc")
			os.Exit(1)
		}
		return
	}
	if err := os.WriteFile(path, want, 0o644); err != nil { //nolint:gosec // documento público do repositório
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
