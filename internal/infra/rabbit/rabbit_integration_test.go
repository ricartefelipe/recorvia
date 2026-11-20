//go:build integration

package rabbit_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

type nopObserver struct{}

func (nopObserver) Processed(string, string, time.Duration) {}
func (nopObserver) InFlight(string, int)                    {}

func startBroker(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcrabbit.Run(ctx, "rabbitmq:4.2.9-management-alpine")
	if err != nil {
		t.Fatalf("rabbitmq: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("encerramento do broker: %v", err)
		}
	})
	url, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condição não atingida no prazo")
		}
		<-tick.C
	}
}

// TestConsumerRetriesDeadLettersAndDeduplicates cobre: publisher confirms,
// ack manual, retentativa com espera, DLQ para erro permanente e para
// tentativas esgotadas, e reinício após commit antes do ack (o efeito
// confirmado no banco não é repetido graças ao inbox).
func TestConsumerRetriesDeadLettersAndDeduplicates(t *testing.T) {
	orig := rabbit.RetryDelays
	rabbit.RetryDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
	t.Cleanup(func() { rabbit.RetryDelays = orig })

	url := startBroker(t)
	env := fixture.New(t, 8, time.Now().UTC())
	tn := env.NewTenant(t, "rabbit")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	spec := rabbit.ConsumerSpec{Name: "teste.efeitos", EventTypes: []string{"teste.evento"}, MaxAttempts: 4}
	conn := rabbit.NewConn(url, log)
	ready := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	wg.Go(func() {
		conn.Run(ctx, func(c *amqp.Connection) error {
			if err := rabbit.DeclareTopology(c, []rabbit.ConsumerSpec{spec}); err != nil {
				return err
			}
			once.Do(func() { close(ready) })
			return nil
		})
	})
	<-ready
	// ready fecha dentro de onConnect, antes de Run registrar a conexão para
	// publicação: publicar logo em seguida pode receber ErrNotConnected.
	waitFor(t, 10*time.Second, conn.Connected)
	pub := rabbit.NewPublisher(conn)

	var effects atomic.Int32
	var attemptsFlaky, attemptsCrash atomic.Int32
	handler := func(ctx context.Context, m rabbit.Message) error {
		switch string(m.Body) {
		case `"permanente"`:
			return rabbit.Permanent(errors.New("mensagem inválida"))
		case `"sempre-falha"`:
			return errors.New("falha transitória sem fim")
		case `"instavel"`:
			if attemptsFlaky.Add(1) < 3 {
				return errors.New("falha transitória")
			}
			return nil
		case `"reinicio"`:
			// Efeito confirmado no banco junto do inbox; o "processo" cai
			// antes do ack na primeira entrega.
			id, err := uuid.Parse(m.ID)
			if err != nil {
				return rabbit.Permanent(err)
			}
			if _, err := env.Store.InTenantInbox(ctx, tn.ID, spec.Name, id, func(context.Context) error {
				effects.Add(1)
				return nil
			}); err != nil {
				return err
			}
			if attemptsCrash.Add(1) == 1 {
				return errors.New("queda simulada antes do ack")
			}
			return nil
		}
		return nil
	}
	consumer := rabbit.NewConsumer(conn, pub, spec, rabbit.ConsumerConfig{Prefetch: 4, Concurrency: 2, HandlerTimeout: 5 * time.Second},
		handler, log, nopObserver{})
	wg.Go(func() { consumer.Run(ctx) })

	var dead sync.Map
	dlq := rabbit.NewConsumer(conn, pub, rabbit.DLQConsumerSpec(spec.Name), rabbit.ConsumerConfig{Prefetch: 1, Concurrency: 1},
		func(_ context.Context, m rabbit.Message) error {
			dead.Store(string(m.Body), m.Headers["x-error"])
			return nil
		}, log, nopObserver{})
	wg.Go(func() { dlq.ConsumeQueue(ctx, rabbit.DLQQueue(spec.Name)) })

	for _, body := range []string{`"permanente"`, `"sempre-falha"`, `"instavel"`, `"reinicio"`} {
		msg := rabbit.Message{ID: uuid.NewString(), Type: "teste.evento", TenantID: tn.ID.String(), Body: []byte(body), OccurredAt: time.Now()}
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		if err := pub.Publish(pctx, "", msg, 0); err != nil {
			pcancel()
			t.Fatalf("publicação confirmada falhou: %v", err)
		}
		pcancel()
	}
	waitFor(t, 20*time.Second, func() bool {
		_, p := dead.Load(`"permanente"`)
		_, s := dead.Load(`"sempre-falha"`)
		return p && s && attemptsFlaky.Load() >= 3 && attemptsCrash.Load() >= 2
	})
	if _, ok := dead.Load(`"instavel"`); ok {
		t.Fatal("mensagem recuperável foi para a DLQ")
	}
	if got := effects.Load(); got != 1 {
		t.Fatalf("efeito aplicado %d vezes; o inbox deveria garantir uma única aplicação", got)
	}
	cancel()
	wg.Wait()
}

// Publicar com o broker indisponível falha de forma explícita (a outbox
// mantém o evento pendente).
func TestPublishFailsWhenBrokerIsDown(t *testing.T) {
	conn := rabbit.NewConn("amqp://guest:guest@127.0.0.1:1/", slog.New(slog.NewTextHandler(io.Discard, nil)))
	pub := rabbit.NewPublisher(conn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := pub.Publish(ctx, "", rabbit.Message{ID: uuid.NewString(), Type: "x.y"}, 0)
	if !errors.Is(err, rabbit.ErrNotConnected) {
		t.Fatalf("esperava ErrNotConnected, obtive %v", err)
	}
	if conn.Connected() {
		t.Fatal("conexão reportada sem broker")
	}
}
