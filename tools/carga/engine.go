package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"

	"github.com/ricartefelipe/recorvia/internal/app"
	"github.com/ricartefelipe/recorvia/internal/infra/config"
)

// runEngine é o diagnóstico do motor de recorrência isolado: com o worker
// parado, cria assinaturas que vencem agora (pela API) e executa o motor
// neste processo, sem os consumidores, medindo assinaturas e faturas por
// segundo e, opcionalmente, o perfil de CPU. O worker é religado no fim.
func runEngine(ctx context.Context, log *slog.Logger, e env, args []string) error {
	fs := newFlags("motor")
	n := fs.Int("assinaturas", 300, "assinaturas novas (início hoje)")
	cpuProfile := fs.String("cpuprofile", "", "arquivo do perfil de CPU do motor")
	conc := fs.Int("concorrencia", 0, "tenants em paralelo no motor (0 usa RECORVIA_BILLING_CONCURRENCY)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := readState(e.StateFile)
	if err != nil {
		return err
	}
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	core, err := app.Open(ctx, cfg, log, "recorvia-carga-motor")
	if err != nil {
		return err
	}
	defer core.Close()
	if *conc > 0 {
		core.Engine.Concurrency = *conc
	}
	if err := stopWorker(ctx, e); err != nil {
		return err
	}
	defer func() {
		if err := startWorker(context.WithoutCancel(ctx), &e); err != nil {
			log.Error("worker não foi religado", slog.String("error", err.Error()))
		}
	}()
	hc := newHTTPClient(32)
	rec := newRecorder()
	run := randomHex(4)
	today := time.Now().In(tenantLoc).Format(time.DateOnly)
	type task struct{ org, cust int }
	var tasks []task
	for i := 0; len(tasks) < *n; i++ {
		oi := i % len(st.Orgs)
		if c := i / len(st.Orgs); c < len(st.Orgs[oi].CustomerIDs) {
			tasks = append(tasks, task{oi, c})
		}
		if i > *n*len(st.Orgs)*10 {
			break
		}
	}
	if err := parallel(ctx, log, "motor: assinaturas", 16, len(tasks), func(ctx context.Context, i int) error {
		o := st.Orgs[tasks[i].org]
		c := &apiClient{base: e.API, token: o.Token, http: hc, rec: rec}
		h := http.Header{"Idempotency-Key": {fmt.Sprintf("carga-motor-%s-%06d", run, i)}}
		_, err := c.call(ctx, "criar assinatura", http.MethodPost, "/api/v1/subscriptions", map[string]any{"customer_id": o.CustomerIDs[tasks[i].cust],
			"plan_id": o.FlatPlans[i%len(o.FlatPlans)], "quantity": 1, "start_date": today}, h, nil)
		return err
	}); err != nil {
		return err
	}
	obs, err := openObserver(ctx, log, e)
	if err != nil {
		return err
	}
	defer obs.close()
	if err := obs.resetStatements(ctx); err != nil {
		return err
	}
	if *cpuProfile != "" {
		f, err := os.Create(filepath.Clean(*cpuProfile))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		defer pprof.StopCPUProfile()
	}
	start := time.Now()
	var subs, invoices int
	for {
		res, err := core.Engine.RunAll(ctx)
		subs += res.Subscriptions
		invoices += res.Invoices
		if err != nil {
			return err
		}
		if res.Subscriptions == 0 {
			break
		}
	}
	el := time.Since(start)
	top, err := obs.topStatements(ctx, 15)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	writeStatements(&out, top)
	if _, err := os.Stdout.Write(out.Bytes()); err != nil {
		return err
	}
	log.Info("motor", slog.Int("concorrencia", core.Engine.Concurrency), slog.Int("assinaturas", subs), slog.Int("faturas", invoices), slog.Duration("tempo", el),
		slog.Float64("assinaturas_s", float64(subs)/el.Seconds()), slog.Float64("faturas_s", float64(invoices)/el.Seconds()))
	return nil
}
