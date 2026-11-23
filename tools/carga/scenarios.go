package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ricartefelipe/recorvia/internal/paysim"
)

// scenarioCtx reúne o que todo cenário usa.
type scenarioCtx struct {
	log *slog.Logger
	e   env
	st  *state
	obs *observer
	hc  *http.Client
}

// orgPicker sorteia organizações com peso proporcional ao número de
// clientes: as maiores recebem mais tráfego, como numa base real.
type orgPicker struct {
	cum   []int
	total int
}

func newOrgPicker(orgs []org) orgPicker {
	p := orgPicker{cum: make([]int, len(orgs))}
	for i, o := range orgs {
		p.total += max(1, o.Customers)
		p.cum[i] = p.total
	}
	return p
}

func (p orgPicker) pick(r *rand.Rand) int {
	n := r.IntN(p.total)
	for i, c := range p.cum {
		if n < c {
			return i
		}
	}
	return len(p.cum) - 1
}

// runFor executa fn em conc goroutines até o prazo d acabar. O prazo só
// impede novas iterações: as requisições em andamento terminam (cancelá-las
// apareceria como erro no servidor sem ser um erro do sistema medido).
func runFor(ctx context.Context, conc int, d time.Duration, fn func(ctx context.Context, r *rand.Rand)) {
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for w := range conc {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(w))) //nolint:gosec // sorteio das operações da carga
			for ctx.Err() == nil && time.Now().Before(deadline) {
				fn(ctx, r)
			}
		})
	}
	wg.Wait()
}

func runScenario(ctx context.Context, log *slog.Logger, e env, name string, args []string) error {
	st, err := readState(e.StateFile)
	if err != nil {
		return err
	}
	obs, err := openObserver(ctx, log, e)
	if err != nil {
		return err
	}
	defer obs.close()
	sc := &scenarioCtx{log: log, e: e, st: st, obs: obs, hc: newHTTPClient(256)}
	switch name {
	case "leitura":
		return sc.read(ctx, args)
	case "escrita":
		return sc.write(ctx, args)
	case "faturamento":
		return sc.billing(ctx, args)
	case "consumo":
		return sc.usage(ctx, args)
	case "webhooks":
		return sc.webhooks(ctx, args)
	}
	return fmt.Errorf("cenário desconhecido: %s", name)
}

func (sc *scenarioCtx) client(o org, rec *recorder) *apiClient {
	return &apiClient{base: sc.e.API, token: o.Token, http: sc.hc, rec: rec}
}

// report devolve o relatório Markdown do cenário, montado em memória, e a
// função que o grava, com o JSON do resultado ao lado.
func (sc *scenarioCtx) report(name string) (*bytes.Buffer, func(any) error) {
	f := &bytes.Buffer{}
	return f, func(v any) error {
		path, err := outPath(sc.e, name+".md")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Bytes(), 0o600); err != nil {
			return err
		}
		return writeJSON(strings.TrimSuffix(path, ".md")+".json", v)
	}
}

// statements zera as estatísticas do PostgreSQL antes de um trecho medido;
// a falha não interrompe o cenário (o relatório só fica sem a tabela).
func (sc *scenarioCtx) resetStatements(ctx context.Context) {
	if err := sc.obs.resetStatements(ctx); err != nil {
		sc.log.Warn("pg_stat_statements indisponível", slog.String("error", err.Error()))
	}
}

func (sc *scenarioCtx) topStatements(ctx context.Context) []statement {
	st, err := sc.obs.topStatements(ctx, 12)
	if err != nil {
		sc.log.Warn("pg_stat_statements indisponível", slog.String("error", err.Error()))
	}
	return st
}

type levelResult struct {
	Concurrency int            `json:"concurrency"`
	Ops         []opSummary    `json:"ops"`
	Total       opSummary      `json:"total"`
	Window      windowResult   `json:"window"`
	Statements  []statement    `json:"statements"`
	Extra       map[string]any `json:"extra,omitempty"`
}

// ---------- (a) leitura ----------

// read mede as listas paginadas (clientes, faturas, assinaturas) e o painel,
// com várias organizações ao mesmo tempo, em níveis crescentes de
// concorrência.
func (sc *scenarioCtx) read(ctx context.Context, args []string) error {
	fs := newFlags("leitura")
	levels := fs.String("niveis", "8,32,64", "usuários simultâneos de cada nível")
	dur := fs.Duration("duracao", 45*time.Second, "duração de cada nível")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conc, err := intList(*levels)
	if err != nil {
		return err
	}
	picker := newOrgPicker(sc.st.Orgs)
	var results []levelResult
	for _, c := range conc {
		sc.log.Info("leitura", slog.Int("concorrencia", c), slog.Duration("duracao", *dur))
		rec := newRecorder()
		sc.resetStatements(ctx)
		w := openWindow(ctx, sc.log, sc.e)
		rec.begin()
		runFor(ctx, c, *dur, func(ctx context.Context, r *rand.Rand) {
			o := sc.st.Orgs[picker.pick(r)]
			cl := sc.client(o, rec)
			switch n := r.IntN(100); {
			case n < 30:
				sc.listPages(ctx, cl, r, "clientes", "/api/v1/customers", url.Values{"limit": {"25"}})
			case n < 60:
				q := url.Values{"limit": {"25"}}
				if r.IntN(3) == 0 {
					q.Set("status", "open")
				}
				sc.listPages(ctx, cl, r, "faturas", "/api/v1/invoices", q)
			case n < 80:
				sc.listPages(ctx, cl, r, "assinaturas", "/api/v1/subscriptions", url.Values{"limit": {"25"}})
			default:
				_, _ = cl.call(ctx, "painel", http.MethodGet, "/api/v1/dashboard", nil, nil, nil)
			}
		})
		rec.finish()
		ops, total := rec.summary()
		results = append(results, levelResult{Concurrency: c, Ops: ops, Total: total, Window: w.close(ctx), Statements: sc.topStatements(ctx)})
		sc.log.Info("leitura concluída", slog.Int("concorrencia", c), slog.Float64("req_s", total.RPS), slog.Duration("p95", total.P95),
			slog.Int("erros", total.Errors))
	}
	f, save := sc.report("10-leitura")
	fmt.Fprintf(f, "## Cenário (a): leitura pela API\n\n")
	fmt.Fprintf(f, "Credencial de API (Bearer) de %d organizações, sorteadas com peso pelo número de clientes. Mistura: 30%% lista de clientes,"+
		" 30%% lista de faturas (um terço filtrado por em aberto), 20%% lista de assinaturas e 20%% painel; 30%% das listas seguem o cursor"+
		" por mais uma ou duas páginas. %s por nível, sem pausa entre as requisições (cada usuário simultâneo é um laço fechado).\n",
		len(sc.st.Orgs), *dur)
	for _, l := range results {
		fmt.Fprintf(f, "\n### %d usuários simultâneos\n\n", l.Concurrency)
		writeTable(f, l.Ops, l.Total)
		writeWindow(f, l.Window)
		writeStatements(f, l.Statements)
	}
	return save(results)
}

