// Package paysim implementa um provedor de pagamentos simulado,
// determinístico, para desenvolvimento e testes. Ele expõe uma API inspirada
// em provedores reais (clientes, cobranças, consulta por referência externa,
// reembolso, listagem para conciliação), checkout hospedado e webhooks
// assinados com HMAC-SHA256.
//
// Cenários são enfileirados por comerciante e consumidos na próxima criação
// de cobrança: decline, unavailable, timeout_after_success,
// error_after_success, slow, duplicate_webhooks e out_of_order. Cenários de
// reembolso (refund_unavailable, refund_error_after_success) e de
// cancelamento (cancel_unavailable) valem para o próximo pedido do tipo.
// Ações sobre cobranças (pagar, falhar, reembolsar, chargeback, divergência,
// pagamento sem webhook, reenvio de eventos) permitem reproduzir falhas de
// forma controlada; DELETE /v1/charges/{id} cancela a cobrança pendente.
// Identificadores são sequenciais e o estado é persistido em arquivo, de
// modo que a mesma sequência de comandos produz o mesmo resultado.
package paysim

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// Charge é uma cobrança simulada.
type Charge struct {
	ID                string     `json:"id"`
	MerchantKey       string     `json:"-"`
	CustomerID        string     `json:"customer_id"`
	ExternalReference string     `json:"external_reference"`
	Description       string     `json:"description"`
	Amount            int64      `json:"amount"`
	Currency          string     `json:"currency"`
	DueDate           string     `json:"due_date"`
	Status            string     `json:"status"`
	PaidAmount        int64      `json:"paid_amount"`
	Fee               int64      `json:"fee"`
	NetAmount         int64      `json:"net_amount"`
	RefundedAmount    int64      `json:"refunded_amount"`
	ChargebackStatus  string     `json:"chargeback_status,omitempty"`
	CheckoutURL       string     `json:"checkout_url"`
	PaidAt            *time.Time `json:"paid_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DuplicateWebhooks bool       `json:"-"`
	OutOfOrder        bool       `json:"-"`
	HeldEvents        []string   `json:"-"`
}

// Customer é um pagador simulado.
type Customer struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Document          string `json:"document"`
	Email             string `json:"email"`
	ExternalReference string `json:"external_reference"`
}

// Refund é um reembolso simulado.
type Refund struct {
	ID                string    `json:"id"`
	ChargeID          string    `json:"charge_id"`
	Amount            int64     `json:"amount"`
	Status            string    `json:"status"`
	ExternalReference string    `json:"external_reference"`
	CreatedAt         time.Time `json:"created_at"`
}

// Event é um evento de webhook.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
	ChargeID  string          `json:"-"`
	Merchant  string          `json:"-"`
	// Seq é a ordem de criação do evento (sequência numérica do
	// simulador), usada para reenviar os eventos na ordem em que surgiram.
	Seq int64 `json:"-"`
}

// Delivery é uma entrega pendente de webhook.
type Delivery struct {
	EventID       string    `json:"event_id"`
	Merchant      string    `json:"merchant"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastStatus    int       `json:"last_status"`
	Done          bool      `json:"done"`
}

// Merchant é a conta de um integrador, identificada pela chave de API.
type Merchant struct {
	Key           string   `json:"key"`
	WebhookURL    string   `json:"webhook_url"`
	WebhookSecret string   `json:"webhook_secret"`
	Scenarios     []string `json:"scenarios"`
	// RefundScenarios são consumidos pelos próximos pedidos de reembolso.
	RefundScenarios []string `json:"refund_scenarios"`
	// CancelScenarios são consumidos pelos próximos cancelamentos.
	CancelScenarios []string `json:"cancel_scenarios"`
}

type snapshot struct {
	Seq        map[string]int       `json:"seq"`
	Merchants  map[string]*Merchant `json:"merchants"`
	Customers  map[string]*Customer `json:"customers"`
	Charges    map[string]*Charge   `json:"charges"`
	Refunds    map[string]*Refund   `json:"refunds"`
	Events     map[string]*Event    `json:"events"`
	Deliveries []*Delivery          `json:"deliveries"`
	// Campos internos persistidos separadamente (não expostos na API).
	ChargeMerchant map[string]string   `json:"charge_merchant"`
	ChargeFlags    map[string][]string `json:"charge_flags"`
	// ChargeHeld guarda os eventos retidos do cenário out_of_order, ainda
	// não entregues.
	ChargeHeld map[string][]string `json:"charge_held_events"`
	// EventCharge e EventMerchant ligam cada evento à cobrança e ao
	// integrador (o corpo do evento é o do webhook e não os inclui).
	EventCharge   map[string]string `json:"event_charge"`
	EventMerchant map[string]string `json:"event_merchant"`
	EventSeq      map[string]int64  `json:"event_seq"`
}

