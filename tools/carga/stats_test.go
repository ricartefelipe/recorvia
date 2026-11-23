package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	ms := func(v ...int) []time.Duration {
		out := make([]time.Duration, len(v))
		for i, x := range v {
			out[i] = time.Duration(x) * time.Millisecond
		}
		return out
	}
	cases := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"vazio", nil, 50, 0},
		{"um valor", ms(7), 99, 7 * time.Millisecond},
		{"mediana de dez", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 50, 5 * time.Millisecond},
		{"p95 de vinte", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20), 95, 19 * time.Millisecond},
		{"p99 de dez é o máximo", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 99, 10 * time.Millisecond},
		{"p0 é o mínimo", ms(3, 4), 0, 3 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := percentile(tc.sorted, tc.p); got != tc.want {
			t.Errorf("%s: %v, esperado %v", tc.name, got, tc.want)
		}
	}
}

// Situações esperadas (por exemplo, 409 da mesma chave com outro conteúdo)
// não contam como erro; qualquer outra situação da mesma operação conta.
func TestRecorderExpectedStatuses(t *testing.T) {
	r := newRecorder()
	r.expectStatus("conflito", http.StatusConflict, http.StatusUnprocessableEntity)
	r.begin()
	r.observe("conflito", time.Millisecond, http.StatusConflict)
	r.observe("conflito", time.Millisecond, http.StatusCreated)
	r.observe("criar", time.Millisecond, http.StatusCreated)
	r.observe("criar", time.Millisecond, http.StatusInternalServerError)
	r.observe("criar", time.Millisecond, 0)
	r.finish()
	ops, total := r.summary()
	got := map[string]int{}
	for _, o := range ops {
		got[o.Op] = o.Errors
	}
	if got["conflito"] != 1 || got["criar"] != 2 || total.Errors != 3 || total.Count != 5 {
		t.Fatalf("erros por operação %v, total %d de %d", got, total.Errors, total.Count)
	}
}

func TestParseMiB(t *testing.T) {
	cases := map[string]float64{"512MiB": 512, "1.5GiB": 1536, "2048KiB": 2, "1048576B": 1, "800kB": 800e3 / (1 << 20), "x": 0}
	for in, want := range cases {
		if d := parseMiB(in) - want; d > 1e-9 || d < -1e-9 {
			t.Errorf("parseMiB(%q) = %v, esperado %v", in, parseMiB(in), want)
		}
	}
}

func TestOrgSizes(t *testing.T) {
	got := orgSizes(20, 1)
	total, large := 0, 0
	for _, n := range got {
		total += n
		if n == 1500 {
			large++
		}
	}
	if len(got) != 20 || large != 2 || total != 6600 {
		t.Fatalf("perfil de 20 organizações: %v (total %d)", got, total)
	}
	if small := orgSizes(1, 0.001); small[0] != 2 {
		t.Fatalf("escala mínima: %v", small)
	}
}

func TestPeakRate(t *testing.T) {
	s := []billingPoint{{At: 0}, {At: 5 * time.Second, S: since{Invoices: 10}}, {At: 10 * time.Second, S: since{Invoices: 110}},
		{At: 20 * time.Second, S: since{Invoices: 120}}}
	if got := peakRate(s, 10*time.Second); got != 11 {
		t.Fatalf("pico = %v, esperado 11 faturas/s", got)
	}
}

func TestMetricsParsing(t *testing.T) {
	m, err := parseMetrics(strings.NewReader(`# HELP x
recorvia_http_requests_total{route="/a",status_class="2xx"} 10
recorvia_http_requests_total{route="/b",status_class="2xx"} 5
recorvia_http_requests_total{route="/b",status_class="5xx"} 1
recorvia_db_pool_max_connections 20
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := sumMetric(m, "recorvia_http_requests_total", `status_class="2xx"`); got != 15 {
		t.Errorf("2xx = %v", got)
	}
	if got := sumMetric(m, "recorvia_http_requests_total", `status_class="2xx"`, `route="/b"`); got != 5 {
		t.Errorf("2xx de /b = %v", got)
	}
	if got := sumMetric(m, "recorvia_db_pool_max_connections"); got != 20 {
		t.Errorf("pool = %v", got)
	}
}