// listPages lê a primeira página e, em 30% das vezes, segue o cursor por mais
// uma ou duas páginas.
func (sc *scenarioCtx) listPages(ctx context.Context, cl *apiClient, r *rand.Rand, name, path string, q url.Values) {
	var pg page[json.RawMessage]
	if _, err := cl.call(ctx, "listar "+name, http.MethodGet, path+"?"+q.Encode(), nil, nil, &pg); err != nil || r.IntN(10) >= 3 {
		return
	}
	for range 1 + r.IntN(2) {
		if pg.NextCursor == nil || ctx.Err() != nil {
			return
		}
		q.Set("cursor", *pg.NextCursor)
		pg = page[json.RawMessage]{}
		if _, err := cl.call(ctx, "listar "+name+" (página seguinte)", http.MethodGet, path+"?"+q.Encode(), nil, nil, &pg); err != nil {
			return
		}
	}
}

// ---------- (b) escrita com Idempotency-Key ----------

// write cria clientes e assinaturas com Idempotency-Key e repete parte dos
// pedidos: com o mesmo conteúdo (deve devolver o mesmo recurso) e com outro
// conteúdo (deve ser recusado).
func (sc *scenarioCtx) write(ctx context.Context, args []string) error {
	fs := newFlags("escrita")
	levels := fs.String("niveis", "4,16", "clientes simultâneos de cada nível")
	dur := fs.Duration("duracao", 40*time.Second, "duração de cada nível")
	if err := fs.Parse(args); err != nil {
		return err
	}
	conc, err := intList(*levels)
	if err != nil {
		return err
	}
	picker := newOrgPicker(sc.st.Orgs)
	run := randomHex(4)
	var seq atomic.Int64
	var results []levelResult
	const (
		opCustomer = "criar cliente"
		opSub      = "criar assinatura"
		opReplay   = "repetir assinatura (mesma chave e conteúdo)"
		opConflict = "mesma chave com outro conteúdo"
	)
	for _, c := range conc {
		sc.log.Info("escrita", slog.Int("concorrencia", c), slog.Duration("duracao", *dur))
		rec := newRecorder()
		rec.expectStatus(opConflict, http.StatusUnprocessableEntity, http.StatusConflict)
		var mismatches, replays atomic.Int64
		sc.resetStatements(ctx)
		w := openWindow(ctx, sc.log, sc.e)
		rec.begin()
		runFor(ctx, c, *dur, func(ctx context.Context, r *rand.Rand) {
			oi := picker.pick(r)
			o := sc.st.Orgs[oi]
			cl := sc.client(o, rec)
			n := int(seq.Add(1))
			body := customerBody(o.Slug, oi+1, 50000+n%50000)
			body["external_id"] = fmt.Sprintf("%s-escrita-%s-%06d", o.Slug, run, n)
			var cust idResp
			hc := http.Header{"Idempotency-Key": {fmt.Sprintf("carga-escrita-%s-c-%06d", run, n)}}
			if _, err := cl.call(ctx, opCustomer, http.MethodPost, "/api/v1/customers", body, hc, &cust); err != nil {
				return
			}
			plan := o.FlatPlans[r.IntN(len(o.FlatPlans))]
			subBody := map[string]any{"customer_id": cust.ID, "plan_id": plan, "quantity": 1}
			hs := http.Header{"Idempotency-Key": {fmt.Sprintf("carga-escrita-%s-s-%06d", run, n)}}
			var sub idResp
			if _, err := cl.call(ctx, opSub, http.MethodPost, "/api/v1/subscriptions", subBody, hs, &sub); err != nil {
				return
			}
			switch k := r.IntN(10); {
			case k < 2:
				var again idResp
				if _, err := cl.call(ctx, opReplay, http.MethodPost, "/api/v1/subscriptions", subBody, hs, &again); err == nil {
					replays.Add(1)
					if again.ID != sub.ID {
						mismatches.Add(1)
					}
				}
			case k < 3:
				other := map[string]any{"customer_id": cust.ID, "plan_id": o.FlatPlans[(slices.Index(o.FlatPlans, plan)+1)%len(o.FlatPlans)],
					"quantity": 1}
				_, _ = cl.call(ctx, opConflict, http.MethodPost, "/api/v1/subscriptions", other, hs, nil)
			}
		})
		rec.finish()
		ops, total := rec.summary()
		results = append(results, levelResult{Concurrency: c, Ops: ops, Total: total, Window: w.close(ctx), Statements: sc.topStatements(ctx),
			Extra: map[string]any{"replays": replays.Load(), "replay_mismatches": mismatches.Load()}})
		sc.log.Info("escrita concluída", slog.Int("concorrencia", c), slog.Float64("req_s", total.RPS), slog.Duration("p95", total.P95),
			slog.Int("erros", total.Errors), slog.Int64("repeticoes_divergentes", mismatches.Load()))
	}
	f, save := sc.report("20-escrita")
	fmt.Fprintf(f, "## Cenário (b): escrita com Idempotency-Key\n\n")
	fmt.Fprintf(f, "Cada iteração cria um cliente e uma assinatura (início hoje), ambos com Idempotency-Key própria. Em 20%% das iterações a"+
		" criação da assinatura é repetida com a mesma chave e o mesmo conteúdo (deve devolver a mesma assinatura); em 10%%, com a mesma"+
		" chave e outro conteúdo (deve ser recusada com 422 ou 409, que aqui não contam como erro). O worker segue ligado e fatura as"+
		" assinaturas novas. %s por nível.\n", *dur)
	for _, l := range results {
		fmt.Fprintf(f, "\n### %d clientes simultâneos\n\n", l.Concurrency)
		writeTable(f, l.Ops, l.Total)
		fmt.Fprintf(f, "\nRepetições com a mesma chave: %d; devolveram outra assinatura: %d.\n", l.Extra["replays"], l.Extra["replay_mismatches"])
		writeWindow(f, l.Window)
		writeStatements(f, l.Statements)
	}
	return save(results)
}

