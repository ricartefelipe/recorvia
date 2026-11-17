// Package rabbit integra o RabbitMQ: topologia durável, publicação com
// publisher confirms e consumo com ack manual, prefetch e concorrência
// configuráveis, retentativas limitadas com backoff e jitter (filas de espera
// por tentativa) e dead-letter queue.
//
// Garantia: entrega pelo menos uma vez. O ack ocorre somente depois que o
// handler retorna (o handler confirma seus efeitos no banco antes de
// retornar); duplicidades são absorvidas pelo inbox dos consumidores.
package rabbit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exchange é o exchange topic dos eventos de domínio.
const Exchange = "recorvia.events"

// QueueName retorna a fila de um consumidor.
func QueueName(consumer string) string { return "recorvia." + consumer }

// ErrNotConnected indica broker indisponível.
var ErrNotConnected = errors.New("rabbit: sem conexão com o broker")

// Conn mantém a conexão com reconexão automática.
type Conn struct {
	url string
	log *slog.Logger

	mu   sync.RWMutex
	conn *amqp.Connection
}

// NewConn cria o gerenciador de conexão (a conexão é aberta por Run).
func NewConn(url string, log *slog.Logger) *Conn { return &Conn{url: url, log: log} }

// Run mantém a conexão aberta até ctx terminar, reconectando com backoff.
// onConnect é chamado a cada nova conexão (para declarar a topologia).
func (c *Conn) Run(ctx context.Context, onConnect func(*amqp.Connection) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := amqp.DialConfig(c.url, amqp.Config{Heartbeat: 10 * time.Second, Locale: "en_US",
			Properties: amqp.Table{"connection_name": "recorvia"}})
		if err == nil && onConnect != nil {
			if err = onConnect(conn); err != nil {
				err = errors.Join(err, conn.Close())
			}
		}
		if err != nil {
			c.log.Warn("broker indisponível; nova tentativa", slog.String("error", err.Error()), slog.Duration("backoff", backoff))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		c.mu.Lock()
		c.conn = conn
		c.mu.Unlock()
		c.log.Info("conectado ao broker")
		closed := conn.NotifyClose(make(chan *amqp.Error, 1))
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.conn = nil
			c.mu.Unlock()
			if err := conn.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
				c.log.Warn("fechamento da conexão", slog.String("error", err.Error()))
			}
			return
		case e := <-closed:
			c.mu.Lock()
			c.conn = nil
			c.mu.Unlock()
			if e != nil {
				c.log.Warn("conexão com o broker perdida", slog.String("error", e.Error()))
			}
		}
	}
}

// Connected informa se há conexão ativa (readiness do worker).
func (c *Conn) Connected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && !c.conn.IsClosed()
}

func (c *Conn) channel() (*amqp.Channel, error) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || conn.IsClosed() {
		return nil, ErrNotConnected
	}
	return conn.Channel()
}

// ConsumerSpec descreve um consumidor e os eventos que ele assina.
type ConsumerSpec struct {
	Name        string
	EventTypes  []string
	MaxAttempts int
}

// RetryDelays define a espera base por tentativa; o jitter é aplicado sobre ela.
var RetryDelays = []time.Duration{2 * time.Second, 10 * time.Second, 1 * time.Minute, 5 * time.Minute, 15 * time.Minute}

func retryQueue(queue string, attempt int) string { return fmt.Sprintf("%s.retry.%d", queue, attempt) }

func dlqName(queue string) string { return queue + ".dlq" }

// DeclareTopology declara exchange, filas principais, filas de espera por
// tentativa (que devolvem à fila principal ao expirar) e DLQs. É idempotente.
func DeclareTopology(conn *amqp.Connection, specs []ConsumerSpec) (err error) {
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeChannel(ch)) }()
	if err := ch.ExchangeDeclare(Exchange, "topic", true, false, false, false, nil); err != nil {
		return err
	}
	for _, s := range specs {
		q := QueueName(s.Name)
		if _, err := ch.QueueDeclare(q, true, false, false, false, nil); err != nil {
			return fmt.Errorf("rabbit: fila %s: %w", q, err)
		}
		for _, et := range s.EventTypes {
			if err := ch.QueueBind(q, et, Exchange, false, nil); err != nil {
				return err
			}
		}
		for i := 1; i <= len(RetryDelays); i++ {
			args := amqp.Table{"x-dead-letter-exchange": "", "x-dead-letter-routing-key": q}
			if _, err := ch.QueueDeclare(retryQueue(q, i), true, false, false, false, args); err != nil {
				return err
			}
		}
		if _, err := ch.QueueDeclare(dlqName(q), true, false, false, false, nil); err != nil {
			return err
		}
	}
	return nil
}

func closeChannel(ch *amqp.Channel) error {
	if err := ch.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
		return err
	}
	return nil
}

// Message é uma mensagem publicada ou consumida.
type Message struct {
	ID         string
	Type       string
	TenantID   string
	Body       []byte
	Headers    map[string]string
	OccurredAt time.Time
	Attempt    int
}

