package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
)

// tenantLoc é o fuso das organizações sintéticas (datas de início das
// assinaturas).
var tenantLoc = mustLoc("America/Sao_Paulo")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// cnpj gera um CNPJ válido e único por organização e cliente.
func cnpj(orgIndex, n int) string {
	base := fmt.Sprintf("%03d%05d0001", orgIndex%1000, n%100000)
	dv, err := brdoc.CNPJCheckDigits(base)
	if err != nil {
		panic(err)
	}
	return base + dv
}

func customerBody(slug string, orgIndex, n int) map[string]any {
	return map[string]any{"kind": "company", "name": fmt.Sprintf("Cliente %05d da %s", n, slug), "document_number": cnpj(orgIndex, n),
		"email": fmt.Sprintf("financeiro@cliente-%05d.%s.carga.test", n, slug), "external_id": fmt.Sprintf("%s-%05d", slug, n)}
}

type idResp struct {
	ID string `json:"id"`
}

// pickPlan escolhe o plano da assinatura: 20% com consumo medido, o restante
// distribuído entre os planos de preço fixo.
func pickPlan(flatPlans []string, usagePlan string, r *rand.Rand) (plan string, usage bool) {
	if r.IntN(5) == 0 {
		return usagePlan, true
	}
	return flatPlans[r.IntN(len(flatPlans))], false
}

