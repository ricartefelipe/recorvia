package paysim_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/paysim"
)

const (
	merchantKey   = "sk_test_comerciante_a"
	otherKey      = "sk_test_comerciante_b"
	webhookSecret = "whsec_0123456789abcdef"
)

// fakeClock é um relógio manual seguro para uso concorrente (os handlers do
// httptest.Server rodam em outras goroutines).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// webhook é uma entrega recebida pelo receptor de teste.
type webhook struct {
	Header http.Header
	Body   []byte
	ID     string
	Type   string
	At     time.Time
	Charge paysim.ChargeView
}

// receiver é o endpoint de webhooks do comerciante, em httptest.
type receiver struct {
	mu     sync.Mutex
	status int
	got    []webhook
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{status: http.StatusOK}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var ev struct {
			ID        string    `json:"id"`
			Type      string    `json:"type"`
			CreatedAt time.Time `json:"created_at"`
			Data      struct {
				Charge paysim.ChargeView `json:"charge"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &ev); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, webhook{Header: req.Header.Clone(), Body: body, ID: ev.ID, Type: ev.Type, At: ev.CreatedAt, Charge: ev.Data.Charge})
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) setStatus(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = status
}

// take devolve as entregas recebidas desde a última chamada.
func (r *receiver) take() []webhook {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.got
	r.got = nil
	return out
}

// env reúne simulador, relógio e receptor de webhooks do comerciante padrão.
type env struct {
	t     *testing.T
	clock *fakeClock
	st    *paysim.State
	sim   *paysim.Sim
	rcv   *receiver
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newEnv cria o ambiente com estado em memória e webhook configurado.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvAt(t, "", newClock())
}

func newEnvAt(t *testing.T, path string, clock *fakeClock) *env {
	t.Helper()
	st, err := paysim.Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	e := &env{t: t, clock: clock, st: st, rcv: newReceiver(t)}
	e.sim = paysim.New(st, "http://checkout.test/", clock.Now, discardLogger())
	if err := e.sim.ConfigureWebhook(merchantKey, e.rcv.srv.URL+"/webhooks", webhookSecret); err != nil {
		t.Fatalf("ConfigureWebhook: %v", err)
	}
	return e
}

func chargeInput(ref string) paysim.CreateChargeInput {
	return paysim.CreateChargeInput{CustomerID: "cus_000001", Amount: 10000, Currency: "BRL", DueDate: "2026-03-20",
		Description: "Mensalidade de março", ExternalReference: ref}
}

// create agenda o cenário (se houver) e cria a cobrança, exigindo sucesso.
func (e *env) create(scenario, ref string) paysim.Charge {
	e.t.Helper()
	if scenario != "" {
		if err := e.sim.EnqueueScenario(merchantKey, scenario); err != nil {
			e.t.Fatalf("EnqueueScenario(%s): %v", scenario, err)
		}
	}
	c, applied, err := e.sim.CreateCharge(merchantKey, chargeInput(ref))
	if err != nil {
		e.t.Fatalf("CreateCharge(%s): %v", scenario, err)
	}
	if applied != scenario {
		e.t.Fatalf("cenário aplicado = %q, esperado %q", applied, scenario)
	}
	return c
}

func (e *env) apply(id string, a paysim.Action) paysim.Charge {
	e.t.Helper()
	c, err := e.sim.Apply(id, a)
	if err != nil {
		e.t.Fatalf("Apply(%s): %v", a.Name, err)
	}
	return c
}

// dispatch entrega o que estiver vencido e devolve o que o receptor recebeu.
func (e *env) dispatch() []webhook {
	e.t.Helper()
	e.sim.DispatchDue(e.t.Context())
	return e.rcv.take()
}

func (e *env) view(id string) paysim.ChargeView {
	e.t.Helper()
	v, err := e.sim.View(merchantKey, id)
	if err != nil {
		e.t.Fatalf("View(%s): %v", id, err)
	}
	return v
}

// eventKeys resume as entregas como "tipo/id", na ordem de chegada.
func eventKeys(ws []webhook) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Type+"/"+w.ID)
	}
	return out
}

func eventTypes(ws []webhook) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Type)
	}
	return out
}

func ptr[T any](v T) *T { return &v }
