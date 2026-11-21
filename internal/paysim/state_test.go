package paysim_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/paysim"
)

func TestLoadEmptyPathKeepsStateInMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	e := newEnv(t)
	e.create("", "fatura-1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Load(\"\") gravou arquivos: %v", entries)
	}
	// Outro estado em memória começa vazio e não compartilha sequências.
	other := newEnv(t)
	if list, _ := other.sim.ListCharges(merchantKey, "", time.Time{}, time.Time{}, 0, 10); len(list) != 0 {
		t.Fatalf("estado em memória compartilhado: %v", list)
	}
	if c := other.create("", "fatura-1"); c.ID != "ch_000001" {
		t.Fatalf("sequência compartilhada: %s", c.ID)
	}
}

func TestLoadFromFile(t *testing.T) {
	cases := []struct {
		name    string
		content *string
		wantErr string
	}{
		{name: "arquivo inexistente começa vazio"},
		{name: "arquivo ilegível", content: ptr("{não é json"), wantErr: "estado ilegível"},
		{name: "estado vazio válido", content: ptr("{}")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "paysim.json")
			if tc.content != nil {
				if err := os.WriteFile(path, []byte(*tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			st, err := paysim.Load(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("erro = %v, esperado %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := st.ChargesSorted(); len(got) != 0 {
				t.Fatalf("estado inicial com cobranças: %v", got)
			}
		})
	}
}

func TestStateSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paysim.json")
	clock := newClock()
	e := newEnvAt(t, path, clock)
	dup := e.create("duplicate_webhooks", "fatura-dup")
	if err := e.sim.EnqueueScenario(merchantKey, "decline"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("arquivo de estado não gravado: %v", err)
	}
	// Ainda não houve entrega: as duas entregas pendentes devem sobreviver à
	// recarga e chegar ao mesmo receptor, cuja configuração é persistida.
	st, err := paysim.Load(path)
	if err != nil {
		t.Fatalf("recarga: %v", err)
	}
	sim := paysim.New(st, "http://checkout.test", clock.Now, discardLogger())

	// A chave do comerciante dona da cobrança é restaurada.
	if _, err := sim.View(merchantKey, dup.ID); err != nil {
		t.Fatalf("View após recarga: %v", err)
	}
	if _, err := sim.View(otherKey, dup.ID); !errors.Is(err, paysim.ErrNotFound) {
		t.Fatalf("outro comerciante enxerga a cobrança após recarga: %v", err)
	}
	// As entregas pendentes e a configuração de webhook foram persistidas.
	sim.DispatchDue(t.Context())
	if keys := eventKeys(e.rcv.take()); !slices.Equal(keys, []string{"charge.created/evt_000001", "charge.created/evt_000001"}) {
		t.Fatalf("entregas pendentes após recarga = %v", keys)
	}
	// O cenário agendado continua na fila.
	if _, _, err := sim.CreateCharge(merchantKey, chargeInput("fatura-2")); !errors.Is(err, paysim.ErrDeclined) {
		t.Fatalf("cenário agendado perdido: %v", err)
	}
	// As sequências continuam de onde pararam.
	next, _, err := sim.CreateCharge(merchantKey, chargeInput("fatura-3"))
	if err != nil || next.ID != "ch_000002" {
		t.Fatalf("próxima cobrança: %+v, %v", next, err)
	}
	// A marca duplicate_webhooks é restaurada.
	sim.DispatchDue(t.Context())
	e.rcv.take()
	if _, err := sim.Apply(dup.ID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	sim.DispatchDue(t.Context())
	if keys := eventKeys(e.rcv.take()); !slices.Equal(keys, []string{"charge.paid/evt_000003", "charge.paid/evt_000003"}) {
		t.Fatalf("duplicidade após recarga = %v", keys)
	}
}

// Regressão: Charge.HeldEvents não era persistido (tag json:"-" e ausente de
// ChargeFlags). Depois de reiniciar o simulador com arquivo de estado, o
// charge.created retido de uma cobrança out_of_order nunca era entregue.
func TestOutOfOrderHeldEventSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paysim.json")
	clock := newClock()
	e := newEnvAt(t, path, clock)
	c := e.create("out_of_order", "fatura-ooo")

	st, err := paysim.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", clock.Now, discardLogger())
	if _, err := sim.Apply(c.ID, paysim.Action{Name: "pay"}); err != nil {
		t.Fatal(err)
	}
	sim.DispatchDue(t.Context())
	if keys := eventKeys(e.rcv.take()); !slices.Equal(keys, []string{"charge.paid/evt_000002", "charge.created/evt_000001"}) {
		t.Fatalf("entregas após recarga = %v, esperado o evento retido depois do pagamento", keys)
	}
}

// Regressão: Event.ChargeID e Event.Merchant têm a tag json:"-" (para não
// aparecerem no corpo do webhook) e não eram persistidos. Depois da recarga,
// Apply(replay) não encontrava nenhum evento da cobrança e não reenviava nada.
func TestReplayAfterReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paysim.json")
	clock := newClock()
	e := newEnvAt(t, path, clock)
	c := e.create("", "fatura-1")
	e.dispatch()

	st, err := paysim.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sim := paysim.New(st, "http://checkout.test", clock.Now, discardLogger())
	if _, err := sim.Apply(c.ID, paysim.Action{Name: "replay"}); err != nil {
		t.Fatal(err)
	}
	sim.DispatchDue(t.Context())
	if keys := eventKeys(e.rcv.take()); !slices.Equal(keys, []string{"charge.created/evt_000001"}) {
		t.Fatalf("replay após recarga = %v, esperado o charge.created", keys)
	}
}

// O vínculo do pagador com o comerciante sobrevive à recarga do estado: a
// idempotência pela referência externa continua restrita ao comerciante.
func TestCustomerMerchantSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paysim.json")
	e := newEnvAt(t, path, newClock())
	a, err := e.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Ana", ExternalReference: "cli-1"})
	if err != nil {
		t.Fatal(err)
	}
	reloaded := newEnvAt(t, path, newClock())
	if again, err := reloaded.sim.CreateCustomer(merchantKey, paysim.Customer{Name: "Ana", ExternalReference: "cli-1"}); err != nil || again != a {
		t.Fatalf("repetição depois da recarga: %+v, %v; esperado %+v", again, err, a)
	}
	other, err := reloaded.sim.CreateCustomer("outro_comerciante", paysim.Customer{Name: "Ana", ExternalReference: "cli-1"})
	if err != nil || other.ID == a.ID {
		t.Fatalf("outro comerciante depois da recarga: %+v, %v", other, err)
	}
}
