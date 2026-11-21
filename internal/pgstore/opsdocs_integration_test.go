//go:build integration

package pgstore_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// docStatement é uma consulta de um bloco SQL da documentação de operação,
// com os comentários que a precedem. write indica um bloco declarado como de
// escrita (```sql escrita): comandos de alteração deliberados, como incluir
// alguém na equipe da plataforma.
type docStatement struct {
	file, comment, sql string
	write              bool
}

var (
	sqlBlock        = regexp.MustCompile("(?s)```sql( escrita)?\n(.*?)```")
	quotedParameter = regexp.MustCompile(`'<[^'>]+>'`)
	bareParameter   = regexp.MustCompile(`<[a-z_]+>`)
)

// operationsDocStatements extrai as consultas dos blocos ```sql e
// ```sql escrita de docs/operacao (guias e runbooks).
func operationsDocStatements(t *testing.T) []docStatement {
	t.Helper()
	root := filepath.Join("..", "..", "docs", "operacao")
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	runbooks, err := filepath.Glob(filepath.Join(root, "runbooks", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	var out []docStatement
	for _, f := range append(files, runbooks...) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, block := range sqlBlock.FindAllStringSubmatch(string(b), -1) {
			var comment, body []string
			write := block[1] != ""
			for line := range strings.SplitSeq(block[2], "\n") {
				trimmed := strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(trimmed, "--"):
					comment = append(comment, strings.TrimSpace(strings.TrimPrefix(trimmed, "--")))
				case trimmed != "":
					body = append(body, line)
				}
				if strings.HasSuffix(trimmed, ";") {
					out = append(out, docStatement{file: filepath.Base(f), comment: strings.Join(comment, " "), sql: strings.Join(body, "\n"), write: write})
					comment, body = nil, nil
				}
			}
			if len(body) > 0 {
				t.Errorf("%s: consulta sem ponto e vírgula final: %s", filepath.Base(f), strings.Join(body, " "))
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("nenhuma consulta encontrada em docs/operacao")
	}
	return out
}

// expectsNoRows informa se o comentário da consulta declara resultado vazio.
func (s docStatement) expectsNoRows() bool {
	return strings.Contains(s.comment, "Esperado: nenhuma linha")
}

// runDocStatement executa a consulta como superusuário, com os parâmetros de
// exemplo ('<tenant>', <pid>) trocados por NULL, e devolve o número de linhas.
// Consultas de leitura rodam numa transação somente leitura; comandos de
// escrita rodam numa transação que é sempre desfeita, o que confere nomes de
// tabelas, colunas e constraints sem alterar o banco.
func runDocStatement(ctx context.Context, conn *pgx.Conn, s docStatement) (int, error) {
	q := bareParameter.ReplaceAllString(quotedParameter.ReplaceAllString(s.sql, "NULL"), "NULL")
	mode := pgx.ReadOnly
	if s.write {
		mode = pgx.ReadWrite
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: mode})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '20s'"); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

func superConn(t *testing.T, pg pgtest.Env) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), superURLFor(t, pg, "recorvia"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// As consultas dos runbooks e do guia de backup executam contra o esquema
// real (nomes de tabelas e colunas corretos) e, num banco íntegro, as
// marcadas com "Esperado: nenhuma linha" não retornam linhas. Blocos ```sql
// só trazem leitura; alterações deliberadas ficam em blocos ```sql escrita,
// que precisam começar por INSERT, UPDATE ou DELETE e também são executados
// (e desfeitos) contra o esquema.
func TestOperationsDocsSQLRunsAgainstSchema(t *testing.T) {
	pg := pgtest.Start(t)
	conn := superConn(t, pg)
	ctx := context.Background()
	for _, s := range operationsDocStatements(t) {
		upper := strings.ToUpper(strings.TrimSpace(s.sql))
		read := strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")
		switch {
		case !s.write && !read:
			t.Errorf("%s: blocos ```sql só trazem consultas de leitura; use ```sql escrita para alterações deliberadas: %s", s.file, s.sql)
			continue
		case s.write && !strings.HasPrefix(upper, "INSERT") && !strings.HasPrefix(upper, "UPDATE") && !strings.HasPrefix(upper, "DELETE"):
			t.Errorf("%s: blocos ```sql escrita trazem só INSERT, UPDATE ou DELETE: %s", s.file, s.sql)
			continue
		}
		n, err := runDocStatement(ctx, conn, s)
		if err != nil {
			t.Errorf("%s: %v\n%s", s.file, err, s.sql)
			continue
		}
		if s.expectsNoRows() && n != 0 {
			t.Errorf("%s: %d linhas, esperado nenhuma\n%s", s.file, n, s.sql)
		}
	}
}

// As consultas do runbook de isolamento apontam as violações que procuram:
// tabela com tenant_id sem RLS e política sem o contexto de tenant.
func TestIsolationRunbookQueriesDetectViolations(t *testing.T) {
	pg := pgtest.Start(t)
	conn := superConn(t, pg)
	ctx := context.Background()
	find := func(fragments []string) docStatement {
		t.Helper()
	next:
		for _, s := range operationsDocStatements(t) {
			if s.file != "isolamento-de-dados.md" {
				continue
			}
			for _, f := range fragments {
				if !strings.Contains(s.sql, f) {
					continue next
				}
			}
			return s
		}
		t.Fatalf("consulta com %q ausente do runbook de isolamento", fragments)
		return docStatement{}
	}
	cases := []struct {
		name      string
		fragments []string
		setup     []string
	}{
		{"política sem contexto de tenant", []string{"FROM pg_policies", "NOT LIKE '%app_current_tenant()%'"}, []string{
			"CREATE TABLE runbook_vazamento (id uuid, tenant_id uuid)",
			"ALTER TABLE runbook_vazamento ENABLE ROW LEVEL SECURITY",
			"CREATE POLICY runbook_sem_tenant ON runbook_vazamento USING (true)",
		}},
		{"tabela com tenant_id sem RLS", []string{"NOT c.relrowsecurity"}, []string{"CREATE TABLE runbook_sem_rls (id uuid, tenant_id uuid)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := find(c.fragments)
			if !s.expectsNoRows() {
				t.Fatalf("o comentário da consulta deve declarar o resultado de um banco íntegro (Esperado: nenhuma linha): %q", s.comment)
			}
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			for _, stmt := range c.setup {
				if _, err := tx.Exec(ctx, stmt); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := tx.Query(ctx, s.sql)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for rows.Next() {
				n++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatalf("a violação criada deve aparecer exatamente uma vez: %d linhas", n)
			}
		})
	}
}

// O inventário da janela perdida (guia de backup) lista as cobranças que já
// existiam no backup e estão vinculadas ao provedor, cuja situação pode ter
// mudado na janela sem webhook reenviado. O teste não deixa operações
// pendentes: a recuperação agendada de outros testes percorre todos os
// tenants do banco compartilhado.
func TestRestoreWindowInventoryListsLinkedCharges(t *testing.T) {
	ctx := context.Background()
	h := newPayHarness(t, 2*time.Second)
	linked := h.requestAwaitingCharge(t, h.issueInvoice(t, 4000).ID)

	var inventory docStatement
	for _, s := range operationsDocStatements(t) {
		if s.file == "backup-e-restauracao.md" && strings.Contains(s.sql, "provider_charge_id IS NOT NULL") {
			inventory = s
		}
	}
	if inventory.sql == "" {
		t.Fatal("inventário das cobranças vinculadas ausente do guia de backup")
	}
	conn := superConn(t, pgtest.Start(t))
	rows, err := conn.Query(ctx, `SELECT referencia_externa, status FROM (`+strings.TrimSuffix(strings.TrimSpace(inventory.sql), ";")+
		`) q WHERE q.tenant_id = $1`, h.tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var status string
		if err := rows.Scan(&id, &status); err != nil {
			t.Fatal(err)
		}
		got[id] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[linked.ID] != string(payments.ChargeAwaitingPayment) {
		t.Fatalf("inventário: %v; esperado só a cobrança vinculada %s", got, linked.ID)
	}
}