// Publisher publica com confirmação do broker.
type Publisher struct {
	conn *Conn
	mu   sync.Mutex
	ch   *amqp.Channel
}

// NewPublisher cria o publicador.
func NewPublisher(conn *Conn) *Publisher { return &Publisher{conn: conn} }

func (p *Publisher) channel() (*amqp.Channel, error) {
	if p.ch != nil && !p.ch.IsClosed() {
		return p.ch, nil
	}
	ch, err := p.conn.channel()
	if err != nil {
		return nil, err
	}
	if err := ch.Confirm(false); err != nil {
		return nil, errors.Join(err, closeChannel(ch))
	}
	p.ch = ch
	return ch, nil
}

func toTable(m Message) amqp.Table {
	t := amqp.Table{"tenant_id": m.TenantID, "x-attempt": int32(m.Attempt)}
	for k, v := range m.Headers {
		t[k] = v
	}
	return t
}

// Publish envia ao exchange (routing key = tipo do evento) ou diretamente a
// uma fila (queue != "") e aguarda o publisher confirm. Retorna erro se o
// broker não confirmar (nack, canal fechado ou contexto expirado).
func (p *Publisher) Publish(ctx context.Context, queue string, m Message, expiration time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch, err := p.channel()
	if err != nil {
		return err
	}
	exchange, key := Exchange, m.Type
	if queue != "" {
		exchange, key = "", queue
	}
	pub := amqp.Publishing{
		ContentType: "application/json", DeliveryMode: amqp.Persistent, MessageId: m.ID, Type: m.Type,
		Timestamp: m.OccurredAt, Headers: toTable(m), Body: m.Body,
	}
	if expiration > 0 {
		pub.Expiration = strconv.FormatInt(expiration.Milliseconds(), 10)
	}
	dc, err := ch.PublishWithDeferredConfirmWithContext(ctx, exchange, key, false, false, pub)
	if err != nil {
		p.ch = nil
		return err
	}
	ok, err := dc.WaitContext(ctx)
	if err != nil {
		p.ch = nil
		return err
	}
	if !ok {
		return errors.New("rabbit: publicação recusada pelo broker (nack)")
	}
	return nil
}

// PermanentError marca falhas que não devem ser retentadas (mensagem
// inválida, regra de negócio definitiva): vão direto para a DLQ.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanente: " + e.Err.Error() }

// Unwrap expõe a causa.
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent embrulha um erro como permanente.
func Permanent(err error) error { return &PermanentError{Err: err} }

// Handler processa uma mensagem. Retornar nil confirma (ack).
type Handler func(ctx context.Context, m Message) error

// Observer recebe sinais para métricas.
type Observer interface {
	Processed(consumer, outcome string, d time.Duration)
	InFlight(consumer string, delta int)
}

// ConsumerConfig parametriza o consumo.
type ConsumerConfig struct {
	Prefetch       int
	Concurrency    int
	HandlerTimeout time.Duration
}

// Consumer consome uma fila com ack manual.
type Consumer struct {
	conn     *Conn
	pub      *Publisher
	spec     ConsumerSpec
	cfg      ConsumerConfig
	handler  Handler
	log      *slog.Logger
	observer Observer
}

// NewConsumer cria um consumidor.
func NewConsumer(conn *Conn, pub *Publisher, spec ConsumerSpec, cfg ConsumerConfig, h Handler, log *slog.Logger, obs Observer) *Consumer {
	if spec.MaxAttempts <= 0 || spec.MaxAttempts > len(RetryDelays)+1 {
		spec.MaxAttempts = len(RetryDelays) + 1
	}
	cfg.Concurrency = max(cfg.Concurrency, 1)
	cfg.Prefetch = max(cfg.Prefetch, cfg.Concurrency)
	if cfg.HandlerTimeout <= 0 {
		cfg.HandlerTimeout = time.Minute
	}
	return &Consumer{conn: conn, pub: pub, spec: spec, cfg: cfg, handler: h, log: log.With("consumer", spec.Name), observer: obs}
}

// Run consome até ctx terminar, reabrindo o canal após perda de conexão.
// No encerramento, para de receber e aguarda as mensagens em andamento.
func (c *Consumer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := c.consume(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Warn("consumo interrompido; nova tentativa", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Consumer) consume(ctx context.Context) (err error) {
	ch, err := c.conn.channel()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeChannel(ch)) }()
	if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return err
	}
	tag := c.spec.Name + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	deliveries, err := ch.ConsumeWithContext(ctx, QueueName(c.spec.Name), tag, false, false, false, false, nil)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, c.cfg.Concurrency)
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, ok := <-deliveries:
			if !ok {
				return errors.New("rabbit: canal de entrega encerrado")
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			wg.Go(func() {
				defer func() { <-sem }()
				c.handle(ctx, d)
			})
		}
	}
}