// State guarda todo o estado do simulador.
type State struct {
	mu   sync.Mutex
	path string
	s    snapshot
}

// ErrNotFound indica recurso inexistente.
var ErrNotFound = errors.New("paysim: não encontrado")

// Load carrega o estado de path (vazio = somente memória).
func Load(path string) (*State, error) {
	st := &State{path: path, s: snapshot{Seq: map[string]int{}, Merchants: map[string]*Merchant{}, Customers: map[string]*Customer{},
		Charges: map[string]*Charge{}, Refunds: map[string]*Refund{}, Events: map[string]*Event{},
		ChargeMerchant: map[string]string{}, ChargeFlags: map[string][]string{}, ChargeHeld: map[string][]string{},
		EventCharge: map[string]string{}, EventMerchant: map[string]string{}, EventSeq: map[string]int64{}}}
	if path == "" {
		return st, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // caminho do arquivo de estado definido pelo operador do ambiente local
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &st.s); err != nil {
		return nil, fmt.Errorf("paysim: estado ilegível: %w", err)
	}
	// Estados gravados antes destes campos voltam sem eles.
	for _, m := range []*map[string]string{&st.s.ChargeMerchant, &st.s.EventCharge, &st.s.EventMerchant} {
		if *m == nil {
			*m = map[string]string{}
		}
	}
	for _, m := range []*map[string][]string{&st.s.ChargeFlags, &st.s.ChargeHeld} {
		if *m == nil {
			*m = map[string][]string{}
		}
	}
	if st.s.EventSeq == nil {
		st.s.EventSeq = map[string]int64{}
	}
	for id, e := range st.s.Events {
		e.ChargeID, e.Merchant = st.s.EventCharge[id], st.s.EventMerchant[id]
		seq, ok := st.s.EventSeq[id]
		if !ok {
			// Estado gravado antes da sequência: o número do id é a ordem.
			seq, _ = strconv.ParseInt(strings.TrimPrefix(id, "evt_"), 10, 64)
		}
		e.Seq = seq
	}
	for id, c := range st.s.Charges {
		c.MerchantKey = st.s.ChargeMerchant[id]
		c.HeldEvents = st.s.ChargeHeld[id]
		for _, f := range st.s.ChargeFlags[id] {
			switch f {
			case "duplicate_webhooks":
				c.DuplicateWebhooks = true
			case "out_of_order":
				c.OutOfOrder = true
			}
		}
	}
	return st, nil
}

// persist grava o estado de forma atômica (arquivo temporário + rename).
// Deve ser chamado com o mutex adquirido.
func (st *State) persist() error {
	if st.path == "" {
		return nil
	}
	for id, c := range st.s.Charges {
		st.s.ChargeMerchant[id] = c.MerchantKey
		var flags []string
		if c.DuplicateWebhooks {
			flags = append(flags, "duplicate_webhooks")
		}
		if c.OutOfOrder {
			flags = append(flags, "out_of_order")
		}
		st.s.ChargeFlags[id] = flags
		if len(c.HeldEvents) > 0 {
			st.s.ChargeHeld[id] = c.HeldEvents
		} else {
			delete(st.s.ChargeHeld, id)
		}
	}
	for id, e := range st.s.Events {
		st.s.EventCharge[id], st.s.EventMerchant[id], st.s.EventSeq[id] = e.ChargeID, e.Merchant, e.Seq
	}
	b, err := json.Marshal(st.s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(st.path), ".paysim-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return os.Rename(tmp.Name(), st.path)
}

func (st *State) nextID(prefix string) string {
	n := st.nextSeq(prefix)
	return fmt.Sprintf("%s_%06d", prefix, n)
}

// nextSeq avança e devolve a sequência numérica do prefixo.
func (st *State) nextSeq(prefix string) int64 {
	st.s.Seq[prefix]++
	return int64(st.s.Seq[prefix])
}

// Fee calcula a taxa determinística do simulador: 2,99% (half-even) + R$ 0,49.
func Fee(amount int64) int64 {
	pct, err := money.New(amount, money.BRL).MulRat(big.NewRat(299, 10000), money.HalfEven)
	if err != nil {
		return 0
	}
	return pct.Amount() + 49
}

func (st *State) merchant(key string) *Merchant {
	m, ok := st.s.Merchants[key]
	if !ok {
		m = &Merchant{Key: key}
		st.s.Merchants[key] = m
	}
	return m
}

// ChargesSorted lista cobranças por criação.
func (st *State) ChargesSorted() []Charge {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Charge, 0, len(st.s.Charges))
	for _, c := range st.s.Charges {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
