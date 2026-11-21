//go:build integration

package pgstore_test

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/safehttp"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// dockerCLI executa o docker CLI contra o daemon do container de teste.
func dockerCLI(t *testing.T, pg pgtest.Env, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+pg.DockerHost)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), errors.New(err.Error() + ": " + strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// O exercício de recuperação de desastre, contra o Docker real, recusa um
// projeto do compose que já tenha containers e volumes criados de outra forma
// (por exemplo, uma restauração manual mantida para inspeção) e não remove
// nada dele; recursos com o rótulo do próprio exercício são aceitos.
func TestDisasterRecoveryScriptKeepsForeignComposeProject(t *testing.T) {
	for _, tool := range []string{"bash", "docker", "curl", "jq", "openssl", "sha256sum", "base64", "od", "realpath"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s é necessário para o exercício de recuperação de desastre", tool)
		}
	}
	pg := pgtest.Start(t)
	if _, err := dockerCLI(t, pg, "compose", "version", "--short"); err != nil {
		t.Skipf("plugin docker compose indisponível: %v", err)
	}
	image, err := dockerCLI(t, pg, "inspect", "-f", "{{.Config.Image}}", pg.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	// create cria, sem iniciar, um container e um volume com os rótulos que
	// o compose daria a um projeto com o nome informado.
	create := func(t *testing.T, project string, extraLabels ...string) (container, volume string) {
		t.Helper()
		volume = project + "_postgres-data"
		labels := []string{"--label", "com.docker.compose.project=" + project}
		for _, l := range extraLabels {
			labels = append(labels, "--label", l)
		}
		args := append([]string{"volume", "create"}, labels...)
		if _, err := dockerCLI(t, pg, append(args, "--label", "com.docker.compose.volume=postgres-data", volume)...); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = dockerCLI(t, pg, "volume", "rm", "-f", volume) })
		args = append([]string{"create", "--name", project + "-postgres-1"}, labels...)
		args = append(args, "--label", "com.docker.compose.service=postgres", "-v", volume+":/var/lib/postgresql", image)
		container, err := dockerCLI(t, pg, args...)
		if err != nil {
			t.Fatal(err)
		}
		// Registrada depois da remoção do volume: executa antes dela.
		t.Cleanup(func() { _, _ = dockerCLI(t, pg, "rm", "-f", container) })
		return container, volume
	}
	exists := func(kind, name string) bool {
		t.Helper()
		args := []string{"inspect", "-f", "{{.Name}}", name}
		if kind == "volume" {
			args = []string{"volume", "inspect", "-f", "{{.Name}}", name}
		}
		_, err := dockerCLI(t, pg, args...)
		return err == nil
	}
	script := filepath.Join("..", "..", "deploy", "backup", "dr-check.sh")

	t.Run("projeto criado de outra forma", func(t *testing.T) {
		project := "recorvia-teste-alheio-" + uuid.NewString()[:8]
		container, volume := create(t, project)
		out := filepath.Join(t.TempDir(), "saida")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", script)
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+pg.DockerHost, "RECORVIA_DR_PROJECT="+project, "RECORVIA_DR_DIR="+out,
			"RECORVIA_DR_REUSE_PROJECT=0", "TMPDIR="+t.TempDir())
		cmd.WaitDelay = 10 * time.Second
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("o exercício deveria recusar o projeto: %v\n%s", err, stderr.String())
		}
		if !strings.Contains(stderr.String(), "já tem containers e volumes que não foram criados pelo exercício") ||
			!strings.Contains(stderr.String(), "nada foi removido") {
			t.Fatalf("mensagem inesperada:\n%s", stderr.String())
		}
		if !exists("container", container) || !exists("volume", volume) {
			t.Fatalf("o container e o volume do projeto alheio deveriam continuar existindo:\n%s", stderr.String())
		}
		if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a recusa não deveria criar o diretório de saída: %v", err)
		}
	})
	t.Run("sobras do próprio exercício", func(t *testing.T) {
		project := "recorvia-teste-proprio-" + uuid.NewString()[:8]
		create(t, project, "recorvia.dr-check=1")
		lib := filepath.Join("..", "..", "deploy", "backup")
		cmd := exec.Command("bash", "-c", "set -euo pipefail\nsource "+filepath.Join(lib, "common.sh")+"\nsource "+filepath.Join(lib, "dr-lib.sh")+
			"\nrequire_own_project \"$1\"", "dr-lib", project)
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+pg.DockerHost, "RECORVIA_DR_REUSE_PROJECT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("recursos com o rótulo do exercício deveriam ser aceitos: %v\n%s", err, out)
		}
	})
}

