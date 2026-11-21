package paysim

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Cenários de criação de cobrança.
var Scenarios = []string{"decline", "unavailable", "timeout_after_success", "error_after_success", "slow", "duplicate_webhooks", "out_of_order"}

// RefundScenarios são cenários aplicados ao próximo pedido de reembolso:
// refund_unavailable recusa sem processar (503); refund_error_after_success
// efetiva o reembolso e responde com erro (502), como resposta perdida.
var RefundScenarios = []string{"refund_unavailable", "refund_error_after_success"}

// CancelScenarios são cenários aplicados ao próximo cancelamento:
// cancel_unavailable recusa sem processar (503), como provedor indisponível.
var CancelScenarios = []string{"cancel_unavailable"}

// Sim é o simulador.
type Sim struct {
	st        *State
	publicURL string
	now       func() time.Time
	log       *slog.Logger
	http      *http.Client
	// TimeoutDelay é a demora aplicada no cenário timeout_after_success.
	TimeoutDelay time.Duration
	// SlowDelay é a demora do cenário slow.
	SlowDelay time.Duration
}

// New cria o simulador.
func New(st *State, publicURL string, now func() time.Time, log *slog.Logger) *Sim {
	return &Sim{st: st, publicURL: strings.TrimRight(publicURL, "/"), now: now, log: log,
		http:         &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		TimeoutDelay: 30 * time.Second, SlowDelay: 2 * time.Second}
}

// ErrDeclined e ErrUnavailable representam cenários de falha na criação.
var (
	ErrDeclined    = errors.New("cobrança recusada")
	ErrUnavailable = errors.New("provedor indisponível")
)

// ConfigureWebhook define o destino e o segredo dos webhooks do comerciante.
func (s *Sim) ConfigureWebhook(key, url, secret string) error {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("url inválida")
	}
	if len(secret) < 16 {
		return fmt.Errorf("segredo muito curto")
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	m := s.st.merchant(key)
	m.WebhookURL, m.WebhookSecret = url, secret
	return s.st.persist()
}

