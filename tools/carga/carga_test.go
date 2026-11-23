package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMetricsURLFromPort(t *testing.T) {
	cases := []struct {
		out, want string
		wantErr   bool
	}{
		{"127.0.0.1:49153", "http://127.0.0.1:49153/metrics", false},
		{"127.0.0.1:49153\n[::1]:49153\n", "http://127.0.0.1:49153/metrics", false},
		{"", "", true},
		{"127.0.0.1:", "", true},
		{"sem porta", "", true},
	}
	for _, tc := range cases {
		got, err := metricsURLFromPort(tc.out)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%q: %q, %v; esperado %q, erro=%v", tc.out, got, err, tc.want, tc.wantErr)
		}
	}
}

// Uma leitura de /metrics que falha (worker religado em outra porta, por
// exemplo) aparece no resultado do trecho, em vez de virar deltas zerados
// sem aviso.
func TestWindowWarnsWhenMetricsFail(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "recorvia_db_pool_acquires_total 10")
	}))
	defer ok.Close()
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL + "/metrics"
	gone.Close()
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()

	cases := []struct {
		name         string
		api, worker  string
		wantWarnings int
	}{
		{"as duas respondem", ok.URL, ok.URL, 0},
		{"worker em porta antiga", ok.URL, goneURL, 2},
		{"worker responde 404", ok.URL, notFound.URL, 2},
		{"endereço vazio", "", ok.URL, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := openWindow(context.Background(), nil, env{APIMetrics: tc.api, WorkerMetrics: tc.worker})
			res := w.close(context.Background())
			if len(res.Warnings) != tc.wantWarnings {
				t.Fatalf("avisos %q, esperados %d", res.Warnings, tc.wantWarnings)
			}
		})
	}
}

// fakeAPI responde à criação de clientes e assinaturas e à busca de
// clientes. conflictFirst faz a primeira criação de cada external_id
// devolver 409 depois de gravar o cliente (como uma nova tentativa depois de
// um prazo esgotado).
type fakeAPI struct {
	mu            sync.Mutex
	byExt         map[string]string
	posts         atomic.Int64
	conflictFirst bool
	conflictNoHit bool
	seq           atomic.Int64
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/customers":
		f.posts.Add(1)
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		ext, _ := in["external_id"].(string)
		if f.conflictNoHit {
			http.Error(w, `{"error":"documento já cadastrado"}`, http.StatusConflict)
			return
		}
		f.mu.Lock()
		id, exists := f.byExt[ext]
		if !exists {
			id = fmt.Sprintf("cli-%d", f.seq.Add(1))
			f.byExt[ext] = id
		}
		f.mu.Unlock()
		if exists || f.conflictFirst {
			http.Error(w, `{"error":"external_id já usado"}`, http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/customers":
		q := r.URL.Query().Get("q")
		f.mu.Lock()
		var data []map[string]string
		for ext, id := range f.byExt {
			if strings.Contains(ext, q) {
				data = append(data, map[string]string{"id": id, "external_id": ext})
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "next_cursor": nil})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/subscriptions":
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("sub-%d", f.seq.Add(1))})
	default:
		http.NotFound(w, r)
	}
}

func TestCreateOrFindCustomerOnConflict(t *testing.T) {
	cases := []struct {
		name      string
		api       *fakeAPI
		wantID    bool
		wantPosts int64
	}{
		{"409 com o cliente já gravado: usa o existente", &fakeAPI{byExt: map[string]string{}, conflictFirst: true}, true, 1},
		{"409 sem cliente com o external_id: não repete", &fakeAPI{byExt: map[string]string{}, conflictNoHit: true}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.api)
			defer srv.Close()
			c := &apiClient{base: srv.URL, http: srv.Client(), rec: newRecorder()}
			id, err := createOrFindCustomer(context.Background(), c, customerBody("carga-001", 1, 7))
			if tc.wantID && (err != nil || id != tc.api.byExt["carga-001-00007"]) {
				t.Fatalf("id %q, erro %v", id, err)
			}
			if !tc.wantID && err == nil {
				t.Fatalf("esperado erro, id %q", id)
			}
			if got := tc.api.posts.Load(); got != tc.wantPosts {
				t.Fatalf("%d criações enviadas, esperadas %d", got, tc.wantPosts)
			}
		})
	}
}

// Com -race: as goroutines da criação não leem st enquanto ele é alterado.
func TestCreateCustomersFillsState(t *testing.T) {
	api := &fakeAPI{byExt: map[string]string{}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	st := &state{}
	for i := range 3 {
		st.Orgs = append(st.Orgs, org{Slug: fmt.Sprintf("carga-%03d", i+1), Token: "t", Customers: 40,
			FlatPlans: []string{"p1", "p2"}, UsagePlan: "consumo"})
	}
	planned, err := createCustomers(context.Background(), nil, srv.URL, srv.Client(), newRecorder(), st, 8, 30)
	if err != nil || planned != 120 {
		t.Fatalf("planejados %d, erro %v", planned, err)
	}
	for _, o := range st.Orgs {
		if len(o.CustomerIDs) != 40 || len(o.UsageSubs) == 0 || len(o.UsageSubs) >= 40 {
			t.Fatalf("%s: %d clientes, %d assinaturas de consumo", o.Slug, len(o.CustomerIDs), len(o.UsageSubs))
		}
	}
}

// Frações fora de 0 a 1 são recusadas antes de qualquer efeito (o
// faturamento pararia o worker e depois entraria em pânico com -pagas > 1).
func TestScenarioSharesAreValidated(t *testing.T) {
	sc := &scenarioCtx{}
	cases := []struct {
		name string
		run  func([]string) error
		args []string
	}{
		{"faturamento -pagas acima de 1", func(a []string) error { return sc.billing(context.Background(), a) }, []string{"-pagas", "1.5"}},
		{"faturamento -pagas negativa", func(a []string) error { return sc.billing(context.Background(), a) }, []string{"-pagas", "-0.1"}},
		{"faturamento -fracao acima de 1", func(a []string) error { return sc.billing(context.Background(), a) }, []string{"-fracao", "2"}},
		{"consumo -duplicados negativa", func(a []string) error { return sc.usage(context.Background(), a) }, []string{"-duplicados", "-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(tc.args)
			if err == nil || !strings.Contains(err.Error(), "entre 0 e 1") {
				t.Fatalf("esperada recusa da fração: %v", err)
			}
		})
	}
	if err := checkShare("pagas", 0); err != nil {
		t.Fatal(err)
	}
	if err := checkShare("pagas", 1); err != nil {
		t.Fatal(err)
	}
}
