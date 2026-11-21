package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

func okDB(context.Context) error   { return nil }
func failDB(context.Context) error { return errors.New("conexão recusada") }

func TestWorkerHealthMuxSeparatesLivenessFromReadiness(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("recorvia_up 1\n")) })
	cases := []struct {
		name       string
		probes     WorkerProbes
		path       string
		wantCode   int
		wantStatus string
		wantChecks map[string]string
	}{
		{"liveness sem dependências", WorkerProbes{Database: failDB, Broker: func() bool { return false }}, "/livez", http.StatusOK, "ok", nil},
		{"pronto com banco e broker", WorkerProbes{Database: okDB, Broker: func() bool { return true }}, "/readyz", http.StatusOK, "ok",
			map[string]string{"postgres": "ok", "rabbitmq": "ok"}},
		{"banco indisponível", WorkerProbes{Database: failDB, Broker: func() bool { return true }}, "/readyz", http.StatusServiceUnavailable, "unavailable",
			map[string]string{"postgres": "unavailable", "rabbitmq": "ok"}},
		{"broker desconectado", WorkerProbes{Database: okDB, Broker: func() bool { return false }}, "/readyz", http.StatusServiceUnavailable, "unavailable",
			map[string]string{"postgres": "ok", "rabbitmq": "unavailable"}},
		{"verificações ausentes não indicam prontidão", WorkerProbes{}, "/readyz", http.StatusServiceUnavailable, "unavailable",
			map[string]string{"postgres": "unavailable", "rabbitmq": "unavailable"}},
		{"encerramento em andamento", WorkerProbes{Database: okDB, Broker: func() bool { return true }, ShuttingDown: func() bool { return true }},
			"/readyz", http.StatusServiceUnavailable, "unavailable", map[string]string{"postgres": "ok", "rabbitmq": "ok", "worker": "shutting_down"}},
		{"liveness durante o encerramento", WorkerProbes{ShuttingDown: func() bool { return true }}, "/livez", http.StatusOK, "ok", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WorkerHealthMux(metrics, tc.probes).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, esperado %d", rec.Code, tc.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("Content-Type = %q", ct)
			}
			var body struct {
				Status string            `json:"status"`
				Checks map[string]string `json:"checks"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status != tc.wantStatus || len(body.Checks) != len(tc.wantChecks) {
				t.Fatalf("corpo %+v", body)
			}
			for k, v := range tc.wantChecks {
				if body.Checks[k] != v {
					t.Fatalf("verificação %s = %q, esperado %q", k, body.Checks[k], v)
				}
			}
		})
	}
}

func TestWorkerHealthMuxServesMetricsAndRejectsOtherMethods(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("recorvia_up 1\n")) })
	mux := WorkerHealthMux(metrics, WorkerProbes{Database: okDB, Broker: func() bool { return true }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "recorvia_up") {
		t.Fatalf("métricas: %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/readyz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /readyz: %d", rec.Code)
	}
}

func TestHealthcheckQueriesLiveness(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/livez" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Healthcheck("", ":"+port); err != nil {
		t.Fatalf("endereço padrão com porta: %v", err)
	}
	if err := Healthcheck(strings.TrimPrefix(srv.URL, "http://"), ":1"); err != nil {
		t.Fatalf("endereço explícito: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/livez" {
		t.Fatalf("caminhos consultados: %v", paths)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	if err := Healthcheck(strings.TrimPrefix(unhealthy.URL, "http://"), ""); err == nil {
		t.Fatal("processo com /livez diferente de 200 deveria falhar")
	}
	if err := Healthcheck("127.0.0.1:1", ""); err == nil {
		t.Fatal("porta sem processo deveria falhar")
	}
}

func TestDBPoolStatsReadsPool(t *testing.T) {
	// O pool não conecta na criação: as estatísticas refletem a configuração.
	cfg, err := pgxpool.ParseConfig("postgres://recorvia_app@127.0.0.1:1/recorvia?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s := DBPoolStats(postgres.New(pool))()
	if s.Max != 7 || s.Acquired != 0 || s.Acquires != 0 {
		t.Fatalf("estatísticas %+v", s)
	}
}

// As contagens do banco chegam inteiras às métricas das execuções de
// conciliação em andamento.
func TestReconciliationRunStatsMapsEveryState(t *testing.T) {
	got := reconciliationRunStats(pgstore.ReconciliationRunStats{Queued: 3, Executing: 2, Stalled: 1, OldestWaitingSeconds: 412.5})
	if want := (telemetry.ReconciliationRunStats{Queued: 3, Executing: 2, Stalled: 1, OldestWaitingSeconds: 412.5}); got != want {
		t.Fatalf("contagens %+v, esperado %+v", got, want)
	}
}

// Sem acesso ao banco, a leitura feita pela API a cada coleta devolve o erro
// (as séries mantêm o último valor) e registra a falha no log, sem travar a
// exposição das métricas.
func TestReconciliationRunStatsReportsReadFailure(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://recorvia_app@127.0.0.1:1/recorvia?sslmode=disable&connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var logs bytes.Buffer
	store := pgstore.New(postgres.New(pool), nil, clock.NewManual(time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)), nil)
	metrics := telemetry.NewMetrics("api")
	metrics.SetReconciliationRuns(telemetry.ReconciliationRunStats{Queued: 1, OldestWaitingSeconds: 480})
	metrics.ObserveReconciliationRuns(5*time.Second, ReconciliationRunStats(store, slog.New(slog.NewTextHandler(&logs, nil))))
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() != "recorvia_reconciliation_oldest_waiting_seconds" {
			continue
		}
		found = true
		if v := f.GetMetric()[0].GetGauge().GetValue(); v != 480 {
			t.Fatalf("espera mais longa = %v, esperado o último valor (480)", v)
		}
	}
	if !found {
		t.Fatal("série recorvia_reconciliation_oldest_waiting_seconds ausente")
	}
	if !strings.Contains(logs.String(), "leitura das execuções de conciliação em andamento falhou") {
		t.Fatalf("falha de leitura sem registro no log: %q", logs.String())
	}
}