// EnqueueScenario agenda um cenário para a próxima criação de cobrança ou,
// para cenários de reembolso e de cancelamento, para o próximo pedido desse
// tipo.
func (s *Sim) EnqueueScenario(key, scenario string) error {
	refund, cancel := slices.Contains(RefundScenarios, scenario), slices.Contains(CancelScenarios, scenario)
	if !slices.Contains(Scenarios, scenario) && !refund && !cancel {
		return fmt.Errorf("cenário desconhecido: %s", scenario)
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	m := s.st.merchant(key)
	switch {
	case refund:
		m.RefundScenarios = append(m.RefundScenarios, scenario)
	case cancel:
		m.CancelScenarios = append(m.CancelScenarios, scenario)
	default:
		m.Scenarios = append(m.Scenarios, scenario)
	}
	return s.st.persist()
}

// ErrCancelUnavailable reproduz o cenário cancel_unavailable.
var ErrCancelUnavailable = errors.New("cancelamento temporariamente indisponível")

// ErrNotCancelable indica cobrança já paga, reembolsada ou contestada.
var ErrNotCancelable = errors.New("cobrança não pode ser cancelada")

// Cancel cancela a cobrança pendente ou recusada (idempotente para cobrança
// já cancelada) e notifica o comerciante.
func (s *Sim) Cancel(key, id string) (ChargeView, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	c, ok := s.st.s.Charges[id]
	if !ok || c.MerchantKey != key {
		return ChargeView{}, ErrNotFound
	}
	if m := s.st.merchant(key); len(m.CancelScenarios) > 0 {
		m.CancelScenarios = m.CancelScenarios[1:]
		return ChargeView{}, errors.Join(ErrCancelUnavailable, s.st.persist())
	}
	switch c.Status {
	case "canceled":
		return s.viewLocked(c), nil
	case "pending", "failed":
	default:
		return s.viewLocked(c), ErrNotCancelable
	}
	c.Status, c.UpdatedAt = "canceled", s.now()
	s.emit(c, "charge.canceled")
	return s.viewLocked(c), s.st.persist()
}

// ChargeView é a cobrança com os reembolsos, como exposta na API e nos
// webhooks.
type ChargeView struct {
	Charge
	Refunds []Refund `json:"refunds"`
}

// viewLocked monta a visão da cobrança. Deve ser chamado com o mutex.
func (s *Sim) viewLocked(c *Charge) ChargeView {
	v := ChargeView{Charge: *c, Refunds: []Refund{}}
	for _, r := range s.st.s.Refunds {
		if r.ChargeID == c.ID {
			v.Refunds = append(v.Refunds, *r)
		}
	}
	slices.SortFunc(v.Refunds, func(a, b Refund) int { return strings.Compare(a.ID, b.ID) })
	return v
}

// View retorna a visão de uma cobrança do comerciante.
func (s *Sim) View(key, id string) (ChargeView, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	c, ok := s.st.s.Charges[id]
	if !ok || (key != "" && c.MerchantKey != key) {
		return ChargeView{}, ErrNotFound
	}
	return s.viewLocked(c), nil
}

// CreateCustomer registra um pagador (idempotente por referência externa).
func (s *Sim) CreateCustomer(key string, c Customer) (Customer, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	s.st.merchant(key)
	if c.ExternalReference != "" {
		for _, existing := range s.st.s.Customers {
			if existing.ExternalReference == c.ExternalReference {
				return *existing, nil
			}
		}
	}
	c.ID = s.st.nextID("cus")
	s.st.s.Customers[c.ID] = &c
	return c, s.st.persist()
}

// CreateChargeInput é o pedido de criação.
type CreateChargeInput struct {
	CustomerID        string `json:"customer_id"`
	Amount            int64  `json:"amount"`
	Currency          string `json:"currency"`
	DueDate           string `json:"due_date"`
	Description       string `json:"description"`
	ExternalReference string `json:"external_reference"`
}

// CreateCharge cria a cobrança aplicando o próximo cenário do comerciante.
// Retorna o cenário aplicado para que o servidor reproduza o efeito na resposta.
func (s *Sim) CreateCharge(key string, in CreateChargeInput) (Charge, string, error) {
	if in.Amount <= 0 || in.Currency != "BRL" || in.ExternalReference == "" {
		return Charge{}, "", fmt.Errorf("pedido inválido")
	}
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	m := s.st.merchant(key)
	scenario := ""
	if len(m.Scenarios) > 0 {
		scenario, m.Scenarios = m.Scenarios[0], m.Scenarios[1:]
	}
	switch scenario {
	case "decline":
		return Charge{}, scenario, errors.Join(ErrDeclined, s.st.persist())
	case "unavailable":
		return Charge{}, scenario, errors.Join(ErrUnavailable, s.st.persist())
	}
	now := s.now()
	c := &Charge{ID: s.st.nextID("ch"), MerchantKey: key, CustomerID: in.CustomerID, ExternalReference: in.ExternalReference,
		Description: in.Description, Amount: in.Amount, Currency: in.Currency, DueDate: in.DueDate, Status: "pending",
		CreatedAt: now, UpdatedAt: now, DuplicateWebhooks: scenario == "duplicate_webhooks", OutOfOrder: scenario == "out_of_order"}
	c.CheckoutURL = s.publicURL + "/checkout/" + c.ID
	s.st.s.Charges[c.ID] = c
	s.emit(c, "charge.created")
	return *c, scenario, s.st.persist()
}

// Charge retorna uma cobrança do comerciante.
func (s *Sim) Charge(key, id string) (Charge, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	c, ok := s.st.s.Charges[id]
	if !ok || (key != "" && c.MerchantKey != key) {
		return Charge{}, ErrNotFound
	}
	return *c, nil
}

// ListCharges filtra por referência externa ou janela de atualização.
func (s *Sim) ListCharges(key, externalRef string, from, to time.Time, offset, limit int) ([]ChargeView, bool) {
	all := s.st.ChargesSorted()
	var out []ChargeView
	for _, c := range all {
		if c.MerchantKey != key {
			continue
		}
		if externalRef != "" && c.ExternalReference != externalRef {
			continue
		}
		if !from.IsZero() && c.UpdatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && !c.UpdatedAt.Before(to) {
			continue
		}
		v, err := s.View(key, c.ID)
		if err != nil {
			continue
		}
		out = append(out, v)
	}
	if offset >= len(out) {
		return nil, false
	}
	end := min(offset+limit, len(out))
	return out[offset:end], end < len(out)
}

// emit cria o evento e agenda a entrega. Deve ser chamado com o mutex.
func (s *Sim) emit(c *Charge, eventType string) {
	data, err := json.Marshal(map[string]any{"charge": s.viewLocked(c)})
	if err != nil {
		s.log.Error("serialização do evento falhou", slog.String("error", err.Error()))
		return
	}
	seq := s.st.nextSeq("evt")
	ev := &Event{ID: fmt.Sprintf("evt_%06d", seq), Type: eventType, CreatedAt: s.now(), Data: data, ChargeID: c.ID, Merchant: c.MerchantKey, Seq: seq}
	s.st.s.Events[ev.ID] = ev
	if c.OutOfOrder && eventType == "charge.created" {
		c.HeldEvents = append(c.HeldEvents, ev.ID)
		return
	}
	s.enqueue(ev)
	if c.OutOfOrder && len(c.HeldEvents) > 0 {
		// O evento novo segue antes dos retidos: entrega fora de ordem.
		for _, held := range c.HeldEvents {
			if e, ok := s.st.s.Events[held]; ok {
				s.enqueue(e)
			}
		}
		c.HeldEvents = nil
	}
	if c.DuplicateWebhooks {
		s.enqueue(ev)
	}
}

func (s *Sim) enqueue(ev *Event) {
	s.st.s.Deliveries = append(s.st.s.Deliveries, &Delivery{EventID: ev.ID, Merchant: ev.Merchant, NextAttemptAt: s.now()})
}

// Action aplica uma ação de simulação sobre a cobrança.
type Action struct {
	Name   string `json:"action"`
	Amount int64  `json:"amount"`
	Fee    *int64 `json:"fee"`
	Stage  string `json:"stage"`
}

// Apply executa a ação: pay, pay_silently, fail, refund, chargeback,
// diverge e replay. Uma cobrança recusada (fail) ainda pode ser paga, como no
// pagamento por outro meio depois da recusa do cartão no mesmo checkout.
func (s *Sim) Apply(id string, a Action) (Charge, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	c, ok := s.st.s.Charges[id]
	if !ok {
		return Charge{}, ErrNotFound
	}
	now := s.now()
	switch a.Name {
	case "pay", "pay_silently":
		if c.Status != "pending" && c.Status != "failed" {
			return *c, fmt.Errorf("cobrança não está pendente nem recusada")
		}
		paid := c.Amount
		if a.Amount > 0 {
			paid = a.Amount
		}
		fee := Fee(paid)
		if a.Fee != nil {
			fee = *a.Fee
		}
		c.Status, c.PaidAmount, c.Fee, c.NetAmount, c.PaidAt, c.UpdatedAt = "paid", paid, fee, paid-fee, &now, now
		if a.Name == "pay" {
			s.emit(c, "charge.paid")
		}
	case "fail":
		if c.Status != "pending" {
			return *c, fmt.Errorf("cobrança não está pendente")
		}
		c.Status, c.UpdatedAt = "failed", now
		s.emit(c, "charge.failed")
	case "refund":
		if err := s.refundLocked(c, a.Amount, ""); err != nil {
			return *c, err
		}
	case "chargeback":
		switch a.Stage {
		case "requested":
			if c.Status != "paid" && c.Status != "partially_refunded" {
				return *c, fmt.Errorf("cobrança não está paga")
			}
			c.Status, c.ChargebackStatus, c.UpdatedAt = "chargeback_requested", "requested", now
			s.emit(c, "charge.chargeback_requested")
		case "won":
			if c.ChargebackStatus != "requested" {
				return *c, fmt.Errorf("não há chargeback aberto")
			}
			c.Status, c.ChargebackStatus, c.UpdatedAt = "paid", "won", now
			s.emit(c, "charge.chargeback_won")
		case "lost":
			if c.ChargebackStatus != "requested" {
				return *c, fmt.Errorf("não há chargeback aberto")
			}
			c.Status, c.ChargebackStatus, c.UpdatedAt = "chargeback_lost", "lost", now
			s.emit(c, "charge.chargeback_lost")
		default:
			return *c, fmt.Errorf("estágio inválido")
		}
	case "diverge":
		// Altera os valores do provedor sem notificar: a conciliação deve
		// apontar a divergência.
		if a.Amount > 0 {
			c.PaidAmount = a.Amount
		}
		if a.Fee != nil {
			c.Fee = *a.Fee
		}
		c.NetAmount, c.UpdatedAt = c.PaidAmount-c.Fee, now
	case "replay":
		// Ordem de criação dos eventos (sequência numérica, não o relógio nem
		// o texto do id), para que a mesma sequência de comandos produza as
		// mesmas entregas.
		var evs []*Event
		for _, e := range s.st.s.Events {
			if e.ChargeID == c.ID {
				evs = append(evs, e)
			}
		}
		slices.SortFunc(evs, func(a, b *Event) int { return cmp.Compare(a.Seq, b.Seq) })
		for _, e := range evs {
			s.enqueue(e)
		}
	default:
		return *c, fmt.Errorf("ação desconhecida")
	}
	return *c, s.st.persist()
}

func (s *Sim) refundLocked(c *Charge, amount int64, extRef string) error {
	if c.Status != "paid" && c.Status != "partially_refunded" {
		return fmt.Errorf("cobrança não reembolsável")
	}
	if amount <= 0 {
		amount = c.PaidAmount - c.RefundedAmount
	}
	if amount > c.PaidAmount-c.RefundedAmount {
		return fmt.Errorf("reembolso excede o saldo")
	}
	now := s.now()
	c.RefundedAmount += amount
	c.UpdatedAt = now
	r := &Refund{ID: s.st.nextID("rf"), ChargeID: c.ID, Amount: amount, Status: "succeeded", ExternalReference: extRef, CreatedAt: now}
	s.st.s.Refunds[r.ID] = r
	if c.RefundedAmount == c.PaidAmount {
		c.Status = "refunded"
		s.emit(c, "charge.refunded")
	} else {
		c.Status = "partially_refunded"
		s.emit(c, "charge.partially_refunded")
	}
	return nil
}

// RefundInput é o pedido de reembolso.
type RefundInput struct {
	Amount            int64  `json:"amount"`
	Description       string `json:"description"`
	ExternalReference string `json:"external_reference"`
}

// ErrRefundUnavailable e ErrRefundResponseLost reproduzem os cenários de
// reembolso: recusa sem processar e efetivação com resposta perdida.
var (
	ErrRefundUnavailable  = errors.New("reembolso temporariamente indisponível")
	ErrRefundResponseLost = errors.New("reembolso efetivado com resposta perdida")
)

// Refund reembolsa (idempotente por referência externa do reembolso).
func (s *Sim) Refund(key, chargeID string, in RefundInput) (Refund, error) {
	s.st.mu.Lock()
	defer s.st.mu.Unlock()
	c, ok := s.st.s.Charges[chargeID]
	if !ok || c.MerchantKey != key {
		return Refund{}, ErrNotFound
	}
	if in.ExternalReference != "" {
		for _, r := range s.st.s.Refunds {
			if r.ChargeID == chargeID && r.ExternalReference == in.ExternalReference {
				return *r, nil
			}
		}
	}
	m := s.st.merchant(key)
	scenario := ""
	if len(m.RefundScenarios) > 0 {
		scenario, m.RefundScenarios = m.RefundScenarios[0], m.RefundScenarios[1:]
	}
	if scenario == "refund_unavailable" {
		return Refund{}, errors.Join(ErrRefundUnavailable, s.st.persist())
	}
	if err := s.refundLocked(c, in.Amount, in.ExternalReference); err != nil {
		return Refund{}, errors.Join(err, s.st.persist())
	}
	if scenario == "refund_error_after_success" {
		return Refund{}, errors.Join(ErrRefundResponseLost, s.st.persist())
	}
	var last *Refund
	for _, r := range s.st.s.Refunds {
		if r.ChargeID == chargeID && (last == nil || r.ID > last.ID) {
			last = r
		}
	}
	return *last, s.st.persist()
}

// Sign calcula a assinatura de um webhook: HMAC-SHA256 de "<t>.<corpo>".
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// SignatureHeader é o header de assinatura dos webhooks do simulador.
const SignatureHeader = "Paysim-Signature"

// DispatchDue envia as entregas vencidas. Retorna quantas foram tentadas.
func (s *Sim) DispatchDue(ctx context.Context) int {
	type job struct {
		d      *Delivery
		ev     Event
		url    string
		secret string
	}
	s.st.mu.Lock()
	now := s.now()
	var jobs []job
	for _, d := range s.st.s.Deliveries {
		if d.Done || d.NextAttemptAt.After(now) || d.Attempts >= 10 {
			continue
		}
		m := s.st.s.Merchants[d.Merchant]
		ev := s.st.s.Events[d.EventID]
		if m == nil || ev == nil || m.WebhookURL == "" {
			continue
		}
		jobs = append(jobs, job{d: d, ev: *ev, url: m.WebhookURL, secret: m.WebhookSecret})
	}
	s.st.mu.Unlock()

	for _, j := range jobs {
		status := s.deliver(ctx, j.url, j.secret, j.ev)
		s.st.mu.Lock()
		j.d.Attempts++
		j.d.LastStatus = status
		if status >= 200 && status < 300 {
			j.d.Done = true
		} else {
			backoff := time.Duration(1<<min(j.d.Attempts, 8)) * time.Second
			j.d.NextAttemptAt = s.now().Add(backoff)
		}
		if err := s.st.persist(); err != nil {
			s.log.Error("persistência falhou", slog.String("error", err.Error()))
		}
		s.st.mu.Unlock()
	}
	// Compacta entregas concluídas.
	s.st.mu.Lock()
	kept := s.st.s.Deliveries[:0]
	for _, d := range s.st.s.Deliveries {
		if !d.Done {
			kept = append(kept, d)
		}
	}
	s.st.s.Deliveries = kept
	s.st.mu.Unlock()
	return len(jobs)
}

func (s *Sim) deliver(ctx context.Context, url, secret string, ev Event) int {
	body, err := json.Marshal(ev)
	if err != nil {
		return 0
	}
	t := s.now().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, fmt.Sprintf("t=%d,v1=%s", t, Sign(secret, t, body)))
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("entrega de webhook falhou", slog.String("event", ev.ID), slog.String("error", err.Error()))
		return 0
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)); err != nil {
		s.log.Warn("leitura da resposta", slog.String("error", err.Error()))
	}
	if err := resp.Body.Close(); err != nil {
		s.log.Warn("fechamento da resposta", slog.String("error", err.Error()))
	}
	s.log.Info("webhook entregue", slog.String("event", ev.ID), slog.String("type", ev.Type), slog.Int("status", resp.StatusCode))
	return resp.StatusCode
}
