package paysim_test

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/payments/gateways/simulator"
	"github.com/ricartefelipe/recorvia/internal/paysim"
)

func TestCreateChargeAppliesScenario(t *testing.T) {
	cases := []struct {
		scenario    string
		wantErr     error
		wantCreated bool
		wantEvents  []string
	}{
		{scenario: "", wantCreated: true, wantEvents: []string{"charge.created/evt_000001"}},
		{scenario: "decline", wantErr: paysim.ErrDeclined},
		{scenario: "unavailable", wantErr: paysim.ErrUnavailable},
		// Os cenários de resposta (demora, erro após sucesso) criam a cobrança
		// normalmente; o efeito na resposta é aplicado pelo servidor HTTP.
		{scenario: "timeout_after_success", wantCreated: true, wantEvents: []string{"charge.created/evt_000001"}},
		{scenario: "error_after_success", wantCreated: true, wantEvents: []string{"charge.created/evt_000001"}},
		{scenario: "slow", wantCreated: true, wantEvents: []string{"charge.created/evt_000001"}},
		{scenario: "duplicate_webhooks", wantCreated: true, wantEvents: []string{"charge.created/evt_000001", "charge.created/evt_000001"}},
		// out_of_order retém o charge.created até o próximo evento.
		{scenario: "out_of_order", wantCreated: true, wantEvents: []string{}},
	}
	for _, tc := range cases {
		t.Run("cenario_"+tc.scenario, func(t *testing.T) {
			e := newEnv(t)
			if tc.scenario != "" {
				if err := e.sim.EnqueueScenario(merchantKey, tc.scenario); err != nil {
					t.Fatalf("EnqueueScenario: %v", err)
				}
			}
			c, applied, err := e.sim.CreateCharge(merchantKey, chargeInput("fatura-1"))
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("erro = %v, esperado %v", err, tc.wantErr)
			}
			if applied != tc.scenario {
				t.Fatalf("cenário aplicado = %q, esperado %q", applied, tc.scenario)
			}
			list, _ := e.sim.ListCharges(merchantKey, "fatura-1", time.Time{}, time.Time{}, 0, 10)
			if got := len(list) == 1; got != tc.wantCreated {
				t.Fatalf("cobrança criada = %v, esperado %v", got, tc.wantCreated)
			}
			if tc.wantCreated {
				if c.ID != "ch_000001" || c.Status != "pending" || c.CheckoutURL != "http://checkout.test/checkout/ch_000001" {
					t.Fatalf("cobrança inesperada: %+v", c)
				}
			}
			if got := eventKeys(e.dispatch()); !slices.Equal(got, tc.wantEvents) && (len(got) != 0 || len(tc.wantEvents) != 0) {
				t.Fatalf("eventos = %v, esperado %v", got, tc.wantEvents)
			}

			// O cenário vale só para uma criação: a próxima é normal.
			next, applied, err := e.sim.CreateCharge(merchantKey, chargeInput("fatura-2"))
			if err != nil || applied != "" || next.Status != "pending" {
				t.Fatalf("segunda criação: cobrança %+v, cenário %q, erro %v", next, applied, err)
			}
			if got := eventTypes(e.dispatch()); !slices.Equal(got, []string{"charge.created"}) {
				t.Fatalf("eventos da segunda criação = %v", got)
			}
		})
	}
}

func TestScenarioQueueIsFIFOAndPerMerchant(t *testing.T) {
	e := newEnv(t)
	for _, sc := range []string{"decline", "unavailable"} {
		if err := e.sim.EnqueueScenario(merchantKey, sc); err != nil {
			t.Fatal(err)
		}
	}
	// Outro comerciante não consome a fila do primeiro.
	if _, applied, err := e.sim.CreateCharge(otherKey, chargeInput("b-1")); err != nil || applied != "" {
		t.Fatalf("outro comerciante: cenário %q, erro %v", applied, err)
	}
	for i, want := range []error{paysim.ErrDeclined, paysim.ErrUnavailable, nil} {
		_, _, err := e.sim.CreateCharge(merchantKey, chargeInput("a-1"))
		if !errors.Is(err, want) || (want == nil && err != nil) {
			t.Fatalf("criação %d: erro = %v, esperado %v", i+1, err, want)
		}
	}
}

func TestEnqueueScenarioRejectsUnknown(t *testing.T) {
	e := newEnv(t)
	for _, sc := range []string{"", "diverge", "DECLINE", "refund"} {
		if err := e.sim.EnqueueScenario(merchantKey, sc); err == nil {
			t.Errorf("cenário %q aceito", sc)
		}
	}
	for _, sc := range append(append(slices.Clone(paysim.Scenarios), paysim.RefundScenarios...), paysim.CancelScenarios...) {
		if err := e.sim.EnqueueScenario(merchantKey, sc); err != nil {
			t.Errorf("cenário %q recusado: %v", sc, err)
		}
	}
}