// A varredura do dump contra o PostgreSQL real: um segredo gravado em claro
// numa coluna bytea sai do pg_restore em hexadecimal e não aparece como
// texto; a varredura do exercício o encontra mesmo assim, não acusa o dado
// cifrado e continua encontrando segredos em colunas de texto.
func TestDisasterRecoveryDumpScanAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	pg := pgtest.Start(t)
	database := "dr_scan_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	super, err := pgx.Connect(ctx, pg.SuperURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := super.Exec(context.Background(), "DROP DATABASE IF EXISTS "+database+" WITH (FORCE)"); err != nil {
			t.Errorf("remoção do banco da varredura: %v", err)
		}
		_ = super.Close(context.Background())
	})
	if _, err := super.Exec(ctx, "CREATE DATABASE "+database); err != nil {
		t.Fatal(err)
	}
	const inBytea, inText, absent = "sim_dr_0123456789abcdef", "whsec-gravado-em-texto-0001", "segredo-que-nao-esta-no-banco"
	conn, err := pgx.Connect(ctx, superURLFor(t, pg, database))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE contas (id int PRIMARY KEY, nome text, observacao text, api_key_ciphertext bytea)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO contas VALUES
		(1, 'Cliente A do exercício', NULL, convert_to($1, 'UTF8')),
		(2, 'Cliente B do exercício', NULL, decode('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 'hex')),
		(3, 'Cliente C do exercício', $2, NULL)`, inBytea, inText); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal(err)
	}

	sqlFile := filepath.Join(t.TempDir(), "dump.sql")
	lib := filepath.Join("..", "..", "deploy", "backup")
	run := func(script string, args ...string) string {
		t.Helper()
		full := "set -euo pipefail\nsource " + filepath.Join(lib, "common.sh") + "\nsource " + filepath.Join(lib, "dr-lib.sh") + "\n" + script
		cmd := exec.Command("bash", append([]string{"-c", full, "dr-lib", pg.ContainerID, database, sqlFile}, args...)...)
		cmd.Env = append(os.Environ(), "DOCKER_HOST="+pg.DockerHost)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", script, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String())
	}
	// O mesmo encadeamento do exercício: dump em formato custom e pg_restore
	// para SQL, dentro do container.
	run(`docker exec "$1" pg_dump -Fc -U postgres -d "$2" | docker exec -i "$1" pg_restore -f - >"$3"`)
	dump, err := os.ReadFile(sqlFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dump), "Cliente A do exercício") {
		t.Fatalf("o conteúdo do dump não foi lido:\n%s", dump)
	}
	if strings.Contains(string(dump), inBytea) {
		t.Fatal("premissa do teste: o segredo gravado numa coluna bytea não deveria aparecer como texto no dump")
	}
	cases := []struct {
		name    string
		secrets []string
		want    string
	}{
		{"segredo em claro numa coluna bytea", []string{inBytea}, "1"},
		{"segredo em claro numa coluna de texto", []string{inText}, "1"},
		{"os dois segredos", []string{absent, inBytea, inText}, "2"},
		{"segredo que não está no banco", []string{absent}, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(`count_secret_leaks "${@:4}" <"$3"`, tc.secrets...); got != tc.want {
				t.Fatalf("linhas com segredo = %s, esperado %s", got, tc.want)
			}
		})
	}
}

// A rotação em duas fases do guia de operação, com processos que convivem em
// fases diferentes sobre o banco real: o que a API já na fase 2 cifra com a
// chave nova (conta de provedor e segredo de endpoint) é lido pelo worker
// ainda na fase 1, que entrega o webhook com assinatura válida e passa pela
// conferência da inicialização. Um processo que não recebeu a chave nova (o
// que a rotação num único passo produz) encerra a entrega sem volta, não lê
// as credenciais e é recusado ao reiniciar.
func TestKeyRotationPhasesAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	iso := pgtest.Isolated(t)
	env := fixture.NewOn(t, pgtest.AppDBOf(t, iso, 8), now)
	phase1 := pgstore.New(env.DB, testKeyring(t, "t1", map[string]byte{"t1": 7, "t2": 8}), env.Clock, nil)
	phase2 := pgstore.New(env.DB, testKeyring(t, "t2", map[string]byte{"t1": 7, "t2": 8}), env.Clock, nil)
	before := env.Store // só a chave t1
	owner, err := pgxpool.New(ctx, iso.OwnerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)

	tn := env.NewTenant(t, "rotacao")
	if _, err := env.Platform.ChangePlan(ctx, tn.ID, tn.Actor, "profissional", env.PlatformVersion(t, tn.ID)); err != nil {
		t.Fatal(err)
	}
	rcv := newReceiver(t)
	res := &staticResolver{answers: map[string][]netip.Addr{"hooks.tenant.test": {netip.MustParseAddr("127.0.0.1")}}}
	client := safehttp.New(safehttp.Options{Policy: safehttp.Policy{AllowedPrivate: loopbackOnly}, Resolver: res})
	service := func(store *pgstore.Store) *webhooks.Service {
		return webhooks.NewService(store.Webhooks(), transport{client}, client, env.Clock, webhooks.Config{Lease: time.Minute,
			Timeout: 5 * time.Second, Jitter: func() float64 { return 0.5 }})
	}

	// API na fase 2: cifra com a chave nova.
	err = phase2.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
		return tx.SaveAccount(ctx, payments.ProviderAccount{ID: ids.New(), TenantID: tn.ID, Provider: "simulator", Environment: "local",
			Status: "active", IsDefault: true, APIKey: "sim_rotacao_0001", APIKeyHint: "0001", Secret: strings.Repeat("r", 40),
			CreatedAt: now, UpdatedAt: now, Version: 1}, []byte("hash-do-token-rotacao"), 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := service(phase2).CreateEndpoint(ctx, tn.ID, tn.Actor, &tn.Owner.ID, webhooks.Input{
		URL: "http://hooks.tenant.test:" + rcv.port() + "/recorvia", EventTypes: []string{outbox.EventInvoicePaid}})
	if err != nil {
		t.Fatal(err)
	}
	defaultAccount := func(store *pgstore.Store) (payments.ProviderAccount, error) {
		var acc payments.ProviderAccount
		err := store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
			var err error
			acc, err = tx.DefaultAccount(ctx)
			return err
		})
		return acc, err
	}
	deliveryStatus := func(eventID uuid.UUID) (status, lastError string) {
		t.Helper()
		var reason *string
		if err := owner.QueryRow(ctx, `SELECT status, last_error FROM webhook_deliveries WHERE tenant_id = $1 AND event_id = $2`,
			tn.ID, eventID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if reason != nil {
			lastError = *reason
		}
		return status, lastError
	}

	t.Run("worker na fase 1 lê o que a API na fase 2 cifrou", func(t *testing.T) {
		acc, err := defaultAccount(phase1)
		if err != nil || acc.APIKey != "sim_rotacao_0001" {
			t.Fatalf("credenciais cifradas com a chave nova: %+v %v", acc, err)
		}
		worker := service(phase1)
		evt := ids.New()
		if err := worker.HandleEvent(ctx, tn.ID, evt, outbox.EventInvoicePaid, now, []byte(`{"invoice_id":"x"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := worker.DeliverTenant(ctx, tn.ID); err != nil {
			t.Fatal(err)
		}
		if rcv.count() != 1 {
			t.Fatalf("entregas recebidas = %d", rcv.count())
		}
		r, body := rcv.requests[0], rcv.bodies[0]
		if err := webhooks.Verify(secret, r.Header.Get(webhooks.HeaderID), r.Header.Get(webhooks.HeaderTimestamp),
			r.Header.Get(webhooks.HeaderSignature), body, now, 5*time.Minute); err != nil {
			t.Fatalf("assinatura da entrega feita pelo worker na fase 1: %v", err)
		}
		if status, _ := deliveryStatus(evt); status != string(webhooks.Succeeded) {
			t.Fatalf("entrega = %s", status)
		}
		// Os dois processos passam pela conferência da inicialização.
		for name, store := range map[string]*pgstore.Store{"fase 1": phase1, "fase 2": phase2} {
			audit, err := store.AuditKeyring(ctx)
			if err != nil || audit.Err() != nil {
				t.Fatalf("conferência do keyring do processo na %s: %v %v", name, err, audit.Err())
			}
		}
	})

	t.Run("processo sem a chave nova encerra o trabalho sem volta", func(t *testing.T) {
		_, err := defaultAccount(before)
		if e, ok := apperr.As(err); !ok || e.Code != "provider_credentials_unreadable" || !errors.Is(err, secrets.ErrUnknownKey) {
			t.Fatalf("erro = %v, esperado credenciais ilegíveis por chave desconhecida", err)
		}
		old := service(before)
		evt := ids.New()
		if err := old.HandleEvent(ctx, tn.ID, evt, outbox.EventInvoicePaid, now, []byte(`{"invoice_id":"y"}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := old.DeliverTenant(ctx, tn.ID); err != nil {
			t.Fatal(err)
		}
		if status, lastError := deliveryStatus(evt); status != string(webhooks.Exhausted) || lastError != webhooks.ReasonSecretUnavailable {
			t.Fatalf("entrega = %s (%q), esperado esgotada por segredo indisponível", status, lastError)
		}
		if rcv.count() != 1 {
			t.Fatalf("nenhuma entrega nova deveria ter chegado ao destinatário: %d", rcv.count())
		}
		// Reiniciado no meio da rotação num único passo, o processo antigo é
		// recusado pela conferência (a chave nova não está no keyring dele).
		audit, err := before.AuditKeyring(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := audit.Err(); !errors.Is(err, secrets.ErrKeyringMismatch) || !strings.Contains(err.Error(), `chave "t2" ausente do keyring`) {
			t.Fatalf("erro = %v, esperado recusa pela chave t2 ausente", err)
		}
	})
}
