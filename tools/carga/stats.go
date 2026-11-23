package main

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// recorder acumula a latência e o desfecho de cada operação medida. É seguro
// para uso concorrente.
type recorder struct {
	mu    sync.Mutex
	start time.Time
	end   time.Time
	ops   map[string]*opStats
	// expect lista, por operação, as situações esperadas que não contam como
	// erro (por exemplo, 422 da mesma Idempotency-Key com outro conteúdo).
	expect map[string]map[int]bool
}

type opStats struct {
	durations []time.Duration
	errors    int
	statuses  map[int]int
}

func newRecorder() *recorder {
	return &recorder{ops: map[string]*opStats{}, expect: map[string]map[int]bool{}}
}

// expectStatus marca as situações da operação que são o resultado esperado,
// mesmo com 400 ou acima; qualquer outra situação conta como erro.
func (r *recorder) expectStatus(op string, statuses ...int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[int]bool{}
	for _, s := range statuses {
		m[s] = true
	}
	r.expect[op] = m
}

// begin e finish delimitam a janela usada no cálculo da vazão.
func (r *recorder) begin() {
	r.mu.Lock()
	r.start = time.Now()
	r.mu.Unlock()
}

func (r *recorder) finish() {
	r.mu.Lock()
	r.end = time.Now()
	r.mu.Unlock()
}

// observe registra uma operação. status 0 indica falha de transporte; status
// 400 ou acima conta como erro.
func (r *recorder) observe(op string, d time.Duration, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.ops[op]
	if s == nil {
		s = &opStats{statuses: map[int]int{}}
		r.ops[op] = s
	}
	s.durations = append(s.durations, d)
	s.statuses[status]++
	if exp, ok := r.expect[op]; ok {
		if !exp[status] {
			s.errors++
		}
	} else if status == 0 || status >= 400 {
		s.errors++
	}
}

// opSummary é o resumo de uma operação.
type opSummary struct {
	Op        string        `json:"op"`
	Count     int           `json:"count"`
	Errors    int           `json:"errors"`
	ErrorRate float64       `json:"error_rate"`
	RPS       float64       `json:"rps"`
	P50       time.Duration `json:"p50_ns"`
	P95       time.Duration `json:"p95_ns"`
	P99       time.Duration `json:"p99_ns"`
	Max       time.Duration `json:"max_ns"`
	Statuses  map[int]int   `json:"statuses"`
	Window    time.Duration `json:"window_ns"`
}

// summary devolve o resumo por operação (ordem alfabética) e o total.
func (r *recorder) summary() (ops []opSummary, total opSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
	window := r.end.Sub(r.start)
	if r.end.IsZero() {
		window = time.Since(r.start)
	}
	var all []time.Duration
	total = opSummary{Op: "total", Statuses: map[int]int{}, Window: window}
	names := make([]string, 0, len(r.ops))
	for n := range r.ops {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s := r.ops[n]
		sum := summarize(n, s.durations, s.errors, window)
		sum.Statuses = copyCounts(s.statuses)
		ops = append(ops, sum)
		all = append(all, s.durations...)
		total.Errors += s.errors
		for k, v := range s.statuses {
			total.Statuses[k] += v
		}
	}
	t := summarize("total", all, total.Errors, window)
	t.Statuses = total.Statuses
	return ops, t
}

func copyCounts(m map[int]int) map[int]int {
	out := make(map[int]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// summarize calcula os percentis (método do posto mais próximo), a taxa de
// erro e a vazão na janela.
func summarize(op string, durations []time.Duration, errors int, window time.Duration) opSummary {
	d := slices.Clone(durations)
	slices.Sort(d)
	s := opSummary{Op: op, Count: len(d), Errors: errors, Window: window}
	if len(d) == 0 {
		return s
	}
	s.P50, s.P95, s.P99, s.Max = percentile(d, 50), percentile(d, 95), percentile(d, 99), d[len(d)-1]
	s.ErrorRate = float64(errors) / float64(len(d))
	if window > 0 {
		s.RPS = float64(len(d)) / window.Seconds()
	}
	return s
}

// percentile devolve o percentil p (0 a 100) de uma lista ordenada pelo método
// do posto mais próximo: o menor valor com pelo menos p% das amostras até ele.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	rank := int(p/100*float64(len(sorted)) + 0.999999999)
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

// writeTable escreve o resumo em tabela Markdown.
func writeTable(w *bytes.Buffer, ops []opSummary, total opSummary) {
	fmt.Fprintln(w, "| Operação | Requisições | Vazão (req/s) | p50 (ms) | p95 (ms) | p99 (ms) | máx (ms) | Erros | Situações |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|---|")
	for _, s := range append(ops, total) {
		fmt.Fprintf(w, "| %s | %d | %.1f | %s | %s | %s | %s | %d (%.2f%%) | %s |\n", s.Op, s.Count, s.RPS, ms(s.P50), ms(s.P95), ms(s.P99),
			ms(s.Max), s.Errors, 100*s.ErrorRate, statusText(s.Statuses))
	}
}

func statusText(m map[int]int) string {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := fmt.Sprint(k)
		if k == 0 {
			label = "falha"
		}
		parts = append(parts, fmt.Sprintf("%s: %d", label, m[k]))
	}
	return strings.Join(parts, ", ")
}
