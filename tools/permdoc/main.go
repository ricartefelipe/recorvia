// Comando permdoc gera a matriz de permissões (docs/arquitetura/permissoes.md)
// a partir de internal/tenancy e, das rotas registradas em internal/httpapi, a
// lista das que aceitam apenas a sessão de um usuário. Com -check, falha se o
// documento estiver desatualizado (usado na CI).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ricartefelipe/recorvia/internal/apicredentials"
	"github.com/ricartefelipe/recorvia/internal/httpapi"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

const path = "docs/arquitetura/permissoes.md"

func render() []byte {
	var b bytes.Buffer
	b.WriteString("# Matriz de permissões\n\n")
	b.WriteString("Gerada por `go run ./tools/permdoc` a partir de `internal/tenancy/permissions.go` e das rotas\n")
	b.WriteString("registradas em `internal/httpapi`; a CI verifica que este arquivo está atualizado. Toda rota da\n")
	b.WriteString("API declara a permissão exigida (`internal/httpapi/routes.go`), e o teste de contrato garante\n")
	b.WriteString("que nenhuma rota de tenant fica sem declaração.\n\n")
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
	b.WriteString("- A equipe da plataforma (`platform_staff`) vê o catálogo de organizações, sem dados financeiros\n")
	b.WriteString("  dos clientes dos tenants, e atende os pedidos de serviços adicionais (agendar, concluir e\n")
	b.WriteString("  cancelar, com auditoria na trilha da organização; ADR-077). As rotas `/api/v1/admin/...` exigem a\n")
	b.WriteString("  sessão de um integrante da equipe. Acesso aos demais dados de um tenant exige convite do próprio\n")
	b.WriteString("  tenant e fica auditado.\n")
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
	b.WriteString("- Rotas de conta e de sessão e as rotas que exigem permissão não delegável aceitam apenas a\n")
	b.WriteString("  sessão de um usuário (credenciais recebem 403 `user_session_required`); nas demais, as ações\n")
	b.WriteString("  por credencial têm autoria nula nas tabelas e o ator `api_credential` na auditoria (ADR-059).\n")
	b.WriteString("  O portal do cliente tem sessão própria, restrita ao cliente e à organização do link.\n")
	b.WriteString("\n## Rotas restritas à sessão de usuário\n\n")
	b.WriteString("Geradas a partir das rotas registradas em `internal/httpapi`. Uma credencial de API recebe 403\n")
	b.WriteString("`user_session_required` nestas rotas antes da verificação de escopos. Todas as demais rotas de\n")
	b.WriteString("organização, inclusive as leituras de membros, papéis, convites e do plano da plataforma,\n")
	b.WriteString("aceitam credenciais pelos escopos.\n\n")
	b.WriteString("| Rota | Contexto | Permissão |\n|---|---|---|\n")
	a, _ := httpapi.NewRouter(httpapi.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Clock: clock.System{}})
	for _, r := range a.Routes() {
		if !r.UserSession {
			continue
		}
		scope, perm := "conta e sessão", "—"
		if r.Access == httpapi.TenantScoped {
			scope = "organização ativa"
		}
		if r.Permission != "" {
			perm = "`" + string(r.Permission) + "`"
		}
		fmt.Fprintf(&b, "| `%s %s` | %s | %s |\n", r.Method, r.Pattern, scope, perm)
	}
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
