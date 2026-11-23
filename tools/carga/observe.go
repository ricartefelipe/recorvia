package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// observer lê o andamento do processamento assíncrono direto no banco, com o
// superusuário do ambiente de carga e somente consultas de contagem. Não
// grava nada: os dados são criados pela API e pelos serviços.
type observer struct {
	db  *pgxpool.Pool
	log *slog.Logger
}

// observerTimeout limita cada consulta do observador: um banco parado (ou a
// máquina suspensa) não pode prender o teste numa espera sem prazo.
const observerTimeout = time.Minute

func boundCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, observerTimeout)
}

func openObserver(ctx context.Context, log *slog.Logger, e env) (*observer, error) {
	if e.ObserverDB == "" {
		return nil, fmt.Errorf("RECORVIA_LOAD_OBSERVER_DB não definido")
	}
	cfg, err := pgxpool.ParseConfig(e.ObserverDB)
	if err != nil {
		return nil, fmt.Errorf("RECORVIA_LOAD_OBSERVER_DB inválido")
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.ConnectTimeout = 15 * time.Second
	cfg.ConnConfig.RuntimeParams["application_name"] = "recorvia-carga-observador"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &observer{db: db, log: log}, nil
}

func (o *observer) close() { o.db.Close() }

// progress é uma fotografia das filas de trabalho do Recorvia.
type progress struct {
	DueSubscriptions int64 `json:"due_subscriptions"`
	Invoices         int64 `json:"invoices"`
	InvoicesPaid     int64 `json:"invoices_paid"`
	InvoicesOpen     int64 `json:"invoices_open"`
	Charges          int64 `json:"charges"`
	ChargesPaid      int64 `json:"charges_paid"`
	OutboxPending    int64 `json:"outbox_pending"`
	WebhooksPending  int64 `json:"webhooks_pending"`
	WebhooksTotal    int64 `json:"webhooks_total"`
	Payments         int64 `json:"payments"`
	DeadLetters      int64 `json:"dead_letters"`
	Notifications    int64 `json:"notifications_pending"`
}

func (o *observer) progress(ctx context.Context) (progress, error) {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	var p progress
	err := o.db.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM subscriptions WHERE next_billing_at <= now() AND status <> 'suspended'),
  (SELECT count(*) FROM invoices WHERE status <> 'draft'),
  (SELECT count(*) FROM invoices WHERE status = 'paid'),
  (SELECT count(*) FROM invoices WHERE status IN ('open', 'partially_paid')),
  (SELECT count(*) FROM charges),
  (SELECT count(*) FROM charges WHERE status = 'paid'),
  (SELECT count(*) FROM outbox_events WHERE published_at IS NULL),
  (SELECT count(*) FROM provider_webhook_events WHERE status = 'received'),
  (SELECT count(*) FROM provider_webhook_events),
  (SELECT count(*) FROM payments),
  (SELECT count(*) FROM dead_letters),
  (SELECT count(*) FROM notifications WHERE status IN ('scheduled', 'sending') AND send_after <= now())`).Scan(&p.DueSubscriptions, &p.Invoices, &p.InvoicesPaid,
		&p.InvoicesOpen, &p.Charges, &p.ChargesPaid, &p.OutboxPending, &p.WebhooksPending, &p.WebhooksTotal, &p.Payments, &p.DeadLetters,
		&p.Notifications)
	return p, err
}

// doublePaid conta faturas com mais de um pagamento confirmado por cobrança
// (efeito duplicado de um webhook repetido).
func (o *observer) doublePaid(ctx context.Context) (int64, error) {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	var n int64
	err := o.db.QueryRow(ctx, `SELECT count(*) FROM (SELECT charge_id FROM payments WHERE charge_id IS NOT NULL
  GROUP BY charge_id HAVING count(*) > 1) d`).Scan(&n)
	return n, err
}

// waitUntil consulta o progresso a cada intervalo até cond ser verdadeira ou
// o prazo acabar. Falha também quando o progresso fica parado por
// stallLimit. Sem onTick, registra o andamento a cada 30 s. Devolve a série
// de fotografias (para a taxa ao longo do tempo).
func (o *observer) waitUntil(ctx context.Context, what string, every, limit time.Duration, cond func(progress) bool,
	onTick func(time.Duration, progress)) ([]sample, error) {
	start := time.Now()
	stall := newStallGuard[progress]()
	var series []sample
	var lastLog time.Duration
	for {
		p, err := o.progress(ctx)
		if err != nil {
			return series, fmt.Errorf("%s: %w", what, err)
		}
		el := time.Since(start)
		series = append(series, sample{At: el, P: p})
		switch {
		case onTick != nil:
			onTick(el, p)
		case o.log != nil && (el-lastLog >= 30*time.Second || cond(p)):
			lastLog = el
			o.log.Info(what, slog.Duration("decorrido", el.Round(time.Second)), slog.Int64("outbox_pendente", p.OutboxPending),
				slog.Int64("webhooks_pendentes", p.WebhooksPending), slog.Int64("assinaturas_vencidas", p.DueSubscriptions))
		}
		if cond(p) {
			return series, nil
		}
		if el > limit {
			return series, fmt.Errorf("%s: prazo de %s esgotado", what, limit)
		}
		if err := stall.check(p); err != nil {
			return series, fmt.Errorf("%s: %w", what, err)
		}
		select {
		case <-ctx.Done():
			return series, ctx.Err()
		case <-time.After(every):
		}
	}
}

type sample struct {
	At time.Duration `json:"at_ns"`
	P  progress      `json:"progress"`
}

// containerStats resume CPU (% de um núcleo) e memória de um container
// durante o cenário.
type containerStats struct {
	Name    string  `json:"name"`
	Samples int     `json:"samples"`
	CPUAvg  float64 `json:"cpu_avg"`
	CPUMax  float64 `json:"cpu_max"`
	MemMax  float64 `json:"mem_max_mib"`
	MemLast float64 `json:"mem_last_mib"`
	cpuSum  float64
}

// statsSampler consulta "docker stats" dos containers do projeto em
// intervalos, em segundo plano.
type statsSampler struct {
	mu      sync.Mutex
	project string
	stats   map[string]*containerStats
	stop    chan struct{}
	done    chan struct{}
}

func startStats(project string) *statsSampler {
	s := &statsSampler{project: project, stats: map[string]*containerStats{}, stop: make(chan struct{}), done: make(chan struct{})}
	go s.loop()
	return s
}

func (s *statsSampler) loop() {
	defer close(s.done)
	if s.project == "" {
		return
	}
	for {
		s.sampleOnce()
		select {
		case <-s.stop:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *statsSampler) sampleOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	names, err := exec.CommandContext(ctx, "docker", "ps", "--filter", "label=com.docker.compose.project="+s.project, "--format", "{{.Names}}").Output() //nolint:gosec // projeto do compose do próprio teste
	if err != nil {
		return
	}
	list := strings.Fields(string(names))
	if len(list) == 0 {
		return
	}
	out, err := exec.CommandContext(ctx, "docker", append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, list...)...).Output() //nolint:gosec // containers listados acima, do próprio projeto
	if err != nil {
		return
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	s.mu.Lock()
	defer s.mu.Unlock()
	for sc.Scan() {
		var row struct {
			Name     string `json:"Name"`
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		name := strings.TrimPrefix(row.Name, s.project+"-")
		name = strings.TrimSuffix(name, "-1")
		cpu, _ := strconv.ParseFloat(strings.TrimSuffix(row.CPUPerc, "%"), 64)
		mem := parseMiB(strings.TrimSpace(strings.SplitN(row.MemUsage, "/", 2)[0]))
		c := s.stats[name]
		if c == nil {
			c = &containerStats{Name: name}
			s.stats[name] = c
		}
		c.Samples++
		c.cpuSum += cpu
		c.CPUAvg = c.cpuSum / float64(c.Samples)
		c.CPUMax = max(c.CPUMax, cpu)
		c.MemMax = max(c.MemMax, mem)
		c.MemLast = mem
	}
}

func (s *statsSampler) finish() []containerStats {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]containerStats, 0, len(s.stats))
	for _, c := range s.stats {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b containerStats) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// parseMiB converte "512.3MiB", "1.2GiB" ou "800kB" em MiB.
func parseMiB(s string) float64 {
	units := []struct {
		suffix string
		mul    float64
	}{{"GiB", 1024}, {"MiB", 1}, {"KiB", 1.0 / 1024}, {"GB", 1e9 / (1 << 20)}, {"MB", 1e6 / (1 << 20)}, {"kB", 1e3 / (1 << 20)}, {"B", 1.0 / (1 << 20)}}
	for _, u := range units {
		if v, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0
			}
			return f * u.mul
		}
	}
	return 0
}

// metricsClient lê /metrics e a API do RabbitMQ, com prazo próprio.
var metricsClient = &http.Client{Timeout: 15 * time.Second}

// metricsSnapshot lê /metrics no formato de texto do Prometheus: série
// (nome com rótulos) para valor.
func metricsSnapshot(ctx context.Context, url string) (map[string]float64, error) {
	if url == "" {
		return map[string]float64{}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := metricsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return parseMetrics(resp.Body)
}

func parseMetrics(r io.Reader) (map[string]float64, error) {
	out := map[string]float64{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i <= 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		out[line[:i]] = v
	}
	return out, sc.Err()
}

// sumMetric soma as séries de um nome cujos rótulos contêm todos os filtros
// (por exemplo `status="429"`).
func sumMetric(m map[string]float64, name string, filters ...string) float64 {
	var total float64
	for k, v := range m {
		base, labels, _ := strings.Cut(k, "{")
		if base != name {
			continue
		}
		ok := true
		for _, f := range filters {
			if !strings.Contains(labels, f) {
				ok = false
				break
			}
		}
		if ok {
			total += v
		}
	}
	return total
}

// queueSampler acompanha, em segundo plano, a soma das mensagens nas filas
// do RabbitMQ (prontas e sem confirmação) pela API de administração.
type queueSampler struct {
	mu   sync.Mutex
	e    env
	max  float64
	maxQ string
	stop chan struct{}
	done chan struct{}
}

func startQueues(e env) *queueSampler {
	q := &queueSampler{e: e, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(q.done)
		if e.RabbitAPI == "" {
			return
		}
		for {
			q.sampleOnce()
			select {
			case <-q.stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return q
}

func (q *queueSampler) sampleOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.e.RabbitAPI+"/api/queues?columns=name,messages", nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(q.e.RabbitUser, q.e.RabbitPass)
	resp, err := metricsClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var rows []struct {
		Name     string  `json:"name"`
		Messages float64 `json:"messages"`
	}
	if json.NewDecoder(resp.Body).Decode(&rows) != nil {
		return
	}
	var total, top float64
	topName := ""
	for _, r := range rows {
		total += r.Messages
		if r.Messages > top {
			top, topName = r.Messages, r.Name
		}
	}
	q.mu.Lock()
	if total > q.max {
		q.max, q.maxQ = total, topName
	}
	q.mu.Unlock()
}

func (q *queueSampler) finish() (float64, string) {
	close(q.stop)
	<-q.done
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.max, q.maxQ
}

// since conta o que o processamento assíncrono produziu a partir de um
// instante: faturas emitidas, cobranças criadas, faturas emitidas ainda sem
// cobrança e pagamentos confirmados pelo provedor.
type since struct {
	Invoices          int64 `json:"invoices"`
	Charges           int64 `json:"charges"`
	InvoicesNoCharge  int64 `json:"invoices_without_charge"`
	ProviderPayments  int64 `json:"provider_payments"`
	WebhooksProcessed int64 `json:"webhooks_processed"`
}

func (o *observer) since(ctx context.Context, t time.Time) (since, error) {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	var s since
	err := o.db.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM invoices WHERE issued_at >= $1),
  (SELECT count(*) FROM charges WHERE created_at >= $1),
  (SELECT count(*) FROM invoices i WHERE i.issued_at >= $1 AND i.amount_due > 0 AND i.status IN ('open', 'partially_paid')
      AND NOT EXISTS (SELECT 1 FROM charges c WHERE c.tenant_id = i.tenant_id AND c.invoice_id = i.id)),
  (SELECT count(*) FROM payments WHERE source = 'provider' AND created_at >= $1),
  (SELECT count(*) FROM provider_webhook_events WHERE processed_at >= $1)`, t).Scan(&s.Invoices, &s.Charges, &s.InvoicesNoCharge,
		&s.ProviderPayments, &s.WebhooksProcessed)
	return s, err
}

