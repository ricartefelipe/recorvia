//go:build integration

package pgstore_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// runBackupScript executa um script de deploy/backup contra o container do
// teste e devolve a saída padrão, a saída de erro e o erro de execução.
func runBackupScript(t *testing.T, pg pgtest.Env, dir, name string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join("..", "..", "deploy", "backup", name)}, args...)...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+pg.DockerHost, "RECORVIA_PG_CONTAINER="+pg.ContainerID, "RECORVIA_BACKUP_DIR="+dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return strings.TrimSpace(stdout.String()), stderr.String(), err
}

// superURLFor devolve a URL do superusuário apontando para o banco informado
// (pgtest.Env.SuperURL aponta para o banco postgres).
func superURLFor(t *testing.T, pg pgtest.Env, database string) string {
	t.Helper()
	u, err := url.Parse(pg.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	return u.String()
}

func manifestValue(t *testing.T, manifest []byte, key string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + ` (.+)$`).FindSubmatch(manifest)
	if m == nil {
		t.Fatalf("manifesto sem %q", key)
	}
	return string(m[1])
}

// O procedimento de backup e restauração (deploy/backup, ADR-055) contra o
// PostgreSQL real: faturas e lançamentos do ledger emitidos pelo motor em dois
// tenants, dump com manifesto calculado no mesmo snapshot enquanto outra
// conexão grava continuamente na origem, restauração num banco novo,
// comparação do manifesto, isolamento com o papel da aplicação e remoção do
// banco de verificação. Um manifesto adulterado é detectado.
func TestBackupAndRestoreCheckScripts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatal("bash é necessário para os scripts de backup")
	}
	ctx := context.Background()
	env := fixture.New(t, 4, now)
	const perTenant = 3
	var writerTenant string
	for _, prefix := range []string{"backup-a", "backup-b"} {
		tn := env.NewTenant(t, prefix)
		writerTenant = tn.ID.String()
		plan := env.NewFlatPlan(t, tn, 12345)
		for range perTenant {
			c := env.NewCustomer(t, tn)
			env.NewSubscription(t, tn, c.ID, plan.ID, nil)
		}
		if _, err := env.Engine.RunTenant(ctx, tn.ID, tn.Actor); err != nil {
			t.Fatal(err)
		}
	}
	pg := pgtest.Start(t)
	dir := t.TempDir()
	conn, err := pgx.Connect(ctx, pg.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Escrita concorrente durante todo o backup (registros de auditoria de um
	// dos tenants): o manifesto e o dump vêm do mesmo snapshot, então a
	// restauração continua idêntica ao manifesto.
	writer, err := pgx.Connect(ctx, superURLFor(t, pg, "recorvia"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close(ctx) }()
	stop := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				written <- nil
				return
			default:
			}
			if _, err := writer.Exec(ctx, `INSERT INTO audit_log (id, tenant_id, occurred_at, actor_type, action, resource_type)
				VALUES (gen_random_uuid(), $1, now(), 'system', 'backup.concurrent_write', 'backup_test')`, writerTenant); err != nil {
				written <- err
				return
			}
		}
	}()
	dump, logs, err := runBackupScript(t, pg, dir, "backup.sh")
	close(stop)
	if werr := <-written; werr != nil {
		t.Fatalf("escrita concorrente: %v", werr)
	}
	if err != nil {
		t.Fatalf("backup: %v\n%s", err, logs)
	}
	t.Logf("backup.sh:\n%s", logs)
	base := strings.TrimSuffix(dump, ".dump")
	for _, suffix := range []string{".dump", ".manifest", ".sha256"} {
		st, err := os.Stat(base + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s com permissões %v; esperado 0600", suffix, st.Mode().Perm())
		}
	}
	manifest, err := os.ReadFile(base + ".manifest")
	if err != nil {
		t.Fatal(err)
	}
	// RECORVIA_BACKUP_EXPORT_DIR guarda uma cópia do backup para exercitar a
	// restauração em outro servidor (docs/operacao/backup-e-restauracao.md).
	if export := os.Getenv("RECORVIA_BACKUP_EXPORT_DIR"); export != "" {
		for _, suffix := range []string{".dump", ".manifest", ".sha256"} {
			b, err := os.ReadFile(base + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(export, filepath.Base(base)+suffix), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("backup copiado para %s", export)
	}
	if n, _ := strconv.Atoi(manifestValue(t, manifest, "table invoices")); n < 2*perTenant {
		t.Fatalf("manifesto com %d faturas; esperado ao menos %d", n, 2*perTenant)
	}
	if v := manifestValue(t, manifest, "goose_version"); v == "0" {
		t.Fatal("manifesto sem versão de migration")
	}
	var auditNow int
	if err := writer.QueryRow(ctx, "SELECT count(*) FROM audit_log").Scan(&auditNow); err != nil {
		t.Fatal(err)
	}
	auditAtSnapshot, _ := strconv.Atoi(manifestValue(t, manifest, "table audit_log"))
	if auditNow <= auditAtSnapshot {
		t.Fatalf("a escrita concorrente deveria continuar depois do snapshot: %d registros agora, %d no manifesto", auditNow, auditAtSnapshot)
	}
	t.Logf("audit_log: %d registros no snapshot do backup, %d na origem ao final do backup", auditAtSnapshot, auditNow)
	sample := strings.Fields(manifestValue(t, manifest, "ledger_sample"))
	if len(sample) != 7 || sample[4] != sample[5] {
		t.Fatalf("lançamento de referência inválido ou desbalanceado: %v", sample)
	}

	_, logs, err = runBackupScript(t, pg, dir, "restore-check.sh", dump)
	if err != nil {
		t.Fatalf("restore-check: %v\n%s", err, logs)
	}
	t.Logf("restore-check.sh:\n%s", logs)
	for _, want := range []string{"soma SHA-256 conferida", "isolamento conferido com recorvia_app", "restauração verificada"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("saída sem %q:\n%s", want, logs)
		}
	}

	// Manifesto adulterado (uma fatura a mais): a verificação precisa falhar.
	tampered := t.TempDir()
	for _, suffix := range []string{".dump", ".sha256"} {
		b, err := os.ReadFile(base + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tampered, filepath.Base(base)+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	invoices := manifestValue(t, manifest, "table invoices")
	n, _ := strconv.Atoi(invoices)
	altered := bytes.Replace(manifest, []byte("table invoices "+invoices+"\n"), []byte("table invoices "+strconv.Itoa(n+1)+"\n"), 1)
	if err := os.WriteFile(filepath.Join(tampered, filepath.Base(base)+".manifest"), altered, 0o600); err != nil {
		t.Fatal(err)
	}
	_, logs, err = runBackupScript(t, pg, tampered, "restore-check.sh")
	if err == nil || !strings.Contains(logs, "difere do manifesto") {
		t.Fatalf("manifesto adulterado não detectado: %v\n%s", err, logs)
	}

	// Os bancos de verificação são removidos ao final, com sucesso ou falha.
	var left int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE datname LIKE 'recorvia_restore_check_%'").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("%d bancos de verificação remanescentes", left)
	}
}
