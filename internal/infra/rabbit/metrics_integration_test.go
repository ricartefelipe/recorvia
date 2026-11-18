//go:build integration

package rabbit_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	tcrabbit "github.com/testcontainers/testcontainers-go/modules/rabbitmq"

	"github.com/ricartefelipe/recorvia/internal/infra/rabbit"
)

// sampleLine reconhece uma amostra do formato texto do Prometheus:
// nome{rótulos} valor.
var sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})?\s+(\S+)`)

var labelPair = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)

type sample struct {
	labels map[string]string
	value  float64
}

// scrape coleta o endpoint e agrupa as amostras por nome de métrica.
func scrape(t *testing.T, url string) map[string][]sample {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("coleta de %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("coleta de %s: %d %s", url, resp.StatusCode, b)
	}
	out := map[string][]sample{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		labels := map[string]string{}
		for _, lp := range labelPair.FindAllStringSubmatch(m[2], -1) {
			labels[lp[1]] = lp[2]
		}
		out[m[1]] = append(out[m[1]], sample{labels: labels, value: v})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func queueValue(samples map[string][]sample, metric, queue string) (float64, bool) {
	for _, s := range samples[metric] {
		if s.labels["queue"] == queue {
			return s.value, true
		}
	}
	return 0, false
}

// As regras de alerta de filas (deploy/observability/prometheus/rules)
// dependem de séries do plugin rabbitmq_prometheus no endpoint
// /metrics/detailed com as famílias coletadas pelo Prometheus do perfil
// observability. O teste publica mensagens com o publicador real (que
// preenche o carimbo de tempo com o instante do evento) numa fila sem
// consumidor e confere nomes, rótulos e valores das séries usadas pelos
// alertas de profundidade, ausência de consumidor, DLQ e atraso.
func TestPrometheusPluginExposesQueueSeriesUsedByAlerts(t *testing.T) {
	ctx := context.Background()
	ctr, err := tcrabbit.Run(ctx, "rabbitmq:4.2.9-management-alpine", testcontainers.WithExposedPorts("15692/tcp"))
	if err != nil {
		t.Fatalf("rabbitmq: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("encerramento do broker: %v", err)
		}
	})
	amqpURL, err := ctr.AmqpURL(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(ctx, "15692/tcp")
	if err != nil {
		t.Fatal(err)
	}
	metricsURL := fmt.Sprintf("http://%s:%s/metrics/detailed?family=queue_coarse_metrics&family=queue_consumer_count&family=queue_metrics",
		host, port.Port())

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runCtx, cancel := context.WithCancel(ctx)
	spec := rabbit.ConsumerSpec{Name: "teste.metricas", EventTypes: []string{"teste.metricas"}, MaxAttempts: 2}
	conn := rabbit.NewConn(amqpURL, log)
	ready := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Go(func() {
		conn.Run(runCtx, func(c *amqp.Connection) error {
			if err := rabbit.DeclareTopology(c, []rabbit.ConsumerSpec{spec}); err != nil {
				return err
			}
			once.Do(func() { close(ready) })
			return nil
		})
	})
	<-ready
	pub := rabbit.NewPublisher(conn)
	occurred := time.Now().Add(-10 * time.Minute).Truncate(time.Second).UTC()
	const published = 3
	for range published {
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		err := pub.Publish(pctx, "", rabbit.Message{ID: uuid.NewString(), Type: "teste.metricas", TenantID: uuid.NewString(),
			Body: []byte(`{}`), OccurredAt: occurred}, 0)
		pcancel()
		if err != nil {
			t.Fatal(err)
		}
	}

	queue := rabbit.QueueName(spec.Name)
	var got map[string][]sample
	// As estatísticas das filas são emitidas periodicamente pelo broker.
	waitFor(t, 60*time.Second, func() bool {
		got = scrape(t, metricsURL)
		v, ok := queueValue(got, "rabbitmq_detailed_queue_messages_ready", queue)
		return ok && v == published
	})
	cases := []struct {
		metric, queue string
		want          float64
	}{
		{"rabbitmq_detailed_queue_messages_ready", queue, published},
		{"rabbitmq_detailed_queue_messages", queue, published},
		{"rabbitmq_detailed_queue_messages_unacked", queue, 0},
		{"rabbitmq_detailed_queue_consumers", queue, 0},
		// Carimbo AMQP em segundos: o atraso da fila é time() menos este valor.
		{"rabbitmq_detailed_queue_head_message_timestamp", queue, float64(occurred.Unix())},
		{"rabbitmq_detailed_queue_messages", queue + ".dlq", 0},
		{"rabbitmq_detailed_queue_messages_ready", queue + ".retry.1", 0},
	}
	for _, tc := range cases {
		v, ok := queueValue(got, tc.metric, tc.queue)
		if !ok {
			t.Errorf("série %s{queue=%q} ausente", tc.metric, tc.queue)
			continue
		}
		if v != tc.want {
			t.Errorf("%s{queue=%q} = %v, esperado %v", tc.metric, tc.queue, v, tc.want)
		}
	}
	// As séries por fila só identificam vhost, fila e tipo de fila (sem dados
	// de tenant).
	allowed := map[string]bool{"vhost": true, "queue": true, "queue_type": true, "membership": true}
	for name, samples := range got {
		if !strings.HasPrefix(name, "rabbitmq_detailed_queue_") {
			continue
		}
		for _, s := range samples {
			for k := range s.labels {
				if !allowed[k] {
					t.Errorf("%s com rótulo inesperado %s", name, k)
				}
			}
		}
	}
}
