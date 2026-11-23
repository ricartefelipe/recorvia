package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// window agrupa o que é observado durante um trecho medido: CPU e memória
// dos containers, pico das filas do RabbitMQ e as métricas da API e do
// worker no início e no fim (pool do PostgreSQL, respostas e consumo).
type window struct {
	e        env
	started  time.Time
	stats    *statsSampler
	queues   *queueSampler
	apiStart map[string]float64
	wrkStart map[string]float64
}

type windowResult struct {
	Duration   time.Duration      `json:"duration_ns"`
	Containers []containerStats   `json:"containers"`
	QueueMax   float64            `json:"queue_max_messages"`
	QueueTop   string             `json:"queue_top"`
	Deltas     map[string]float64 `json:"deltas"`
	Gauges     map[string]float64 `json:"gauges"`
}

func openWindow(ctx context.Context, e env) *window {
	w := &window{e: e, started: time.Now(), stats: startStats(e.Project), queues: startQueues(e)}
	w.apiStart, _ = metricsSnapshot(ctx, e.APIMetrics)
	w.wrkStart, _ = metricsSnapshot(ctx, e.WorkerMetrics)
	return w
}

// poolSeries são as séries do pool de conexões acompanhadas em cada serviço.
var poolSeries = []string{"recorvia_db_pool_acquires_total", "recorvia_db_pool_empty_acquires_total",
	"recorvia_db_pool_empty_acquire_wait_seconds_total", "recorvia_db_pool_canceled_acquires_total"}

func (w *window) close(ctx context.Context) windowResult {
	res := windowResult{Duration: time.Since(w.started), Deltas: map[string]float64{}, Gauges: map[string]float64{}}
	res.Containers = w.stats.finish()
	res.QueueMax, res.QueueTop = w.queues.finish()
	apiEnd, _ := metricsSnapshot(ctx, w.e.APIMetrics)
	wrkEnd, _ := metricsSnapshot(ctx, w.e.WorkerMetrics)
	for _, s := range poolSeries {
		res.Deltas["api "+s] = sumMetric(apiEnd, s) - sumMetric(w.apiStart, s)
		res.Deltas["worker "+s] = sumMetric(wrkEnd, s) - sumMetric(w.wrkStart, s)
	}
	for _, class := range []string{"2xx", "4xx", "5xx"} {
		f := `status_class="` + class + `"`
		res.Deltas["api http "+class] = sumMetric(apiEnd, "recorvia_http_requests_total", f) - sumMetric(w.apiStart, "recorvia_http_requests_total", f)
		wf := `route="/webhooks/providers/{provider}/{token}"`
		res.Deltas["api webhooks http "+class] = sumMetric(apiEnd, "recorvia_http_requests_total", f, wf) -
			sumMetric(w.apiStart, "recorvia_http_requests_total", f, wf)
	}
	for _, outcome := range []string{"accepted", "duplicate", "unauthenticated", "invalid", "error"} {
		f := `outcome="` + outcome + `"`
		res.Deltas["webhooks "+outcome] = sumMetric(apiEnd, "recorvia_provider_webhooks_total", f) -
			sumMetric(w.apiStart, "recorvia_provider_webhooks_total", f)
	}
	for _, outcome := range []string{"ack", "retry", "dead_letter", "requeue"} {
		f := `outcome="` + outcome + `"`
		res.Deltas["consumidor "+outcome] = sumMetric(wrkEnd, "recorvia_consumer_messages_total", f) -
			sumMetric(w.wrkStart, "recorvia_consumer_messages_total", f)
	}
	res.Deltas["faturas emitidas (worker)"] = sumMetric(wrkEnd, "recorvia_invoices_issued_total") - sumMetric(w.wrkStart, "recorvia_invoices_issued_total")
	res.Gauges["api pool máximo"] = sumMetric(apiEnd, "recorvia_db_pool_max_connections")
	res.Gauges["worker pool máximo"] = sumMetric(wrkEnd, "recorvia_db_pool_max_connections")
	res.Gauges["conexões no servidor"] = sumMetric(apiEnd, "recorvia_db_server_connections")
	return res
}

// writeWindow escreve CPU e memória por container, filas e pool.
func writeWindow(w *bytes.Buffer, r windowResult) {
	fmt.Fprintf(w, "\nContainers (amostras de `docker stats` a cada ~2 s; CPU em %% de um núcleo):\n\n")
	fmt.Fprintln(w, "| Container | CPU média | CPU máx | Memória máx (MiB) |")
	fmt.Fprintln(w, "|---|---:|---:|---:|")
	for _, c := range r.Containers {
		fmt.Fprintf(w, "| %s | %.0f%% | %.0f%% | %.0f |\n", c.Name, c.CPUAvg, c.CPUMax, c.MemMax)
	}
	fmt.Fprintf(w, "\nPool do PostgreSQL (máximo %.0f na API e %.0f no worker): ", r.Gauges["api pool máximo"], r.Gauges["worker pool máximo"])
	for _, svc := range []string{"api", "worker"} {
		acq := r.Deltas[svc+" recorvia_db_pool_acquires_total"]
		empty := r.Deltas[svc+" recorvia_db_pool_empty_acquires_total"]
		wait := r.Deltas[svc+" recorvia_db_pool_empty_acquire_wait_seconds_total"]
		pct := 0.0
		if acq > 0 {
			pct = 100 * empty / acq
		}
		avg := 0.0
		if empty > 0 {
			avg = 1000 * wait / empty
		}
		fmt.Fprintf(w, "%s com %.0f aquisições, %.0f (%.1f%%) esperaram conexão livre, espera média %.1f ms; ", svc, acq, empty, pct, avg)
	}
	fmt.Fprintf(w, "\n\nFilas do RabbitMQ: pico de %.0f mensagens (maior fila: %s).\n", r.QueueMax, orDash(r.QueueTop))
	fmt.Fprintf(w, "\nRespostas da API no período: 2xx %.0f, 4xx %.0f, 5xx %.0f. Mensagens do worker: confirmadas %.0f, nova tentativa %.0f, DLQ %.0f, devolvidas %.0f.\n",
		r.Deltas["api http 2xx"], r.Deltas["api http 4xx"], r.Deltas["api http 5xx"], r.Deltas["consumidor ack"],
		r.Deltas["consumidor retry"], r.Deltas["consumidor dead_letter"], r.Deltas["consumidor requeue"])
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// writeJSON grava o resultado estruturado ao lado do relatório.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
