package rabbit

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RNF-12: a falha de conexão ao broker, registrada a cada nova tentativa,
// não repete a senha da RECORVIA_AMQP_URL; o amqp091 devolve o erro de
// net/url com a URL inteira quando ela é malformada.
func TestDialErrorsNeverCarryThePassword(t *testing.T) {
	cases := []struct{ url, want string }{
		{"amqp://user:s3gr3d0-amqp@host:port/", "URL inválida"},
		{"amqp://u:s3gr3d0%zz-amqp@h/", "URL inválida"},
		// "/" e "?" sem codificação: o net/url lê como porta o trecho da senha.
		{"amqp://recorvia:s3gr3d0/GhIjK+lm@rabbit:5672/", "URL inválida"},
		{"amqp://recorvia:s3gr3d0?GhIjK@rabbit:5672/", "URL inválida"},
		{"amqp://user:s3gr3d0-amqp@127.0.0.1:1/", "connection refused"},
	}
	for _, tc := range cases {
		_, err := NewConn(tc.url, slog.New(slog.DiscardHandler)).dial()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: erro %v, esperado %q", tc.url, err, tc.want)
		}
		for _, leak := range []string{"s3gr3d0", "%zz", "GhIjK"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: a mensagem %q contém %q", tc.url, err.Error(), leak)
			}
		}
	}
}

// fakeAck registra os acks e nacks das entregas.
type fakeAck struct {
	mu     sync.Mutex
	acks   []uint64
	nacks  []uint64
	nacked chan struct{}
}

func (a *fakeAck) Ack(tag uint64, _ bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.acks = append(a.acks, tag)
	return nil
}

func (a *fakeAck) Nack(tag uint64, _ bool, requeue bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if requeue {
		a.nacks = append(a.nacks, tag)
		a.nacked <- struct{}{}
	}
	return nil
}

func (a *fakeAck) Reject(tag uint64, requeue bool) error { return a.Nack(tag, false, requeue) }

func (a *fakeAck) snapshot() ([]uint64, []uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.acks), slices.Clone(a.nacks)
}

type nopObserver struct{}

func (nopObserver) Processed(string, string, time.Duration) {}
func (nopObserver) InFlight(string, int)                    {}

// RNF-11: no encerramento, a mensagem em processamento termina e é
// confirmada; a entrega que aguardava vaga e as recebidas enquanto o
// cancelamento do consumo é confirmado voltam à fila na hora (nack com
// requeue), sem esperar o fechamento do canal; dispatch só retorna depois da
// mensagem em andamento.
func TestDispatchRequeuesUnprocessedDeliveriesOnShutdown(t *testing.T) {
	ack := &fakeAck{nacked: make(chan struct{}, 8)}
	started, release := make(chan struct{}), make(chan struct{})
	h := func(_ context.Context, m Message) error {
		if m.ID == "m1" {
			close(started)
			<-release
		}
		return nil
	}
	c := NewConsumer(nil, nil, ConsumerSpec{Name: "teste"}, ConsumerConfig{Concurrency: 1}, h, slog.New(slog.DiscardHandler), nopObserver{})
	deliveries := make(chan amqp.Delivery)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.dispatch(ctx, deliveries) }()
	delivery := func(tag uint64, id string) amqp.Delivery {
		return amqp.Delivery{Acknowledger: ack, DeliveryTag: tag, MessageId: id}
	}
	deliveries <- delivery(1, "m1")
	<-started
	deliveries <- delivery(2, "m2") // recebida, aguardando vaga
	cancel()
	deliveries <- delivery(3, "m3") // recebida durante a confirmação do cancelamento
	close(deliveries)
	<-ack.nacked
	<-ack.nacked
	select {
	case err := <-done:
		t.Fatalf("dispatch retornou com a mensagem em processamento: %v", err)
	default:
	}
	if acks, nacks := ack.snapshot(); len(acks) != 0 || !slices.Equal(nacks, []uint64{2, 3}) {
		t.Fatalf("antes da conclusão: acks %v nacks %v", acks, nacks)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if acks, nacks := ack.snapshot(); !slices.Equal(acks, []uint64{1}) || !slices.Equal(nacks, []uint64{2, 3}) {
		t.Fatalf("acks %v nacks %v", acks, nacks)
	}
}

// RNF-07: o aviso de falha de uma mensagem, registrado uma única vez pelo
// consumidor com a decisão tomada, leva message_id, tipo, tenant_id e o
// request_id da requisição de origem; sem broker para agendar a
// retentativa, a mensagem volta à fila.
func TestFailureLogsCarryEventCorrelation(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	conn := NewConn("amqp://indisponivel", log)
	h := func(context.Context, Message) error { return errors.New("provedor indisponível") }
	c := NewConsumer(conn, NewPublisher(conn), ConsumerSpec{Name: "teste"}, ConsumerConfig{}, h, log, nopObserver{})
	ack := &fakeAck{nacked: make(chan struct{}, 1)}
	c.handle(context.Background(), amqp.Delivery{Acknowledger: ack, DeliveryTag: 7, MessageId: "m-7", Type: "invoice.issued",
		Headers: amqp.Table{"tenant_id": "t-1", "request_id": "req-falha-0001"}})
	if _, nacks := ack.snapshot(); !slices.Equal(nacks, []uint64{7}) {
		t.Fatalf("nacks %v", nacks)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 { // retentativa agendada e reencaminhamento falhou
		t.Fatalf("logs %s", out)
	}
	for _, l := range lines {
		for _, want := range []string{`"consumer":"teste"`, `"message_id":"m-7"`, `"event_type":"invoice.issued"`, `"tenant_id":"t-1"`,
			`"request_id":"req-falha-0001"`} {
			if !strings.Contains(l, want) {
				t.Errorf("log sem %s: %s", want, l)
			}
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