// ---------- (c) faturamento em lote ----------

// simCharge é a cobrança como o simulador a expõe na API do comerciante
// (com os reembolsos, o mesmo formato dos webhooks).
type simCharge struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	Org       int             `json:"-"`
	Raw       json.RawMessage `json:"-"`
}

// simCharges lista as cobranças de todas as organizações no simulador, pela
// API do comerciante (chave de cada organização, paginação por offset). A
// lista do simulador é das mais novas para as mais antigas: cobranças criadas
// durante a leitura deslocam as páginas, e as repetidas são descartadas.
func (sc *scenarioCtx) simCharges(ctx context.Context) ([]simCharge, error) {
	var list []simCharge
	seen := map[string]bool{}
	for oi, o := range sc.st.Orgs {
		for offset := 0; ; offset += 100 {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/charges?limit=100&offset=%d", sc.e.Paysim, offset), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+o.PaysimKey)
			resp, err := sc.hc.Do(req)
			if err != nil {
				return nil, err
			}
			var out struct {
				Data    []json.RawMessage `json:"data"`
				HasMore bool              `json:"has_more"`
			}
			err = json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("cobranças do simulador (%s): %w", o.Slug, err)
			}
			for _, raw := range out.Data {
				c := simCharge{Org: oi, Raw: raw}
				if err := json.Unmarshal(raw, &c); err != nil {
					return nil, err
				}
				if !seen[c.ID] {
					seen[c.ID] = true
					list = append(list, c)
				}
			}
			if !out.HasMore {
				break
			}
		}
	}
	return list, nil
}

// simAction aplica uma ação do simulador e devolve a cobrança resultante.
func (sc *scenarioCtx) simAction(ctx context.Context, rec *recorder, op, id, action string) (json.RawMessage, error) {
	b, _ := json.Marshal(map[string]string{"action": action})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sc.e.Paysim+"/sim/charges/"+id+"/actions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := sc.hc.Do(req)
	if err != nil {
		rec.observe(op, time.Since(start), 0)
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	rec.observe(op, time.Since(start), resp.StatusCode)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{Status: resp.StatusCode, Body: truncate(string(data), 200)}
	}
	return data, nil
}

type billingResult struct {
	NewSubscriptions int            `json:"new_subscriptions"`
	CreateOps        []opSummary    `json:"create_ops"`
	DueAtStart       int64          `json:"due_at_start"`
	FirstInvoice     time.Duration  `json:"first_invoice_ns"`
	InvoicesTime     time.Duration  `json:"invoices_time_ns"`
	ChargesTime      time.Duration  `json:"charges_time_ns"`
	Invoices         int64          `json:"invoices"`
	Charges          int64          `json:"charges"`
	PeakInvoiceRate  float64        `json:"peak_invoices_per_s"`
	OutboxPeak       int64          `json:"outbox_peak"`
	BillingWindow    windowResult   `json:"billing_window"`
	BillingStmts     []statement    `json:"billing_statements"`
	Paid             int            `json:"paid"`
	PayOps           []opSummary    `json:"pay_ops"`
	PaymentsTime     time.Duration  `json:"payments_time_ns"`
	ProviderPayments int64          `json:"provider_payments"`
	PaymentWindow    windowResult   `json:"payment_window"`
	PaymentStmts     []statement    `json:"payment_statements"`
	Final            progress       `json:"final"`
	Series           []billingPoint `json:"series"`
}

type billingPoint struct {
	At    time.Duration `json:"at_ns"`
	S     since         `json:"since"`
	Queue int64         `json:"outbox_pending"`
}