// runPopulate cria, pela API, os clientes e as assinaturas de cada
// organização. As assinaturas começam em datas de até cinco meses atrás: o
// motor de recorrência emite as faturas retroativas (histórico) e o ledger
// registra a emissão. Depois do faturamento, 80% das faturas em aberto
// recebem pagamento manual (histórico de recebimentos no ledger).
func runPopulate(ctx context.Context, log *slog.Logger, e env, args []string) error {
	fs := newFlags("popular")
	conc := fs.Int("concorrencia", 16, "requisições simultâneas")
	maxDays := fs.Int("dias", 150, "início mais antigo das assinaturas, em dias")
	payShare := fs.Float64("pagas", 0.8, "fração das faturas em aberto pagas manualmente")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := checkShare("pagas", *payShare); err != nil {
		return err
	}
	st, err := readState(e.StateFile)
	if err != nil {
		return err
	}
	obs, err := openObserver(ctx, log, e)
	if err != nil {
		return err
	}
	defer obs.close()
	rec := newRecorder()
	hc := newHTTPClient(*conc * 2)
	rec.begin()
	planned, err := createCustomers(ctx, log, e.API, hc, rec, st, *conc, *maxDays)
	rec.finish()
	if err != nil {
		return err
	}
	if err := writeState(e.StateFile, st); err != nil {
		return err
	}
	ops, total := rec.summary()
	log.Info("clientes e assinaturas criados", slog.Int("total", planned), slog.Float64("req_s", total.RPS))
	// Histórico de faturas: o motor emite os períodos retroativos.
	start := time.Now()
	series, err := obs.waitUntil(ctx, "faturamento do histórico", 5*time.Second, 60*time.Minute, func(p progress) bool {
		return p.DueSubscriptions == 0 && p.OutboxPending == 0
	}, func(el time.Duration, p progress) {
		log.Info("faturamento do histórico", slog.Duration("decorrido", el.Round(time.Second)), slog.Int64("faturas", p.Invoices),
			slog.Int64("assinaturas_vencidas", p.DueSubscriptions), slog.Int64("outbox_pendente", p.OutboxPending))
	})
	if err != nil {
		return err
	}
	billed := series[len(series)-1].P
	billingTime := time.Since(start)
	// Pagamentos manuais do histórico.
	payRec := newRecorder()
	type inv struct {
		org       int
		id        string
		amountDue int64
	}
	var open []inv
	for oi, o := range st.Orgs {
		c := &apiClient{base: e.API, token: o.Token, http: hc, rec: newRecorder()}
		cursor := ""
		for {
			var pg page[struct {
				ID        string `json:"id"`
				AmountDue int64  `json:"amount_due"`
			}]
			q := url.Values{"status": {"open"}, "limit": {"100"}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			if err := retry(ctx, 6, func() (int, error) {
				return c.call(ctx, "", http.MethodGet, "/api/v1/invoices?"+q.Encode(), nil, nil, &pg)
			}); err != nil {
				return err
			}
			for _, x := range pg.Data {
				if x.AmountDue > 0 {
					open = append(open, inv{oi, x.ID, x.AmountDue})
				}
			}
			if pg.NextCursor == nil || len(pg.Data) == 0 {
				break
			}
			if *pg.NextCursor == cursor {
				return fmt.Errorf("%s: a lista de faturas devolveu o mesmo cursor", o.Slug)
			}
			cursor = *pg.NextCursor
		}
	}
	log.Info("popular: faturas em aberto listadas", slog.Int("faturas", len(open)))
	r := rand.New(rand.NewPCG(7, 7)) //nolint:gosec // sorteio dos dados sintéticos, reproduzível
	var toPay []inv
	for _, x := range open {
		if r.Float64() < *payShare {
			toPay = append(toPay, x)
		}
	}
	payRec.begin()
	err = parallel(ctx, log, "popular: pagamentos manuais do histórico", *conc, len(toPay), func(ctx context.Context, i int) error {
		x := toPay[i]
		o := st.Orgs[x.org]
		c := &apiClient{base: e.API, token: o.Token, http: hc, rec: payRec}
		h := http.Header{"Idempotency-Key": {"carga-historico-" + x.id}}
		return retry(ctx, 6, func() (int, error) {
			return c.call(ctx, "pagamento manual", http.MethodPost, "/api/v1/invoices/"+x.id+"/payments",
				map[string]any{"amount": x.amountDue, "method": "pix", "note": "Pagamento do histórico (carga)"}, h, nil)
		})
	})
	payRec.finish()
	if err != nil {
		return err
	}
	if _, err := obs.waitUntil(ctx, "popular: outbox dos pagamentos", 5*time.Second, 30*time.Minute, func(p progress) bool { return p.OutboxPending == 0 }, nil); err != nil {
		return err
	}
	final, err := obs.progress(ctx)
	if err != nil {
		return err
	}
	payOps, payTotal := payRec.summary()
	path, err := outPath(e, "00-populacao.md")
	if err != nil {
		return err
	}
	f := &bytes.Buffer{}
	fmt.Fprintf(f, "## População dos dados sintéticos\n\n")
	fmt.Fprintf(f, "- Organizações: %d; clientes e assinaturas: %d (perfil 10%% grandes, 30%% médias, 60%% pequenas).\n", len(st.Orgs), planned)
	fmt.Fprintf(f, "- Faturas emitidas pelo motor (histórico de até %d dias): %d em %s (%.1f faturas/s, incluindo a criação ainda em curso).\n",
		*maxDays, billed.Invoices, billingTime.Round(time.Second), float64(billed.Invoices)/billingTime.Seconds())
	fmt.Fprintf(f, "- Pagamentos manuais do histórico: %d de %d faturas em aberto (%.1f req/s).\n", len(toPay), len(open), payTotal.RPS)
	fmt.Fprintf(f, "- Situação final: %d faturas, %d pagas, %d em aberto, %d pagamentos, %d dead letters.\n\n", final.Invoices,
		final.InvoicesPaid, final.InvoicesOpen, final.Payments, final.DeadLetters)
	fmt.Fprintf(f, "Criação de clientes e assinaturas:\n\n")
	writeTable(f, ops, total)
	fmt.Fprintf(f, "\nPagamentos manuais do histórico:\n\n")
	writeTable(f, payOps, payTotal)
	return os.WriteFile(path, f.Bytes(), 0o600)
}

// createCustomers cria, pela API, um cliente e uma assinatura para cada
// cliente planejado de cada organização, em ordem aleatória, e grava em st
// os identificadores criados. Os dados de cada organização usados pelas
// goroutines são copiados antes (st só muda no fim, sem concorrência), e o
// resultado de cada item fica na posição dele. Devolve o total planejado.
func createCustomers(ctx context.Context, log *slog.Logger, api string, hc *http.Client, rec *recorder, st *state, conc, maxDays int) (int, error) {
	type task struct{ org, n int }
	type orgInfo struct {
		slug, token, usagePlan string
		flatPlans              []string
	}
	orgs := make([]orgInfo, len(st.Orgs))
	var tasks []task
	for oi, o := range st.Orgs {
		orgs[oi] = orgInfo{slug: o.Slug, token: o.Token, usagePlan: o.UsagePlan, flatPlans: slices.Clone(o.FlatPlans)}
		for n := range o.Customers {
			tasks = append(tasks, task{oi, n + 1})
		}
	}
	rand.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] }) //nolint:gosec // ordem de criação, sem segurança envolvida
	type created struct{ customer, usageSub string }
	results := make([]created, len(tasks))
	today := time.Now().In(tenantLoc)
	var done atomic.Int64
	err := parallel(ctx, log, "popular: clientes e assinaturas", conc, len(tasks), func(ctx context.Context, i int) error {
		t := tasks[i]
		o := orgs[t.org]
		c := &apiClient{base: api, token: o.token, http: hc, rec: rec}
		r := rand.New(rand.NewPCG(uint64(t.org), uint64(t.n))) //nolint:gosec // sorteio dos dados sintéticos, reproduzível
		custID, err := createOrFindCustomer(ctx, c, customerBody(o.slug, t.org+1, t.n))
		if err != nil {
			return fmt.Errorf("%s: cliente %d: %w", o.slug, t.n, err)
		}
		plan, usage := pickPlan(o.flatPlans, o.usagePlan, r)
		start := today.AddDate(0, 0, -r.IntN(maxDays+1)).Format(time.DateOnly)
		var sub idResp
		h := http.Header{"Idempotency-Key": {fmt.Sprintf("carga-popular-%s-%05d", o.slug, t.n)}}
		if err := retry(ctx, 6, func() (int, error) {
			return c.call(ctx, "criar assinatura", http.MethodPost, "/api/v1/subscriptions",
				map[string]any{"customer_id": custID, "plan_id": plan, "quantity": 1, "start_date": start}, h, &sub)
		}); err != nil {
			return fmt.Errorf("%s: assinatura %d: %w", o.slug, t.n, err)
		}
		results[i] = created{customer: custID}
		if usage {
			results[i].usageSub = sub.ID
		}
		if d := done.Add(1); d%1000 == 0 && log != nil {
			log.Info("clientes e assinaturas criados", slog.Int64("total", d), slog.Int("planejados", len(tasks)))
		}
		return nil
	})
	if err != nil {
		return len(tasks), err
	}
	for i, t := range tasks {
		o := &st.Orgs[t.org]
		o.CustomerIDs = append(o.CustomerIDs, results[i].customer)
		if results[i].usageSub != "" {
			o.UsageSubs = append(o.UsageSubs, results[i].usageSub)
		}
	}
	return len(tasks), nil
}