func TestCreateChargeValidatesInput(t *testing.T) {
	cases := map[string]func(*paysim.CreateChargeInput){
		"valor zero":             func(in *paysim.CreateChargeInput) { in.Amount = 0 },
		"valor negativo":         func(in *paysim.CreateChargeInput) { in.Amount = -1 },
		"moeda diferente":        func(in *paysim.CreateChargeInput) { in.Currency = "USD" },
		"sem referência externa": func(in *paysim.CreateChargeInput) { in.ExternalReference = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if err := e.sim.EnqueueScenario(merchantKey, "decline"); err != nil {
				t.Fatal(err)
			}
			in := chargeInput("fatura-1")
			mutate(&in)
			if _, _, err := e.sim.CreateCharge(merchantKey, in); err == nil || errors.Is(err, paysim.ErrDeclined) {
				t.Fatalf("erro = %v, esperado pedido inválido", err)
			}
			// O pedido inválido não consome o cenário agendado.
			if _, _, err := e.sim.CreateCharge(merchantKey, chargeInput("fatura-1")); !errors.Is(err, paysim.ErrDeclined) {
				t.Fatalf("cenário não preservado: %v", err)
			}
		})
	}
}

func TestDuplicateWebhooksDeliversSameEventTwice(t *testing.T) {
	e := newEnv(t)
	dup := e.create("duplicate_webhooks", "fatura-dup")
	normal := e.create("", "fatura-normal")

	got := e.dispatch()
	want := []string{"charge.created/evt_000001", "charge.created/evt_000001", "charge.created/evt_000002"}
	if keys := eventKeys(got); !slices.Equal(keys, want) {
		t.Fatalf("entregas = %v, esperado %v", keys, want)
	}
	if !bytes.Equal(got[0].Body, got[1].Body) || got[0].Charge.ID != dup.ID {
		t.Fatalf("as duas entregas do evento duplicado diferem:\n%s\n%s", got[0].Body, got[1].Body)
	}
	if got[2].Charge.ID != normal.ID {
		t.Fatalf("cobrança normal = %s", got[2].Charge.ID)
	}

	// A duplicidade vale para todos os eventos da cobrança, não só o primeiro.
	e.apply(dup.ID, paysim.Action{Name: "pay"})
	e.apply(normal.ID, paysim.Action{Name: "pay"})
	want = []string{"charge.paid/evt_000003", "charge.paid/evt_000003", "charge.paid/evt_000004"}
	if keys := eventKeys(e.dispatch()); !slices.Equal(keys, want) {
		t.Fatalf("entregas após pagamento = %v, esperado %v", keys, want)
	}
}

func TestOutOfOrderDeliversLaterEventFirst(t *testing.T) {
	cases := []struct {
		name    string
		trigger func(e *env, id string)
		want    []string
	}{
		{name: "pagamento", trigger: func(e *env, id string) { e.apply(id, paysim.Action{Name: "pay"}) },
			want: []string{"charge.paid/evt_000002", "charge.created/evt_000001"}},
		{name: "recusa", trigger: func(e *env, id string) { e.apply(id, paysim.Action{Name: "fail"}) },
			want: []string{"charge.failed/evt_000002", "charge.created/evt_000001"}},
		{name: "cancelamento", trigger: func(e *env, id string) {
			if _, err := e.sim.Cancel(merchantKey, id); err != nil {
				e.t.Fatal(err)
			}
		}, want: []string{"charge.canceled/evt_000002", "charge.created/evt_000001"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.create("out_of_order", "fatura-ooo")
			if n := e.sim.DispatchDue(t.Context()); n != 0 {
				t.Fatalf("DispatchDue = %d com o evento retido, esperado 0", n)
			}
			if got := e.rcv.take(); len(got) != 0 {
				t.Fatalf("evento retido foi entregue: %v", eventKeys(got))
			}

			e.clock.Advance(time.Minute)
			tc.trigger(e, c.ID)
			got := e.dispatch()
			if keys := eventKeys(got); !slices.Equal(keys, tc.want) {
				t.Fatalf("entregas = %v, esperado %v", keys, tc.want)
			}
			// O evento entregue primeiro é o posterior.
			if !got[0].At.After(got[1].At) {
				t.Fatalf("created_at: primeiro %v, segundo %v; esperado primeiro posterior", got[0].At, got[1].At)
			}
			if got[1].Charge.Status != "pending" {
				t.Fatalf("o evento retido deve carregar a situação da criação, veio %q", got[1].Charge.Status)
			}
		})
	}

	t.Run("somente o primeiro evento é retido", func(t *testing.T) {
		e := newEnv(t)
		c := e.create("out_of_order", "fatura-ooo")
		e.apply(c.ID, paysim.Action{Name: "pay"})
		e.dispatch()
		e.apply(c.ID, paysim.Action{Name: "refund", Amount: 1000})
		if keys := eventKeys(e.dispatch()); !slices.Equal(keys, []string{"charge.partially_refunded/evt_000003"}) {
			t.Fatalf("entregas = %v", keys)
		}
	})
}