// usageCount conta os eventos de consumo gravados.
func (o *observer) usageCount(ctx context.Context) (int64, error) {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	var n int64
	err := o.db.QueryRow(ctx, `SELECT count(*) FROM usage_events`).Scan(&n)
	return n, err
}

// statement é uma linha de pg_stat_statements (somente neste ambiente: a
// extensão é carregada pelo deploy/carga/carga.compose.yaml).
type statement struct {
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	MaxMS   float64 `json:"max_ms"`
	Rows    int64   `json:"rows"`
	Query   string  `json:"query"`
}

// resetStatements cria a extensão no banco do Recorvia (se ausente) e zera as
// estatísticas, para que cada cenário veja só as próprias consultas.
func (o *observer) resetStatements(ctx context.Context) error {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	if _, err := o.db.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err != nil {
		return err
	}
	_, err := o.db.Exec(ctx, `SELECT pg_stat_statements_reset()`)
	return err
}

// topStatements devolve as consultas de maior tempo total desde o último
// reset, sem as do próprio observador.
func (o *observer) topStatements(ctx context.Context, n int) ([]statement, error) {
	ctx, cancel := boundCtx(ctx)
	defer cancel()
	rows, err := o.db.Query(ctx, `SELECT s.calls, s.total_exec_time, s.mean_exec_time, s.max_exec_time, s.rows,
       left(regexp_replace(s.query, '\s+', ' ', 'g'), 220)
  FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
 WHERE r.rolname <> current_user
 ORDER BY s.total_exec_time DESC LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []statement
	for rows.Next() {
		var s statement
		if err := rows.Scan(&s.Calls, &s.TotalMS, &s.MeanMS, &s.MaxMS, &s.Rows, &s.Query); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func writeStatements(w *bytes.Buffer, st []statement) {
	if len(st) == 0 {
		return
	}
	fmt.Fprintf(w, "\nConsultas de maior tempo total no PostgreSQL (pg_stat_statements):\n\n")
	fmt.Fprintln(w, "| Chamadas | Total (ms) | Média (ms) | Máx (ms) | Linhas | Consulta |")
	fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---|")
	for _, s := range st {
		q := strings.ReplaceAll(s.Query, "|", "\\|")
		fmt.Fprintf(w, "| %d | %.0f | %.2f | %.1f | %d | `%s` |\n", s.Calls, s.TotalMS, s.MeanMS, s.MaxMS, s.Rows, q)
	}
}