// createOrFindCustomer cria o cliente. A criação de clientes não aceita
// Idempotency-Key; uma nova tentativa depois de um prazo esgotado (com o
// primeiro envio gravado) recebe 409 pelo external_id repetido, e então o
// cliente existente é buscado pelo external_id. Um 409 sem cliente com esse
// external_id (conflito de documento, por exemplo) não é repetido.
func createOrFindCustomer(ctx context.Context, c *apiClient, body map[string]any) (string, error) {
	ext, _ := body["external_id"].(string)
	var id string
	err := retry(ctx, 6, func() (int, error) {
		var cust idResp
		status, err := c.call(ctx, "criar cliente", http.MethodPost, "/api/v1/customers", body, nil, &cust)
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusConflict {
			found, ferr := findCustomerByExternalID(ctx, c, ext)
			if ferr != nil {
				return status, ferr
			}
			if found == "" {
				return status, permanent(err)
			}
			id = found
			return status, nil
		}
		if err == nil {
			id = cust.ID
		}
		return status, err
	})
	return id, err
}

// findCustomerByExternalID busca, pela API, o cliente com o external_id
// exato; vazio quando não há.
func findCustomerByExternalID(ctx context.Context, c *apiClient, ext string) (string, error) {
	if ext == "" {
		return "", nil
	}
	var pg page[struct {
		ID         string `json:"id"`
		ExternalID string `json:"external_id"`
	}]
	q := url.Values{"q": {ext}, "limit": {"20"}}
	if _, err := c.call(ctx, "", http.MethodGet, "/api/v1/customers?"+q.Encode(), nil, nil, &pg); err != nil {
		return "", err
	}
	for _, x := range pg.Data {
		if x.ExternalID == ext {
			return x.ID, nil
		}
	}
	return "", nil
}

// checkShare confere uma fração informada por opção (0 a 1).
func checkShare(name string, v float64) error {
	if math.IsNaN(v) || v < 0 || v > 1 {
		return fmt.Errorf("-%s deve ficar entre 0 e 1: %v", name, v)
	}
	return nil
}