func TestDivergeChangesProviderValuesWithoutWebhook(t *testing.T) {
	cases := []struct {
		name                 string
		action               paysim.Action
		wantPaid, wantFee    int64
		wantNet, wantRefunds int64
	}{
		{name: "valor pago", action: paysim.Action{Name: "diverge", Amount: 9000}, wantPaid: 9000, wantFee: 348, wantNet: 8652},
		{name: "taxa", action: paysim.Action{Name: "diverge", Fee: ptr[int64](500)}, wantPaid: 10000, wantFee: 500, wantNet: 9500},
		{name: "valor e taxa", action: paysim.Action{Name: "diverge", Amount: 12000, Fee: ptr[int64](0)}, wantPaid: 12000, wantFee: 0, wantNet: 12000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.create("", "fatura-div")
			e.apply(c.ID, paysim.Action{Name: "pay"})
			paidEvents := e.dispatch()
			paidAt := e.clock.Now()

			e.clock.Advance(time.Hour)
			got := e.apply(c.ID, tc.action)
			if got.Status != "paid" || got.PaidAmount != tc.wantPaid || got.Fee != tc.wantFee || got.NetAmount != tc.wantNet {
				t.Fatalf("cobrança após divergência: %+v", got)
			}
			if !got.UpdatedAt.Equal(e.clock.Now()) {
				t.Fatalf("updated_at = %v, esperado %v", got.UpdatedAt, e.clock.Now())
			}
			// Nenhum webhook: o integrador só descobre pela conciliação.
			if w := e.dispatch(); len(w) != 0 {
				t.Fatalf("divergência notificou: %v", eventKeys(w))
			}
			last := paidEvents[len(paidEvents)-1].Charge
			if last.PaidAmount != 10000 || last.Fee != paysim.Fee(10000) {
				t.Fatalf("o webhook de pagamento deveria ter os valores originais: %+v", last)
			}
			// A listagem por janela de atualização (usada na conciliação)
			// mostra os valores divergentes depois do pagamento.
			list, _ := e.sim.ListCharges(merchantKey, "", paidAt.Add(time.Second), time.Time{}, 0, 10)
			if len(list) != 1 || list[0].PaidAmount != tc.wantPaid || list[0].Fee != tc.wantFee || list[0].NetAmount != tc.wantNet {
				t.Fatalf("listagem para conciliação = %+v", list)
			}
		})
	}
}

func TestApplyActions(t *testing.T) {
	cases := []struct {
		name       string
		setup      []paysim.Action
		action     paysim.Action
		wantErr    bool
		wantStatus string
		wantEvents []string
		check      func(t *testing.T, c paysim.ChargeView)
	}{
		{name: "pay", action: paysim.Action{Name: "pay"}, wantStatus: "paid", wantEvents: []string{"charge.paid"},
			check: func(t *testing.T, c paysim.ChargeView) {
				if c.PaidAmount != 10000 || c.Fee != 348 || c.NetAmount != 9652 || c.PaidAt == nil {
					t.Fatalf("valores do pagamento: %+v", c)
				}
			}},
		{name: "pay com valor e taxa informados", action: paysim.Action{Name: "pay", Amount: 9500, Fee: ptr[int64](100)}, wantStatus: "paid",
			wantEvents: []string{"charge.paid"}, check: func(t *testing.T, c paysim.ChargeView) {
				if c.PaidAmount != 9500 || c.Fee != 100 || c.NetAmount != 9400 {
					t.Fatalf("valores do pagamento: %+v", c)
				}
			}},
		{name: "pay_silently não notifica", action: paysim.Action{Name: "pay_silently"}, wantStatus: "paid", wantEvents: []string{}},
		{name: "pay depois de recusa", setup: []paysim.Action{{Name: "fail"}}, action: paysim.Action{Name: "pay"}, wantStatus: "paid",
			wantEvents: []string{"charge.paid"}},
		{name: "pay duas vezes", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "pay"}, wantErr: true, wantStatus: "paid"},
		{name: "fail", action: paysim.Action{Name: "fail"}, wantStatus: "failed", wantEvents: []string{"charge.failed"}},
		{name: "fail depois de pago", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "fail"}, wantErr: true, wantStatus: "paid"},
		{name: "refund parcial", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "refund", Amount: 4000},
			wantStatus: "partially_refunded", wantEvents: []string{"charge.partially_refunded"}, check: func(t *testing.T, c paysim.ChargeView) {
				if c.RefundedAmount != 4000 || len(c.Refunds) != 1 || c.Refunds[0].Amount != 4000 || c.Refunds[0].ID != "rf_000001" {
					t.Fatalf("reembolso: %+v", c)
				}
			}},
		{name: "refund do saldo restante", setup: []paysim.Action{{Name: "pay"}, {Name: "refund", Amount: 4000}}, action: paysim.Action{Name: "refund"},
			wantStatus: "refunded", wantEvents: []string{"charge.refunded"}, check: func(t *testing.T, c paysim.ChargeView) {
				if c.RefundedAmount != 10000 || len(c.Refunds) != 2 || c.Refunds[1].Amount != 6000 {
					t.Fatalf("reembolsos: %+v", c)
				}
			}},
		{name: "refund acima do saldo", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "refund", Amount: 10001}, wantErr: true, wantStatus: "paid"},
		{name: "refund de cobrança pendente", action: paysim.Action{Name: "refund"}, wantErr: true, wantStatus: "pending"},
		{name: "chargeback aberto", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "chargeback", Stage: "requested"},
			wantStatus: "chargeback_requested", wantEvents: []string{"charge.chargeback_requested"}},
		{name: "chargeback ganho", setup: []paysim.Action{{Name: "pay"}, {Name: "chargeback", Stage: "requested"}},
			action: paysim.Action{Name: "chargeback", Stage: "won"}, wantStatus: "paid", wantEvents: []string{"charge.chargeback_won"},
			check: func(t *testing.T, c paysim.ChargeView) {
				if c.ChargebackStatus != "won" {
					t.Fatalf("chargeback_status = %q", c.ChargebackStatus)
				}
			}},
		{name: "chargeback perdido", setup: []paysim.Action{{Name: "pay"}, {Name: "chargeback", Stage: "requested"}},
			action: paysim.Action{Name: "chargeback", Stage: "lost"}, wantStatus: "chargeback_lost", wantEvents: []string{"charge.chargeback_lost"}},
		{name: "chargeback ganho sem disputa aberta", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "chargeback", Stage: "won"},
			wantErr: true, wantStatus: "paid"},
		{name: "chargeback de cobrança pendente", action: paysim.Action{Name: "chargeback", Stage: "requested"}, wantErr: true, wantStatus: "pending"},
		{name: "chargeback com estágio inválido", setup: []paysim.Action{{Name: "pay"}}, action: paysim.Action{Name: "chargeback", Stage: "x"},
			wantErr: true, wantStatus: "paid"},
		{name: "ação desconhecida", action: paysim.Action{Name: "explode"}, wantErr: true, wantStatus: "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.create("", "fatura-1")
			for _, a := range tc.setup {
				e.apply(c.ID, a)
			}
			e.dispatch()
			e.clock.Advance(time.Minute)

			_, err := e.sim.Apply(c.ID, tc.action)
			if (err != nil) != tc.wantErr {
				t.Fatalf("erro = %v, esperado erro: %v", err, tc.wantErr)
			}
			v := e.view(c.ID)
			if v.Status != tc.wantStatus {
				t.Fatalf("situação = %q, esperado %q", v.Status, tc.wantStatus)
			}
			if got := eventTypes(e.dispatch()); !slices.Equal(got, tc.wantEvents) && (len(got) != 0 || len(tc.wantEvents) != 0) {
				t.Fatalf("eventos = %v, esperado %v", got, tc.wantEvents)
			}
			if tc.check != nil {
				tc.check(t, v)
			}
		})
	}

	t.Run("cobrança inexistente", func(t *testing.T) {
		e := newEnv(t)
		if _, err := e.sim.Apply("ch_999999", paysim.Action{Name: "pay"}); !errors.Is(err, paysim.ErrNotFound) {
			t.Fatalf("erro = %v, esperado ErrNotFound", err)
		}
	})
}