// billing reproduz o fechamento: com o worker parado, cria assinaturas que
// vencem todas agora (como as renovações do primeiro dia do mês), religa o
// worker e mede o motor de recorrência até a última fatura e a última
// cobrança no simulador. Depois, parte das cobranças é paga no simulador, que
// devolve os webhooks, e mede-se até o último pagamento confirmado.
func (sc *scenarioCtx) billing(ctx context.Context, args []string) error {
	fs := newFlags("faturamento")
	share := fs.Float64("fracao", 0.5, "fração dos clientes que recebe uma assinatura nova")
	payShare := fs.Float64("pagas", 0.7, "fração das cobranças pagas no simulador")
	conc := fs.Int("concorrencia", 16, "requisições simultâneas na criação e nos pagamentos")
	limit := fs.Duration("prazo", 40*time.Minute, "espera máxima de cada fase")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Antes de parar o worker: uma fração fora de 0 a 1 faria o fatiamento
	// das cobranças entrar em pânico (acima de 1) ou não pagar nada (abaixo
	// de 0).
	if err := errors.Join(checkShare("fracao", *share), checkShare("pagas", *payShare)); err != nil {
		return err
	}
	res := billingResult{}
	type task struct {
		org  int
		cust string
	}
	r := rand.New(rand.NewPCG(11, 11)) //nolint:gosec // sorteio dos clientes do cenário, reproduzível
	var tasks []task
	for oi, o := range sc.st.Orgs {
		for _, c := range o.CustomerIDs {
			if r.Float64() < *share {
				tasks = append(tasks, task{oi, c})
			}
		}
	}
	res.NewSubscriptions = len(tasks)
	sc.log.Info("faturamento: parando o worker", slog.Int("assinaturas_novas", len(tasks)))
	if err := stopWorker(ctx, sc.e); err != nil {
		return err
	}
	// O worker volta mesmo se o cenário falhar no meio.
	workerUp := false
	defer func() {
		if !workerUp {
			if err := startWorker(context.WithoutCancel(ctx), &sc.e); err != nil {
				sc.log.Error("worker não foi religado", slog.String("error", err.Error()))
			}
		}
	}()
	run := randomHex(4)
	today := time.Now().In(tenantLoc).Format(time.DateOnly)
	createRec := newRecorder()
	var done atomic.Int64
	createRec.begin()
	err := parallel(ctx, sc.log, "faturamento: assinaturas com o worker parado", *conc, len(tasks), func(ctx context.Context, i int) error {
		t := tasks[i]
		o := sc.st.Orgs[t.org]
		cl := sc.client(o, createRec)
		plan := o.FlatPlans[i%len(o.FlatPlans)]
		h := http.Header{"Idempotency-Key": {fmt.Sprintf("carga-fechamento-%s-%06d", run, i)}}
		err := retry(ctx, 4, func() (int, error) {
			return cl.call(ctx, "criar assinatura (worker parado)", http.MethodPost, "/api/v1/subscriptions",
				map[string]any{"customer_id": t.cust, "plan_id": plan, "quantity": 1, "start_date": today}, h, nil)
		})
		if d := done.Add(1); d%1000 == 0 {
			sc.log.Info("faturamento: assinaturas criadas", slog.Int64("total", d), slog.Int("planejadas", len(tasks)))
		}
		return err
	})
	createRec.finish()
	if err != nil {
		return fmt.Errorf("criação das assinaturas: %w", err)
	}
	res.CreateOps, _ = createRec.summary()
	p, err := sc.obs.progress(ctx)
	if err != nil {
		return err
	}
	res.DueAtStart = p.DueSubscriptions
	// Fase 1: emissão das faturas e criação das cobranças.
	sc.resetStatements(ctx)
	w := openWindow(ctx, sc.log, sc.e)
	t0 := time.Now()
	if err := startWorker(ctx, &sc.e); err != nil {
		return err
	}
	workerUp = true
	w.e.WorkerMetrics = sc.e.WorkerMetrics
	w.snapshotWorker(ctx)
	sc.log.Info("faturamento: worker religado", slog.Int64("assinaturas_vencidas", p.DueSubscriptions))
	var invoicesAt time.Duration
	deadline := t0.Add(*limit)
	type snapshot struct {
		S  since
		Pr progress
	}
	stall := newStallGuard[snapshot]()
	for {
		s, err := sc.obs.since(ctx, t0)
		if err != nil {
			return err
		}
		pr, err := sc.obs.progress(ctx)
		if err != nil {
			return err
		}
		el := time.Since(t0)
		res.Series = append(res.Series, billingPoint{At: el, S: s, Queue: pr.OutboxPending})
		res.OutboxPeak = max(res.OutboxPeak, pr.OutboxPending)
		if res.FirstInvoice == 0 && s.Invoices > 0 {
			res.FirstInvoice = el
		}
		if invoicesAt == 0 && pr.DueSubscriptions == 0 {
			invoicesAt = el
		}
		sc.log.Info("faturamento: emissão", slog.Duration("decorrido", el.Round(time.Second)), slog.Int64("faturas", s.Invoices),
			slog.Int64("cobrancas", s.Charges), slog.Int64("vencidas", pr.DueSubscriptions), slog.Int64("sem_cobranca", s.InvoicesNoCharge),
			slog.Int64("outbox_pendente", pr.OutboxPending))
		if invoicesAt > 0 && s.InvoicesNoCharge == 0 && pr.OutboxPending == 0 {
			res.Invoices, res.Charges = s.Invoices, s.Charges
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("faturamento: prazo de %s esgotado (faturas %d, cobranças %d)", *limit, s.Invoices, s.Charges)
		}
		if err := stall.check(snapshot{s, pr}); err != nil {
			return fmt.Errorf("faturamento: emissão %w (faturas %d, cobranças %d)", err, s.Invoices, s.Charges)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	res.InvoicesTime, res.ChargesTime = invoicesAt, time.Since(t0)
	res.PeakInvoiceRate = peakRate(res.Series, 10*time.Second)
	res.BillingWindow = w.close(ctx)
	res.BillingStmts = sc.topStatements(ctx)
	sc.log.Info("faturamento: emissão concluída", slog.Int64("faturas", res.Invoices), slog.Duration("faturas_em", res.InvoicesTime),
		slog.Duration("cobrancas_em", res.ChargesTime))
	// Fase 2: pagamentos no simulador e webhooks de volta.
	charges, err := sc.simCharges(ctx)
	if err != nil {
		return err
	}
	var pending []simCharge
	for _, c := range charges {
		if c.Status == "pending" && !c.CreatedAt.Before(t0.Add(-time.Minute)) {
			pending = append(pending, c)
		}
	}
	r.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
	toPay := pending[:int(float64(len(pending))**payShare)]
	res.Paid = len(toPay)
	sc.resetStatements(ctx)
	pw := openWindow(ctx, sc.log, sc.e)
	t2 := time.Now()
	payRec := newRecorder()
	payRec.begin()
	err = parallel(ctx, sc.log, "faturamento: pagamentos no simulador", *conc, len(toPay), func(ctx context.Context, i int) error {
		_, err := sc.simAction(ctx, payRec, "pagar no simulador", toPay[i].ID, "pay")
		return err
	})
	payRec.finish()
	if err != nil {
		return fmt.Errorf("pagamentos no simulador: %w", err)
	}
	res.PayOps, _ = payRec.summary()
	series, err := sc.obs.waitUntil(ctx, "faturamento: pagamentos", 2*time.Second, *limit, func(p progress) bool { return p.WebhooksPending == 0 && p.OutboxPending == 0 },
		func(el time.Duration, p progress) {
			s, _ := sc.obs.since(ctx, t2)
			sc.log.Info("faturamento: pagamentos", slog.Duration("decorrido", el.Round(time.Second)), slog.Int64("confirmados", s.ProviderPayments),
				slog.Int("pagos_no_simulador", len(toPay)), slog.Int64("webhooks_pendentes", p.WebhooksPending))
		})
	_ = series
	if err != nil {
		return fmt.Errorf("pagamentos: %w", err)
	}
	// Os webhooks do simulador chegam depois do pagamento: espera até todos
	// os pagamentos confirmados, o prazo ou stallLimit sem nenhum novo.
	confirmed := newStallGuard[int64]()
	lastLog := time.Now()
	for {
		s, err := sc.obs.since(ctx, t2)
		if err != nil {
			return err
		}
		res.ProviderPayments = s.ProviderPayments
		if s.ProviderPayments >= int64(len(toPay)) || time.Since(t2) > *limit {
			break
		}
		if err := confirmed.check(s.ProviderPayments); err != nil {
			sc.log.Warn("faturamento: pagamentos sem confirmação", slog.String("motivo", err.Error()),
				slog.Int64("confirmados", s.ProviderPayments), slog.Int("pagos_no_simulador", len(toPay)))
			break
		}
		if time.Since(lastLog) >= 30*time.Second {
			lastLog = time.Now()
			sc.log.Info("faturamento: esperando confirmações", slog.Int64("confirmados", s.ProviderPayments),
				slog.Int("pagos_no_simulador", len(toPay)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	res.PaymentsTime = time.Since(t2)
	res.PaymentWindow = pw.close(ctx)
	res.PaymentStmts = sc.topStatements(ctx)
	if res.Final, err = sc.obs.progress(ctx); err != nil {
		return err
	}
	sc.log.Info("faturamento: pagamentos concluídos", slog.Int64("confirmados", res.ProviderPayments), slog.Duration("em", res.PaymentsTime))
	return sc.writeBilling(res)
}

// peakRate é a maior taxa de faturas emitidas numa janela deslizante.
func peakRate(series []billingPoint, win time.Duration) float64 {
	best := 0.0
	start := 0
	for _, cur := range series {
		for _, first := range series[start:] {
			if cur.At-first.At <= win {
				break
			}
			start++
		}
		first := series[start]
		if d := cur.At - first.At; d > 0 {
			best = max(best, float64(cur.S.Invoices-first.S.Invoices)/d.Seconds())
		}
	}
	return best
}

func (sc *scenarioCtx) writeBilling(res billingResult) error {
	f, save := sc.report("30-faturamento")
	rate := func(n int64, d time.Duration) float64 {
		if d <= 0 {
			return 0
		}
		return float64(n) / d.Seconds()
	}
	fmt.Fprintf(f, "## Cenário (c): faturamento em lote (fechamento)\n\n")
	fmt.Fprintf(f, "Com o worker parado, %d assinaturas novas (início hoje) foram criadas pela API para clientes existentes de %d organizações;"+
		" com o worker religado, o motor de recorrência emitiu as faturas, a outbox as publicou e o consumidor de pagamentos criou as"+
		" cobranças no simulador. Depois, %d cobranças foram pagas no simulador, que devolveu os webhooks.\n\n", res.NewSubscriptions,
		len(sc.st.Orgs), res.Paid)
	fmt.Fprintf(f, "| Medida | Valor |\n|---|---:|\n")
	fmt.Fprintf(f, "| Assinaturas vencidas ao religar o worker | %d |\n", res.DueAtStart)
	fmt.Fprintf(f, "| Faturas emitidas | %d |\n", res.Invoices)
	fmt.Fprintf(f, "| Tempo até a primeira fatura (espera do agendador, intervalo de 1 min) | %s |\n", res.FirstInvoice.Round(time.Second))
	fmt.Fprintf(f, "| Tempo até a última fatura | %s |\n", res.InvoicesTime.Round(time.Second))
	fmt.Fprintf(f, "| Faturas por segundo (da primeira à última) | %.1f |\n", rate(res.Invoices, res.InvoicesTime-res.FirstInvoice))
	fmt.Fprintf(f, "| Faturas por segundo (pico em 10 s) | %.1f |\n", res.PeakInvoiceRate)
	fmt.Fprintf(f, "| Cobranças criadas no simulador | %d |\n", res.Charges)
	fmt.Fprintf(f, "| Tempo até a última cobrança (fim da fase) | %s |\n", res.ChargesTime.Round(time.Second))
	fmt.Fprintf(f, "| Pico da outbox pendente | %d |\n", res.OutboxPeak)
	fmt.Fprintf(f, "| Cobranças pagas no simulador | %d |\n", res.Paid)
	fmt.Fprintf(f, "| Pagamentos confirmados pelos webhooks | %d |\n", res.ProviderPayments)
	fmt.Fprintf(f, "| Tempo do primeiro pagamento até o último confirmado | %s |\n", res.PaymentsTime.Round(time.Second))
	fmt.Fprintf(f, "| Pagamentos confirmados por segundo | %.1f |\n", rate(res.ProviderPayments, res.PaymentsTime))
	fmt.Fprintf(f, "| Dead letters no fim | %d |\n", res.Final.DeadLetters)
	fmt.Fprintf(f, "\nEvolução da emissão (a cada ~10 s):\n\n| Decorrido | Faturas | Cobranças | Outbox pendente |\n|---:|---:|---:|---:|\n")
	last := time.Duration(-1)
	for _, p := range res.Series {
		if last >= 0 && p.At-last < 10*time.Second && p != res.Series[len(res.Series)-1] {
			continue
		}
		last = p.At
		fmt.Fprintf(f, "| %s | %d | %d | %d |\n", p.At.Round(time.Second), p.S.Invoices, p.S.Charges, p.Queue)
	}
	fmt.Fprintf(f, "\nCriação das assinaturas com o worker parado:\n\n")
	_, ct := summarizeOps(res.CreateOps)
	writeTable(f, res.CreateOps, ct)
	fmt.Fprintf(f, "\n### Emissão e cobranças\n")
	writeWindow(f, res.BillingWindow)
	writeStatements(f, res.BillingStmts)
	fmt.Fprintf(f, "\n### Pagamentos e webhooks do simulador\n\n")
	_, pt := summarizeOps(res.PayOps)
	writeTable(f, res.PayOps, pt)
	writeWindow(f, res.PaymentWindow)
	writeStatements(f, res.PaymentStmts)
	return save(res)
}

// summarizeOps devolve a linha de total de uma lista já resumida (soma das
// contagens; percentis da maior operação).
func summarizeOps(ops []opSummary) ([]opSummary, opSummary) {
	t := opSummary{Op: "total", Statuses: map[int]int{}}
	for _, o := range ops {
		t.Count += o.Count
		t.Errors += o.Errors
		t.RPS += o.RPS
		t.P50, t.P95, t.P99, t.Max = max(t.P50, o.P50), max(t.P95, o.P95), max(t.P99, o.P99), max(t.Max, o.Max)
		for k, v := range o.Statuses {
			t.Statuses[k] += v
		}
	}
	if t.Count > 0 {
		t.ErrorRate = float64(t.Errors) / float64(t.Count)
	}
	return ops, t
}

// ---------- (d) ingestão de consumo medido ----------

func (sc *scenarioCtx) usage(ctx context.Context, args []string) error {
	fs := newFlags("consumo")
	levels := fs.String("niveis", "4,16", "lotes simultâneos de cada nível")
	dur := fs.Duration("duracao", 40*time.Second, "duração de cada nível")
	batch := fs.Int("lote", 100, "eventos por requisição (máximo 500)")
	dupShare := fs.Float64("duplicados", 0.05, "fração dos eventos reenviados (mesmo external_id)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkShare("duplicados", *dupShare); err != nil {
		return err
	}
	conc, err := intList(*levels)
	if err != nil {
		return err
	}
	if *batch < 1 || *batch > 500 {
		return fmt.Errorf("-lote deve estar entre 1 e 500")
	}
	type target struct {
		org int
		sub string
	}
	var targets []target
	for oi, o := range sc.st.Orgs {
		for _, s := range o.UsageSubs {
			targets = append(targets, target{oi, s})
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("nenhuma assinatura com consumo medido no estado: rode popular")
	}
	run := randomHex(4)
	var seq atomic.Int64
	var results []levelResult
	recent := newRecentEvents(len(sc.st.Orgs), 2000)
	for _, c := range conc {
		sc.log.Info("consumo", slog.Int("concorrencia", c), slog.Duration("duracao", *dur))
		rec := newRecorder()
		outcomes := map[string]int64{}
		var omu sync.Mutex
		before, _ := sc.obs.usageCount(ctx)
		sc.resetStatements(ctx)
		w := openWindow(ctx, sc.log, sc.e)
		rec.begin()
		runFor(ctx, c, *dur, func(ctx context.Context, r *rand.Rand) {
			// Um lote é de uma organização (a credencial é por organização),
			// com eventos de várias assinaturas dela.
			t0 := targets[r.IntN(len(targets))]
			o := sc.st.Orgs[t0.org]
			subs := o.UsageSubs
			events := make([]map[string]any, 0, *batch)
			now := time.Now().UTC()
			for range *batch {
				// Reenvio idêntico de um evento recente (como a nova tentativa
				// de um cliente depois de um tempo esgotado), possivelmente
				// ainda em andamento noutro lote.
				if r.Float64() < *dupShare {
					if ev, ok := recent.pick(t0.org, r); ok {
						events = append(events, ev)
						continue
					}
				}
				ev := map[string]any{"external_id": fmt.Sprintf("carga-%s-%08d", run, seq.Add(1)), "subscription_id": subs[r.IntN(len(subs))],
					"metric_code": o.MetricCode, "quantity": fmt.Sprint(1 + r.IntN(50)),
					"occurred_at": now.Add(-time.Duration(r.IntN(60)) * time.Second).Format(time.RFC3339Nano)}
				recent.add(t0.org, ev)
				events = append(events, ev)
			}
			var out struct {
				Results []struct {
					Outcome string `json:"outcome"`
				} `json:"results"`
			}
			if _, err := sc.client(o, rec).call(ctx, fmt.Sprintf("lote de %d eventos", *batch), http.MethodPost, "/api/v1/usage-events",
				map[string]any{"events": events}, nil, &out); err != nil {
				return
			}
			omu.Lock()
			for _, x := range out.Results {
				outcomes[x.Outcome]++
			}
			omu.Unlock()
		})
		rec.finish()
		ops, total := rec.summary()
		after, _ := sc.obs.usageCount(ctx)
		ev := float64(0)
		if total.Window > 0 {
			ev = float64(outcomes["accepted"]+outcomes["duplicate"]+outcomes["late"]+outcomes["rejected"]) / total.Window.Seconds()
		}
		results = append(results, levelResult{Concurrency: c, Ops: ops, Total: total, Window: w.close(ctx), Statements: sc.topStatements(ctx),
			Extra: map[string]any{"outcomes": outcomes, "events_per_s": ev, "stored": after - before}})
		sc.log.Info("consumo concluído", slog.Int("concorrencia", c), slog.Float64("eventos_s", ev), slog.Duration("p95", total.P95),
			slog.Int("erros", total.Errors))
	}
	f, save := sc.report("50-consumo")
	fmt.Fprintf(f, "## Cenário (d): ingestão de consumo medido\n\n")
	fmt.Fprintf(f, "Lotes de %d eventos por requisição para %d assinaturas com consumo medido; %.0f%% dos eventos são o reenvio"+
		" idêntico de um evento recente, possivelmente ainda em processamento noutro lote (nova tentativa do cliente; devem voltar como"+
		" duplicate). %s por nível.\n", *batch, len(targets), 100**dupShare, *dur)
	for _, l := range results {
		fmt.Fprintf(f, "\n### %d lotes simultâneos\n\n", l.Concurrency)
		writeTable(f, l.Ops, l.Total)
		fmt.Fprintf(f, "\nEventos por segundo: %.0f; resultados: %v; gravados no banco: %d.\n", l.Extra["events_per_s"], l.Extra["outcomes"],
			l.Extra["stored"])
		writeWindow(f, l.Window)
		writeStatements(f, l.Statements)
	}
	return save(results)
}

// recentEvents guarda, por organização, os últimos eventos enviados, para os
// reenvios idênticos.
type recentEvents struct {
	mu    sync.Mutex
	size  int
	byOrg [][]map[string]any
	next  []int
}

func newRecentEvents(orgs, size int) *recentEvents {
	return &recentEvents{size: size, byOrg: make([][]map[string]any, orgs), next: make([]int, orgs)}
}

func (re *recentEvents) add(org int, ev map[string]any) {
	re.mu.Lock()
	defer re.mu.Unlock()
	if len(re.byOrg[org]) < re.size {
		re.byOrg[org] = append(re.byOrg[org], ev)
		return
	}
	re.byOrg[org][re.next[org]] = ev
	re.next[org] = (re.next[org] + 1) % re.size
}

func (re *recentEvents) pick(org int, r *rand.Rand) (map[string]any, bool) {
	re.mu.Lock()
	defer re.mu.Unlock()
	l := re.byOrg[org]
	if len(l) == 0 {
		return nil, false
	}
	return l[r.IntN(len(l))], true
}

// ---------- (e) pico de webhooks do provedor ----------

type webhooksResult struct {
	Charges        int           `json:"charges"`
	Sent           int           `json:"sent"`
	Ops            []opSummary   `json:"ops"`
	Total          opSummary     `json:"total"`
	ReceiveTime    time.Duration `json:"receive_time_ns"`
	ProcessTime    time.Duration `json:"process_time_ns"`
	Payments       int64         `json:"payments"`
	DoublePaid     int64         `json:"double_paid"`
	OutOfOrder     int           `json:"out_of_order"`
	Window         windowResult  `json:"window"`
	Statements     []statement   `json:"statements"`
	Final          progress      `json:"final"`
	AcceptedMetric float64       `json:"accepted"`
	DupMetric      float64       `json:"duplicate"`
}

// webhooks paga em silêncio, no simulador, as cobranças ainda pendentes e
// envia à API, de uma vez, os webhooks assinados correspondentes: o
// charge.paid com repetições e o charge.created antigo, em ordem embaralhada
// (parte chega depois do pagamento). Mede a recepção e o processamento e
// confere que nenhum pagamento foi duplicado.
func (sc *scenarioCtx) webhooks(ctx context.Context, args []string) error {
	fs := newFlags("webhooks")
	conc := fs.Int("concorrencia", 32, "envios simultâneos")
	reps := fs.Int("repeticoes", 2, "cópias extras de cada charge.paid")
	maxCharges := fs.Int("max", 3000, "máximo de cobranças usadas")
	limit := fs.Duration("prazo", 20*time.Minute, "espera máxima do processamento")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, o := range sc.st.Orgs {
		if o.WebhookURL == "" || o.WebhookSecret == "" {
			return fmt.Errorf("%s sem webhook configurado: rode provedor", o.Slug)
		}
	}
	charges, err := sc.simCharges(ctx)
	if err != nil {
		return err
	}
	var pending []simCharge
	for _, c := range charges {
		if c.Status == "pending" {
			pending = append(pending, c)
		}
	}
	if len(pending) > *maxCharges {
		pending = pending[:*maxCharges]
	}
	if len(pending) == 0 {
		return fmt.Errorf("nenhuma cobrança pendente no simulador: rode faturamento antes")
	}
	sc.log.Info("webhooks: pagando em silêncio no simulador", slog.Int("cobrancas", len(pending)))
	silentRec := newRecorder()
	paidViews := make([]json.RawMessage, len(pending))
	if err := parallel(ctx, sc.log, "webhooks: pagamentos em silêncio", 16, len(pending), func(ctx context.Context, i int) error {
		v, err := sc.simAction(ctx, silentRec, "pagar em silêncio", pending[i].ID, "pay_silently")
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusConflict {
			// A cobrança mudou de situação depois da listagem: fica de fora.
			return nil
		}
		paidViews[i] = v
		return err
	}); err != nil {
		return err
	}
	kept := pending[:0]
	keptViews := paidViews[:0]
	for i, v := range paidViews {
		if v != nil {
			kept, keptViews = append(kept, pending[i]), append(keptViews, v)
		}
	}
	pending, paidViews = kept, keptViews
	type delivery struct {
		org  org
		op   string
		body []byte
		seq  int // posição do charge.paid desta cobrança (para fora de ordem)
	}
	var deliveries []delivery
	now := time.Now().UTC()
	// A ação do simulador devolve a cobrança sem a lista de reembolsos; o
	// webhook real traz a lista (vazia aqui).
	withRefunds := func(raw json.RawMessage) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["refunds"] == nil {
			m["refunds"] = []any{}
		}
		return m
	}
	for i, c := range pending {
		o := sc.st.Orgs[c.Org]
		paid, _ := json.Marshal(map[string]any{"id": "evt_carga_" + c.ID + "_paid", "type": "charge.paid", "created_at": now,
			"data": map[string]any{"charge": withRefunds(paidViews[i])}})
		created, _ := json.Marshal(map[string]any{"id": "evt_carga_" + c.ID + "_created", "type": "charge.created", "created_at": c.CreatedAt,
			"data": map[string]any{"charge": withRefunds(c.Raw)}})
		deliveries = append(deliveries, delivery{org: o, op: "charge.paid (primeira)", body: paid, seq: i})
		for range *reps {
			deliveries = append(deliveries, delivery{org: o, op: "charge.paid (repetição)", body: paid, seq: i})
		}
		deliveries = append(deliveries, delivery{org: o, op: "charge.created (atrasado)", body: created, seq: i})
	}
	r := rand.New(rand.NewPCG(5, 5)) //nolint:gosec // ordem das entregas do cenário, reproduzível
	r.Shuffle(len(deliveries), func(i, j int) { deliveries[i], deliveries[j] = deliveries[j], deliveries[i] })
	// Conta quantas cobranças tiveram o charge.created entregue depois de
	// algum charge.paid e renomeia a primeira cópia de cada charge.paid
	// (para que "primeira" seja de fato a primeira enviada).
	seenPaid := map[int]bool{}
	outOfOrder := 0
	for i := range deliveries {
		d := &deliveries[i]
		switch {
		case strings.HasPrefix(d.op, "charge.paid"):
			if seenPaid[d.seq] {
				d.op = "charge.paid (repetição)"
			} else {
				d.op = "charge.paid (primeira)"
			}
			seenPaid[d.seq] = true
		case seenPaid[d.seq]:
			outOfOrder++
		}
	}
	res := webhooksResult{Charges: len(pending), Sent: len(deliveries), OutOfOrder: outOfOrder}
	rec := newRecorder()
	sc.resetStatements(ctx)
	w := openWindow(ctx, sc.log, sc.e)
	t0 := time.Now()
	rec.begin()
	sc.log.Info("webhooks: enviando", slog.Int("entregas", len(deliveries)), slog.Int("concorrencia", *conc))
	if err := parallel(ctx, sc.log, "webhooks: entregas", *conc, len(deliveries), func(ctx context.Context, i int) error {
		d := deliveries[i]
		ts := time.Now().Unix()
		h := http.Header{paysim.SignatureHeader: {fmt.Sprintf("t=%d,v1=%s", ts, paysim.Sign(d.org.WebhookSecret, ts, d.body))}}
		_, err := sc.rawPost(ctx, rec, d.op, sc.e.API+d.org.WebhookURL, d.body, h)
		return err
	}); err != nil {
		sc.log.Warn("webhooks: envios com erro", slog.String("error", err.Error()))
	}
	rec.finish()
	res.ReceiveTime = time.Since(t0)
	res.Ops, res.Total = rec.summary()
	if _, err := sc.obs.waitUntil(ctx, "webhooks: processamento", 2*time.Second, *limit, func(p progress) bool { return p.WebhooksPending == 0 && p.OutboxPending == 0 },
		func(el time.Duration, p progress) {
			sc.log.Info("webhooks: processamento", slog.Duration("decorrido", el.Round(time.Second)), slog.Int64("pendentes", p.WebhooksPending),
				slog.Int64("outbox_pendente", p.OutboxPending))
		}); err != nil {
		return fmt.Errorf("processamento dos webhooks: %w", err)
	}
	res.ProcessTime = time.Since(t0)
	s, err := sc.obs.since(ctx, t0)
	if err != nil {
		return err
	}
	res.Payments = s.ProviderPayments
	if res.DoublePaid, err = sc.obs.doublePaid(ctx); err != nil {
		return err
	}
	res.Window = w.close(ctx)
	res.Statements = sc.topStatements(ctx)
	res.AcceptedMetric, res.DupMetric = res.Window.Deltas["webhooks accepted"], res.Window.Deltas["webhooks duplicate"]
	if res.Final, err = sc.obs.progress(ctx); err != nil {
		return err
	}
	f, save := sc.report("40-webhooks")
	fmt.Fprintf(f, "## Cenário (e): pico de webhooks do provedor\n\n")
	fmt.Fprintf(f, "%d cobranças pendentes foram pagas em silêncio no simulador; para cada uma, a carga enviou à API o charge.paid assinado"+
		" %d vezes (1 + %d repetições) e o charge.created antigo, num total de %d entregas embaralhadas, com %d envios simultâneos. Em %d"+
		" cobranças o charge.created chegou depois de um charge.paid (fora de ordem).\n\n", res.Charges, 1+*reps, *reps, res.Sent, *conc,
		res.OutOfOrder)
	fmt.Fprintf(f, "| Medida | Valor |\n|---|---:|\n")
	fmt.Fprintf(f, "| Tempo de envio de todas as entregas | %s |\n", res.ReceiveTime.Round(time.Millisecond))
	fmt.Fprintf(f, "| Webhooks recebidos por segundo | %.1f |\n", res.Total.RPS)
	fmt.Fprintf(f, "| Tempo até o último processado (desde o primeiro envio) | %s |\n", res.ProcessTime.Round(time.Second))
	fmt.Fprintf(f, "| Aceitos / duplicados (métrica da API) | %.0f / %.0f |\n", res.AcceptedMetric, res.DupMetric)
	fmt.Fprintf(f, "| Pagamentos confirmados | %d de %d cobranças |\n", res.Payments, res.Charges)
	fmt.Fprintf(f, "| Cobranças com mais de um pagamento | %d |\n", res.DoublePaid)
	fmt.Fprintf(f, "| Dead letters no fim | %d |\n\n", res.Final.DeadLetters)
	writeTable(f, res.Ops, res.Total)
	writeWindow(f, res.Window)
	writeStatements(f, res.Statements)
	return save(res)
}

// rawPost envia um corpo pronto (webhook do provedor, sem credencial de API).
func (sc *scenarioCtx) rawPost(ctx context.Context, rec *recorder, op, target string, body []byte, h http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := sc.hc.Do(req)
	if err != nil {
		rec.observe(op, time.Since(start), 0)
		return 0, err
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	rec.observe(op, time.Since(start), resp.StatusCode)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, &httpError{Status: resp.StatusCode, Body: truncate(string(data), 200)}
	}
	return resp.StatusCode, nil
}