func fromDelivery(d amqp.Delivery) Message {
	m := Message{ID: d.MessageId, Type: d.Type, Body: d.Body, OccurredAt: d.Timestamp, Headers: map[string]string{}}
	for k, v := range d.Headers {
		switch val := v.(type) {
		case string:
			m.Headers[k] = val
		case int32:
			if k == "x-attempt" {
				m.Attempt = int(val)
			}
		case int64:
			if k == "x-attempt" {
				m.Attempt = int(val)
			}
		}
	}
	m.TenantID = m.Headers["tenant_id"]
	if orig := m.Headers["recorvia-original-message-id"]; orig != "" {
		m.ID = orig // reentrega direcionada preserva o id original (inbox)
	}
	return m
}

func jitter(d time.Duration) time.Duration {
	// ±20% para espalhar retentativas simultâneas.
	f := 0.8 + rand.Float64()*0.4 //nolint:gosec // jitter não exige aleatoriedade criptográfica
	return time.Duration(float64(d) * f)
}

func (c *Consumer) handle(parent context.Context, d amqp.Delivery) {
	c.observer.InFlight(c.spec.Name, 1)
	defer c.observer.InFlight(c.spec.Name, -1)
	start := time.Now()
	m := fromDelivery(d)
	// O processamento não é cancelado pelo encerramento do worker: a
	// mensagem em andamento termina dentro do timeout do handler.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), c.cfg.HandlerTimeout)
	defer cancel()
	err := c.handler(ctx, m)
	outcome := "ack"
	switch {
	case err == nil:
	case errors.As(err, new(*PermanentError)):
		outcome = "dead_letter"
		err = c.toDLQ(ctx, m, err)
	default:
		next := m.Attempt + 1
		if next >= c.spec.MaxAttempts {
			outcome = "dead_letter"
			err = c.toDLQ(ctx, m, err)
		} else {
			outcome = "retry"
			c.log.Warn("falha no processamento; retentativa agendada", slog.String("message_id", m.ID), slog.Int("attempt", next),
				slog.String("error", err.Error()))
			m.Attempt = next
			delay := jitter(RetryDelays[min(next, len(RetryDelays))-1])
			err = c.pub.Publish(ctx, retryQueue(QueueName(c.spec.Name), min(next, len(RetryDelays))), m, delay)
		}
	}
	if err != nil {
		// Não foi possível agendar retentativa nem DLQ: devolve à fila
		// (nack com requeue) para não perder a mensagem.
		outcome = "requeue"
		c.log.Error("reencaminhamento falhou; mensagem devolvida", slog.String("message_id", m.ID), slog.String("error", err.Error()))
		if nerr := d.Nack(false, true); nerr != nil {
			c.log.Error("nack falhou", slog.String("error", nerr.Error()))
		}
	} else if aerr := d.Ack(false); aerr != nil {
		c.log.Error("ack falhou; a mensagem será reentregue e deduplicada pelo inbox", slog.String("error", aerr.Error()))
	}
	c.observer.Processed(c.spec.Name, outcome, time.Since(start))
}

func (c *Consumer) toDLQ(ctx context.Context, m Message, cause error) error {
	c.log.Error("mensagem enviada à DLQ", slog.String("message_id", m.ID), slog.Int("attempt", m.Attempt), slog.String("error", cause.Error()))
	if m.Headers == nil {
		m.Headers = map[string]string{}
	}
	m.Headers["x-error"] = truncate(cause.Error(), 1000)
	m.Headers["x-consumer"] = c.spec.Name
	return c.pub.Publish(ctx, dlqName(QueueName(c.spec.Name)), m, 0)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DLQConsumerSpec cria o consumo da DLQ de um consumidor (registro em banco).
func DLQConsumerSpec(consumer string) ConsumerSpec {
	return ConsumerSpec{Name: consumer + ".dlq-recorder", MaxAttempts: len(RetryDelays) + 1}
}

// DLQQueue expõe o nome da DLQ de um consumidor.
func DLQQueue(consumer string) string { return dlqName(QueueName(consumer)) }

// ConsumeQueue consome uma fila arbitrária (usado para registrar a DLQ no
// banco). Mesma semântica de ack manual.
func (c *Consumer) ConsumeQueue(ctx context.Context, queue string) {
	for ctx.Err() == nil {
		err := func() (err error) {
			ch, err := c.conn.channel()
			if err != nil {
				return err
			}
			defer func() { err = errors.Join(err, closeChannel(ch)) }()
			if err := ch.Qos(c.cfg.Prefetch, 0, false); err != nil {
				return err
			}
			deliveries, err := ch.ConsumeWithContext(ctx, queue, "", false, false, false, false, nil)
			if err != nil {
				return err
			}
			for d := range deliveries {
				m := fromDelivery(d)
				hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.HandlerTimeout)
				herr := c.handler(hctx, m)
				cancel()
				if herr != nil {
					c.log.Error("registro de DLQ falhou", slog.String("error", herr.Error()))
					// Espera curta antes de devolver, evitando laço quente com o banco indisponível.
					select {
					case <-ctx.Done():
					case <-time.After(time.Second):
					}
					if nerr := d.Nack(false, true); nerr != nil {
						return nerr
					}
					continue
				}
				if aerr := d.Ack(false); aerr != nil {
					return aerr
				}
			}
			return nil
		}()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Warn("consumo da DLQ interrompido", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