func TestReplayRedeliversEventsOfTheCharge(t *testing.T) {
	e := newEnv(t)
	c := e.create("", "fatura-1")
	other := e.create("", "fatura-2")
	e.apply(c.ID, paysim.Action{Name: "pay"})
	e.apply(other.ID, paysim.Action{Name: "pay"})
	first := e.dispatch()

	e.apply(c.ID, paysim.Action{Name: "replay"})
	got := e.dispatch()
	keys := eventKeys(got)
	slices.Sort(keys)
	if want := []string{"charge.created/evt_000001", "charge.paid/evt_000003"}; !slices.Equal(keys, want) {
		t.Fatalf("reenvio = %v, esperado %v", keys, want)
	}
	// O reenvio usa o mesmo corpo do evento original (mesmo id, mesmos dados).
	for _, w := range got {
		i := slices.IndexFunc(first, func(f webhook) bool { return f.ID == w.ID })
		if i < 0 || !bytes.Equal(first[i].Body, w.Body) {
			t.Fatalf("corpo do reenvio de %s difere do original", w.ID)
		}
	}
}

// Regressão: a ordem do replay usava o instante de criação (relógio sem
// leitura monotônica) e desempatava pelo id em texto, que deixa de seguir a
// ordem numérica a partir de evt_1000000. A ordem é a sequência numérica.
func TestReplayOrderFollowsNumericSequenceBeyondSixDigits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paysim.json")
	if err := os.WriteFile(path, []byte(`{"seq":{"evt":999998}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newEnvAt(t, path, newClock())
	c := e.create("", "fatura-milhao")
	e.apply(c.ID, paysim.Action{Name: "pay"})
	e.apply(c.ID, paysim.Action{Name: "refund", Amount: 1000})
	e.dispatch()
	want := []string{"evt_999999", "evt_1000000", "evt_1000001"}
	for _, reload := range []bool{false, true} {
		if reload {
			// A ordem sobrevive à recarga do estado.
			e = newEnvAt(t, path, e.clock)
		}
		e.apply(c.ID, paysim.Action{Name: "replay"})
		got := e.dispatch()
		ids := make([]string, 0, len(got))
		for _, w := range got {
			ids = append(ids, w.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("replay (recarga=%v): ordem = %v, esperado %v", reload, ids, want)
		}
	}
}

// Regressão: o reenvio percorria o mapa de eventos (State.s.Events), cuja
// ordem de iteração é aleatória, e a ordem das entregas do replay mudava
// entre execuções. A mesma sequência de comandos produz o mesmo resultado.
func TestReplayOrderIsDeterministic(t *testing.T) {
	e := newEnv(t)
	c := e.create("", "fatura-1")
	e.apply(c.ID, paysim.Action{Name: "pay"})
	e.apply(c.ID, paysim.Action{Name: "refund", Amount: 1000})
	e.apply(c.ID, paysim.Action{Name: "refund", Amount: 2000})
	e.apply(c.ID, paysim.Action{Name: "refund"})
	e.dispatch()
	want := []string{"evt_000001", "evt_000002", "evt_000003", "evt_000004", "evt_000005"}
	for i := range 20 {
		e.apply(c.ID, paysim.Action{Name: "replay"})
		got := e.dispatch()
		ids := make([]string, 0, len(got))
		for _, w := range got {
			ids = append(ids, w.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("replay %d: ordem = %v, esperado %v", i+1, ids, want)
		}
	}
}

func TestRefundScenariosAndIdempotency(t *testing.T) {
	cases := []struct {
		name         string
		scenario     string
		key          string
		in           paysim.RefundInput
		wantErr      error
		wantRefunded int64
		wantEvents   []string
	}{
		{name: "sucesso", in: paysim.RefundInput{Amount: 3000, ExternalReference: "rf-1"}, wantRefunded: 3000,
			wantEvents: []string{"charge.partially_refunded"}},
		{name: "refund_unavailable não processa", scenario: "refund_unavailable", in: paysim.RefundInput{Amount: 3000, ExternalReference: "rf-1"},
			wantErr: paysim.ErrRefundUnavailable, wantRefunded: 0, wantEvents: []string{}},
		{name: "refund_error_after_success efetiva e perde a resposta", scenario: "refund_error_after_success",
			in: paysim.RefundInput{Amount: 3000, ExternalReference: "rf-1"}, wantErr: paysim.ErrRefundResponseLost, wantRefunded: 3000,
			wantEvents: []string{"charge.partially_refunded"}},
		{name: "outro comerciante", key: otherKey, in: paysim.RefundInput{Amount: 3000}, wantErr: paysim.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.create("", "fatura-1")
			e.apply(c.ID, paysim.Action{Name: "pay"})
			e.dispatch()
			if tc.scenario != "" {
				if err := e.sim.EnqueueScenario(merchantKey, tc.scenario); err != nil {
					t.Fatal(err)
				}
			}
			key := merchantKey
			if tc.key != "" {
				key = tc.key
			}
			r, err := e.sim.Refund(key, c.ID, tc.in)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("erro = %v, esperado %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (r.ID != "rf_000001" || r.Amount != tc.in.Amount || r.Status != "succeeded") {
				t.Fatalf("reembolso = %+v", r)
			}
			if v := e.view(c.ID); v.RefundedAmount != tc.wantRefunded {
				t.Fatalf("reembolsado = %d, esperado %d", v.RefundedAmount, tc.wantRefunded)
			}
			if got := eventTypes(e.dispatch()); !slices.Equal(got, tc.wantEvents) && (len(got) != 0 || len(tc.wantEvents) != 0) {
				t.Fatalf("eventos = %v, esperado %v", got, tc.wantEvents)
			}
			if tc.key != "" {
				return
			}
			// Nova tentativa com a mesma referência: devolve o reembolso
			// existente (ou efetiva agora, se o primeiro pedido foi recusado)
			// sem reembolsar duas vezes.
			again, err := e.sim.Refund(merchantKey, c.ID, tc.in)
			if err != nil || again.ID != "rf_000001" || again.Amount != tc.in.Amount {
				t.Fatalf("nova tentativa: %+v, %v", again, err)
			}
			if v := e.view(c.ID); v.RefundedAmount != tc.in.Amount || len(v.Refunds) != 1 {
				t.Fatalf("após nova tentativa: reembolsado %d, reembolsos %d", v.RefundedAmount, len(v.Refunds))
			}
		})
	}
}

func TestCancel(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(e *env, id string)
		key        string
		wantErr    error
		wantStatus string
		wantEvents []string
	}{
		{name: "pendente", wantStatus: "canceled", wantEvents: []string{"charge.canceled"}},
		{name: "recusada", setup: func(e *env, id string) { e.apply(id, paysim.Action{Name: "fail"}) }, wantStatus: "canceled",
			wantEvents: []string{"charge.canceled"}},
		{name: "já cancelada é idempotente", setup: func(e *env, id string) {
			if _, err := e.sim.Cancel(merchantKey, id); err != nil {
				e.t.Fatal(err)
			}
		}, wantStatus: "canceled", wantEvents: []string{}},
		{name: "paga", setup: func(e *env, id string) { e.apply(id, paysim.Action{Name: "pay"}) }, wantErr: paysim.ErrNotCancelable,
			wantStatus: "paid", wantEvents: []string{}},
		{name: "cancel_unavailable", setup: func(e *env, _ string) {
			if err := e.sim.EnqueueScenario(merchantKey, "cancel_unavailable"); err != nil {
				e.t.Fatal(err)
			}
		}, wantErr: paysim.ErrCancelUnavailable, wantStatus: "pending", wantEvents: []string{}},
		{name: "outro comerciante", key: otherKey, wantErr: paysim.ErrNotFound, wantStatus: "pending", wantEvents: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			c := e.create("", "fatura-1")
			if tc.setup != nil {
				tc.setup(e, c.ID)
			}
			e.dispatch()
			key := merchantKey
			if tc.key != "" {
				key = tc.key
			}
			_, err := e.sim.Cancel(key, c.ID)
			if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
				t.Fatalf("erro = %v, esperado %v", err, tc.wantErr)
			}
			if v := e.view(c.ID); v.Status != tc.wantStatus {
				t.Fatalf("situação = %q, esperado %q", v.Status, tc.wantStatus)
			}
			if got := eventTypes(e.dispatch()); !slices.Equal(got, tc.wantEvents) && (len(got) != 0 || len(tc.wantEvents) != 0) {
				t.Fatalf("eventos = %v, esperado %v", got, tc.wantEvents)
			}
		})
	}

	t.Run("cancel_unavailable vale só para um pedido", func(t *testing.T) {
		e := newEnv(t)
		c := e.create("", "fatura-1")
		if err := e.sim.EnqueueScenario(merchantKey, "cancel_unavailable"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.sim.Cancel(merchantKey, c.ID); !errors.Is(err, paysim.ErrCancelUnavailable) {
			t.Fatalf("primeiro cancelamento: %v", err)
		}
		if v, err := e.sim.Cancel(merchantKey, c.ID); err != nil || v.Status != "canceled" {
			t.Fatalf("segundo cancelamento: %+v, %v", v, err)
		}
	})
}

func TestCreateCustomerIsIdempotentByExternalReference(t *testing.T) {
	e := newEnv(t)
	a, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Ana", ExternalReference: "cli-1"})
	if err != nil || a.ID != "cus_000001" {
		t.Fatalf("primeiro cliente: %+v, %v", a, err)
	}
	again, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Ana Maria", ExternalReference: "cli-1"})
	if err != nil || again != a {
		t.Fatalf("repetição: %+v, %v; esperado %+v", again, err, a)
	}
	b, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Bruno", ExternalReference: "cli-2"})
	if err != nil || b.ID != "cus_000002" {
		t.Fatalf("segundo cliente: %+v, %v", b, err)
	}
	// Sem referência externa, cada pedido cria um pagador novo.
	for _, want := range []string{"cus_000003", "cus_000004"} {
		c, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Sem referência"})
		if err != nil || c.ID != want {
			t.Fatalf("cliente sem referência: %+v, %v; esperado %s", c, err, want)
		}
	}
}

func TestFee(t *testing.T) {
	cases := []struct{ amount, want int64 }{
		{amount: 0, want: 49},
		{amount: 1000, want: 79},   // 29,9 -> 30
		{amount: 5000, want: 199},  // 149,5 -> 150 (empate, par acima)
		{amount: 10000, want: 348}, // 299 exato
		{amount: 15000, want: 497}, // 448,5 -> 448 (empate, par abaixo)
		{amount: 50, want: 50},     // 1,495 -> 1
	}
	for _, tc := range cases {
		if got := paysim.Fee(tc.amount); got != tc.want {
			t.Errorf("Fee(%d) = %d, esperado %d", tc.amount, got, tc.want)
		}
	}
}

func TestConfigureWebhookValidates(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		name, url, secret string
		ok                bool
	}{
		{name: "https", url: "https://recorvia.test/webhooks", secret: webhookSecret, ok: true},
		{name: "sem esquema", url: "recorvia.test/webhooks", secret: webhookSecret},
		{name: "esquema não http", url: "ftp://recorvia.test", secret: webhookSecret},
		{name: "segredo curto", url: "https://recorvia.test/webhooks", secret: "curto"},
	}
	for _, tc := range cases {
		if err := e.sim.ConfigureWebhook(otherKey, tc.url, tc.secret); (err == nil) != tc.ok {
			t.Errorf("%s: erro = %v, esperado sucesso %v", tc.name, err, tc.ok)
		}
	}
}

func TestDispatchRetriesWithBackoff(t *testing.T) {
	e := newEnv(t)
	e.create("", "fatura-1")
	e.rcv.setStatus(http.StatusInternalServerError)

	steps := []struct {
		advance  time.Duration
		status   int
		attempts int
	}{
		{advance: 0, status: http.StatusInternalServerError, attempts: 1},
		{advance: 0, status: http.StatusInternalServerError, attempts: 0},           // aguarda 2 s
		{advance: time.Second, status: http.StatusInternalServerError, attempts: 0}, // ainda dentro da espera
		{advance: time.Second, status: http.StatusInternalServerError, attempts: 1}, // 2 s depois: nova falha, espera 4 s
		{advance: 3 * time.Second, status: http.StatusOK, attempts: 0},
		{advance: time.Second, status: http.StatusOK, attempts: 1}, // entregue
		{advance: time.Hour, status: http.StatusOK, attempts: 0},   // concluída, não reenvia
	}
	for i, s := range steps {
		e.clock.Advance(s.advance)
		e.rcv.setStatus(s.status)
		if n := e.sim.DispatchDue(t.Context()); n != s.attempts {
			t.Fatalf("passo %d: DispatchDue = %d, esperado %d", i+1, n, s.attempts)
		}
	}
	got := e.rcv.take()
	if keys := eventKeys(got); !slices.Equal(keys, []string{"charge.created/evt_000001", "charge.created/evt_000001", "charge.created/evt_000001"}) {
		t.Fatalf("tentativas = %v", keys)
	}
	// Cada tentativa é assinada com o instante do envio.
	if got[0].Header.Get(paysim.SignatureHeader) == got[2].Header.Get(paysim.SignatureHeader) {
		t.Fatalf("a assinatura deveria mudar entre tentativas em instantes diferentes")
	}
}

func TestDispatchWaitsForWebhookConfiguration(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.sim.CreateCharge(otherKey, chargeInput("b-1")); err != nil {
		t.Fatal(err)
	}
	if n := e.sim.DispatchDue(t.Context()); n != 0 {
		t.Fatalf("DispatchDue sem webhook configurado = %d", n)
	}
	if err := e.sim.ConfigureWebhook(otherKey, e.rcv.srv.URL+"/b", webhookSecret); err != nil {
		t.Fatal(err)
	}
	if keys := eventKeys(e.dispatch()); !slices.Equal(keys, []string{"charge.created/evt_000001"}) {
		t.Fatalf("entregas após configurar = %v", keys)
	}
}

func TestWebhookSignatureMatchesGatewayVerifier(t *testing.T) {
	e := newEnv(t)
	e.create("", "fatura-1")
	got := e.dispatch()
	if len(got) != 1 {
		t.Fatalf("entregas = %d", len(got))
	}
	w := got[0]
	ts := e.clock.Now().Unix()
	if h, want := w.Header.Get(paysim.SignatureHeader), "t="+strconv.FormatInt(ts, 10)+",v1="+paysim.Sign(webhookSecret, ts, w.Body); h != want {
		t.Fatalf("header = %q, esperado %q", h, want)
	}
	if ct := w.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}

	tampered := bytes.Replace(w.Body, []byte(`"pending"`), []byte(`"paid"`), 1)
	cases := []struct {
		name    string
		body    []byte
		secret  string
		now     time.Time
		wantErr bool
	}{
		{name: "válida", body: w.Body, secret: webhookSecret, now: e.clock.Now()},
		{name: "dentro da tolerância", body: w.Body, secret: webhookSecret, now: e.clock.Now().Add(4 * time.Minute)},
		{name: "segredo errado", body: w.Body, secret: "outro_segredo_qualquer", now: e.clock.Now(), wantErr: true},
		{name: "corpo adulterado", body: tampered, secret: webhookSecret, now: e.clock.Now(), wantErr: true},
		{name: "fora da janela", body: w.Body, secret: webhookSecret, now: e.clock.Now().Add(6 * time.Minute), wantErr: true},
	}
	for _, tc := range cases {
		err := simulator.Verifier{}.Verify(w.Header, tc.body, tc.secret, tc.now)
		if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, simulator.ErrSignature)) {
			t.Errorf("%s: erro = %v, esperado erro: %v", tc.name, err, tc.wantErr)
		}
	}

	ev, err := simulator.Verifier{}.Parse(w.Body)
	if err != nil || ev.EventID != "evt_000001" || ev.Type != "charge.created" || ev.ProviderChargeID != "ch_000001" {
		t.Fatalf("Parse = %+v, %v", ev, err)
	}
}

func TestSignIsHMACSHA256OfTimestampAndBody(t *testing.T) {
	// Vetores calculados de forma independente:
	// printf '1700000000.{}' | openssl dgst -sha256 -hmac segredo
	cases := []struct {
		secret string
		ts     int64
		body   string
		want   string
	}{
		{secret: "segredo", ts: 1700000000, body: "{}", want: "28d73844c84580182772a1e98a60aeb8f04184b1bef0d4e147cfa59ebf0bfedc"},
	}
	for _, tc := range cases {
		if got := paysim.Sign(tc.secret, tc.ts, []byte(tc.body)); got != tc.want {
			t.Errorf("Sign(%q, %d, %q) = %s, esperado %s", tc.secret, tc.ts, tc.body, got, tc.want)
		}
	}
	base := paysim.Sign("segredo", 1700000000, []byte("{}"))
	if paysim.Sign("segredo", 1700000001, []byte("{}")) == base || paysim.Sign("segredo", 1700000000, []byte("{ }")) == base ||
		paysim.Sign("outro", 1700000000, []byte("{}")) == base {
		t.Fatalf("assinatura não depende de timestamp, corpo e segredo")
	}
}

func TestListChargesFiltersAndPaginates(t *testing.T) {
	e := newEnv(t)
	start := e.clock.Now()
	for _, ref := range []string{"f-1", "f-2", "f-3"} {
		e.create("", ref)
		e.clock.Advance(time.Minute)
	}
	if _, _, err := e.sim.CreateCharge(otherKey, chargeInput("f-1")); err != nil {
		t.Fatal(err)
	}
	ids := func(vs []paysim.ChargeView) []string {
		out := []string{}
		for _, v := range vs {
			out = append(out, v.ID)
		}
		return out
	}
	cases := []struct {
		name     string
		ref      string
		from, to time.Time
		offset   int
		limit    int
		want     []string
		wantMore bool
	}{
		{name: "todas, mais recentes primeiro", limit: 10, want: []string{"ch_000003", "ch_000002", "ch_000001"}},
		{name: "por referência", ref: "f-1", limit: 10, want: []string{"ch_000001"}},
		{name: "janela [início, fim)", from: start.Add(time.Minute), to: start.Add(2 * time.Minute), limit: 10, want: []string{"ch_000002"}},
		{name: "primeira página", limit: 2, want: []string{"ch_000003", "ch_000002"}, wantMore: true},
		{name: "última página", offset: 2, limit: 2, want: []string{"ch_000001"}},
		{name: "além do fim", offset: 5, limit: 2, want: []string{}},
	}
	for _, tc := range cases {
		got, more := e.sim.ListCharges(merchantKey, tc.ref, tc.from, tc.to, tc.offset, tc.limit)
		if !slices.Equal(ids(got), tc.want) || more != tc.wantMore {
			t.Errorf("%s: %v (mais: %v), esperado %v (mais: %v)", tc.name, ids(got), more, tc.want, tc.wantMore)
		}
	}
}

// runScript executa a mesma sequência de comandos e devolve tudo o que é
// observável pelo integrador: cobranças devolvidas, reembolsos e webhooks.
func runScript(t *testing.T, e *env) ([]paysim.Charge, []paysim.Refund, []webhook) {
	t.Helper()
	var charges []paysim.Charge
	var refunds []paysim.Refund
	var hooks []webhook
	if _, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Ana", ExternalReference: "cli-1"}); err != nil {
		t.Fatal(err)
	}
	a := e.create("", "fatura-a")
	b := e.create("duplicate_webhooks", "fatura-b")
	c := e.create("out_of_order", "fatura-c")
	charges = append(charges, a, b, c)
	hooks = append(hooks, e.dispatch()...)
	e.clock.Advance(time.Minute)
	charges = append(charges, e.apply(a.ID, paysim.Action{Name: "pay"}), e.apply(b.ID, paysim.Action{Name: "pay", Amount: 9000}),
		e.apply(c.ID, paysim.Action{Name: "pay"}))
	hooks = append(hooks, e.dispatch()...)
	e.clock.Advance(time.Minute)
	r, err := e.sim.Refund(merchantKey, a.ID, paysim.RefundInput{Amount: 2500, ExternalReference: "rf-a"})
	if err != nil {
		t.Fatal(err)
	}
	refunds = append(refunds, r)
	charges = append(charges, e.apply(c.ID, paysim.Action{Name: "diverge", Fee: ptr[int64](10)}))
	hooks = append(hooks, e.dispatch()...)
	return charges, refunds, hooks
}

func TestSameCommandsProduceSameResult(t *testing.T) {
	c1, r1, h1 := runScript(t, newEnv(t))
	c2, r2, h2 := runScript(t, newEnv(t))

	if !reflect.DeepEqual(c1, c2) {
		t.Fatalf("cobranças diferem:\n%+v\n%+v", c1, c2)
	}
	if !reflect.DeepEqual(r1, r2) {
		t.Fatalf("reembolsos diferem:\n%+v\n%+v", r1, r2)
	}
	if len(h1) != len(h2) {
		t.Fatalf("quantidade de webhooks difere: %d e %d", len(h1), len(h2))
	}
	for i := range h1 {
		if !bytes.Equal(h1[i].Body, h2[i].Body) || h1[i].Header.Get(paysim.SignatureHeader) != h2[i].Header.Get(paysim.SignatureHeader) {
			t.Fatalf("webhook %d difere:\n%s\n%s", i, h1[i].Body, h2[i].Body)
		}
	}

	// Identificadores sequenciais por prefixo, na ordem das operações.
	wantIDs := []string{"ch_000001", "ch_000002", "ch_000003"}
	for i, want := range wantIDs {
		if c1[i].ID != want {
			t.Errorf("cobrança %d: id %s, esperado %s", i, c1[i].ID, want)
		}
	}
	if r1[0].ID != "rf_000001" {
		t.Errorf("reembolso: id %s", r1[0].ID)
	}
	wantHooks := []string{
		"charge.created/evt_000001",
		"charge.created/evt_000002", "charge.created/evt_000002", // duplicate_webhooks
		"charge.paid/evt_000004",
		"charge.paid/evt_000005", "charge.paid/evt_000005",
		"charge.paid/evt_000006", "charge.created/evt_000003", // out_of_order
		"charge.partially_refunded/evt_000007",
	}
	if got := eventKeys(h1); !slices.Equal(got, wantHooks) {
		t.Fatalf("webhooks = %v\nesperado   %v", got, wantHooks)
	}
}
