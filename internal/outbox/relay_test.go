package outbox_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/outbox"
)

type relayClaim struct {
	worker string
	batch  int
	lease  time.Duration
}

type release struct {
	id     uuid.UUID
	delay  time.Duration
	cause  string
	ctxErr error
}

type mark struct {
	ids    []uuid.UUID
	ctxErr error
}

// fakeRelayStore simula a tabela outbox_events com lease: cada chamada a
// Claim devolve o próximo lote enfileirado.
type fakeRelayStore struct {
	mu       sync.Mutex
	batches  [][]outbox.Record
	claimErr error
	claims   []relayClaim
	marks    []mark
	markErr  error
	releases []release
	relErr   error
	// claimed recebe um sinal a cada reivindicação (opcional).
	claimed chan struct{}
}

func (s *fakeRelayStore) Claim(_ context.Context, worker string, batch int, lease time.Duration) ([]outbox.Record, error) {
	s.mu.Lock()
	s.claims = append(s.claims, relayClaim{worker, batch, lease})
	var out []outbox.Record
	if len(s.batches) > 0 {
		out, s.batches = s.batches[0], s.batches[1:]
	}
	err := s.claimErr
	ch := s.claimed
	s.mu.Unlock()
	if ch != nil {
		ch <- struct{}{}
	}
	return out, err
}

func (s *fakeRelayStore) MarkPublished(ctx context.Context, _ string, ids []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marks = append(s.marks, mark{append([]uuid.UUID(nil), ids...), ctx.Err()})
	if s.markErr != nil {
		return 0, s.markErr
	}
	return len(ids), nil
}

func (s *fakeRelayStore) Release(ctx context.Context, _ string, id uuid.UUID, delay time.Duration, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, release{id, delay, cause, ctx.Err()})
	return s.relErr
}

// fakePublisher confirma todos os eventos, exceto os marcados em fail.
type fakePublisher struct {
	mu        sync.Mutex
	fail      map[uuid.UUID]error
	published []uuid.UUID
	deadlines []time.Duration
	onPublish func()
}

func (p *fakePublisher) PublishEvent(ctx context.Context, r outbox.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, time.Until(dl))
	}
	if p.onPublish != nil {
		p.onPublish()
	}
	if err := p.fail[r.ID]; err != nil {
		return err
	}
	p.published = append(p.published, r.ID)
	return nil
}

type fakeRelayObserver struct {
	mu        sync.Mutex
	published []int
	failed    int
}

func (o *fakeRelayObserver) Published(n int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.published = append(o.published, n)
}

func (o *fakeRelayObserver) PublishFailed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.failed++
}

func records(attempts ...int) []outbox.Record {
	out := make([]outbox.Record, len(attempts))
	for i, a := range attempts {
		out[i] = outbox.Record{ID: uuid.New(), TenantID: uuid.New(), Type: outbox.EventInvoiceIssued, Payload: []byte(`{}`), Attempts: a}
	}
	return out
}

func ids(rs []outbox.Record) []uuid.UUID {
	out := make([]uuid.UUID, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func newRelay(store outbox.RelayStore, pub outbox.Publisher, obs outbox.RelayObserver, batch int) *outbox.Relay {
	return outbox.NewRelay(store, pub, "relay-1", batch, time.Minute, time.Second, slog.New(slog.DiscardHandler), obs)
}

func TestRelayRunOncePublishesAndMarks(t *testing.T) {
	batch := records(0, 0, 0)
	store := &fakeRelayStore{batches: [][]outbox.Record{batch}}
	pub := &fakePublisher{}
	obs := &fakeRelayObserver{}
	r := newRelay(store, pub, obs, 10)

	n, err := r.RunOnce(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("RunOnce: n=%d err=%v", n, err)
	}
	if len(store.claims) != 1 || store.claims[0] != (relayClaim{"relay-1", 10, time.Minute}) {
		t.Fatalf("reivindicação inesperada: %+v", store.claims)
	}
	if !slices.Equal(pub.published, ids(batch)) {
		t.Fatalf("eventos devem ser publicados na ordem reivindicada: %v", pub.published)
	}
	if len(store.marks) != 1 || !slices.Equal(store.marks[0].ids, ids(batch)) {
		t.Fatalf("todos os publicados devem ser marcados de uma vez: %+v", store.marks)
	}
	if len(store.releases) != 0 || obs.failed != 0 {
		t.Fatalf("nenhuma falha esperada: releases=%v failed=%d", store.releases, obs.failed)
	}
	if !slices.Equal(obs.published, []int{3}) {
		t.Fatalf("observador deve receber a contagem marcada: %v", obs.published)
	}
}

func TestRelayRunOnceBoundsEachPublishByTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeRelayStore{batches: [][]outbox.Record{records(0, 0)}}
		pub := &fakePublisher{}
		r := newRelay(store, pub, &fakeRelayObserver{}, 10)
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(pub.deadlines) != 2 {
			t.Fatalf("cada publicação deve ter prazo próprio: %v", pub.deadlines)
		}
		for _, d := range pub.deadlines {
			if d != 10*time.Second {
				t.Fatalf("prazo de publicação %v, esperado 10s", d)
			}
		}
	})
}

func TestRelayRunOnceWithNothingToPublish(t *testing.T) {
	store := &fakeRelayStore{}
	obs := &fakeRelayObserver{}
	r := newRelay(store, &fakePublisher{}, obs, 10)
	n, err := r.RunOnce(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(store.marks) != 0 || len(obs.published) != 0 {
		t.Fatal("lote vazio não marca nem publica métricas")
	}
}

func TestRelayRunOnceClaimError(t *testing.T) {
	boom := errors.New("banco indisponível")
	store := &fakeRelayStore{claimErr: boom}
	r := newRelay(store, &fakePublisher{}, &fakeRelayObserver{}, 10)
	n, err := r.RunOnce(context.Background())
	if n != 0 || !errors.Is(err, boom) {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestRelayPublishFailureReleasesWithoutMarking(t *testing.T) {
	batch := records(0, 3, 0)
	store := &fakeRelayStore{batches: [][]outbox.Record{batch}}
	pub := &fakePublisher{fail: map[uuid.UUID]error{batch[1].ID: errors.New("broker sem confirmação")}}
	obs := &fakeRelayObserver{}
	r := newRelay(store, pub, obs, 10)

	n, err := r.RunOnce(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("falha de um evento não interrompe o lote: n=%d err=%v", n, err)
	}
	if len(store.marks) != 1 || !slices.Equal(store.marks[0].ids, []uuid.UUID{batch[0].ID, batch[2].ID}) {
		t.Fatalf("somente os confirmados são marcados: %+v", store.marks)
	}
	if len(store.releases) != 1 {
		t.Fatalf("o evento com falha deve ser liberado: %+v", store.releases)
	}
	rel := store.releases[0]
	if rel.id != batch[1].ID || rel.delay != outbox.RetryDelay(3) || rel.delay != 4*time.Second {
		t.Fatalf("liberação inesperada: %+v", rel)
	}
	if rel.cause != "broker sem confirmação" {
		t.Fatalf("causa registrada: %q", rel.cause)
	}
	if obs.failed != 1 || !slices.Equal(obs.published, []int{2}) {
		t.Fatalf("métricas: failed=%d published=%v", obs.failed, obs.published)
	}
}

func TestRelayRetryDelayGrowsWithAttempts(t *testing.T) {
	var batches [][]outbox.Record
	failing := map[uuid.UUID]error{}
	for attempts := 1; attempts <= 12; attempts++ {
		b := records(attempts)
		failing[b[0].ID] = errors.New("broker indisponível")
		batches = append(batches, b)
	}
	store := &fakeRelayStore{batches: batches}
	r := newRelay(store, &fakePublisher{fail: failing}, &fakeRelayObserver{}, 10)
	for range batches {
		if _, err := r.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.marks) != 0 {
		t.Fatalf("nada deve ser marcado como publicado: %+v", store.marks)
	}
	if len(store.releases) != len(batches) {
		t.Fatalf("cada falha libera o evento: %d", len(store.releases))
	}
	for i := 1; i < len(store.releases); i++ {
		prev, cur := store.releases[i-1].delay, store.releases[i].delay
		if cur < prev {
			t.Fatalf("espera não pode diminuir: %v depois de %v", cur, prev)
		}
		if cur > 5*time.Minute {
			t.Fatalf("espera acima do limite: %v", cur)
		}
	}
	if first, last := store.releases[0].delay, store.releases[len(store.releases)-1].delay; first != time.Second || last != 5*time.Minute {
		t.Fatalf("espera deve crescer de 1s até o limite de 5min: %v .. %v", first, last)
	}
}

func TestRelayReleaseFailureIsTolerated(t *testing.T) {
	batch := records(1)
	store := &fakeRelayStore{batches: [][]outbox.Record{batch}, relErr: errors.New("conexão perdida")}
	pub := &fakePublisher{fail: map[uuid.UUID]error{batch[0].ID: errors.New("broker indisponível")}}
	r := newRelay(store, pub, &fakeRelayObserver{}, 10)
	n, err := r.RunOnce(context.Background())
	// A liberação em melhor esforço falhou; o lease expira e o evento volta.
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(store.marks) != 0 {
		t.Fatal("evento não publicado não pode ser marcado")
	}
}

// Se a marcação falhar após o broker confirmar, o relay devolve o erro sem
// liberar os eventos. O lease expira e os mesmos eventos são reivindicados e
// publicados de novo: a entrega é pelo menos uma vez e os consumidores
// deduplicam pelo id do evento (inbox).
func TestRelayMarkFailureLeadsToRepublishAfterLeaseExpires(t *testing.T) {
	batch := records(0, 0)
	store := &fakeRelayStore{batches: [][]outbox.Record{batch, batch}, markErr: errors.New("timeout no commit")}
	pub := &fakePublisher{}
	obs := &fakeRelayObserver{}
	r := newRelay(store, pub, obs, 10)

	n, err := r.RunOnce(context.Background())
	if n != 2 || err == nil {
		t.Fatalf("falha ao marcar deve ser devolvida: n=%d err=%v", n, err)
	}
	if len(store.releases) != 0 {
		t.Fatal("eventos publicados não são liberados; o lease expira sozinho")
	}
	if len(obs.published) != 0 {
		t.Fatalf("sem marcação não há métrica de publicados: %v", obs.published)
	}

	// Lease expirado: a próxima reivindicação devolve os mesmos eventos.
	store.mu.Lock()
	store.markErr = nil
	store.mu.Unlock()
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := append(ids(batch), ids(batch)...)
	if !slices.Equal(pub.published, want) {
		t.Fatalf("os eventos devem ser republicados com o mesmo id: %v", pub.published)
	}
	if !slices.Equal(obs.published, []int{2}) {
		t.Fatalf("a segunda marcação conclui o lote: %v", obs.published)
	}
}

func TestRelayFinishesBookkeepingAfterCancellation(t *testing.T) {
	batch := records(0, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeRelayStore{batches: [][]outbox.Record{batch}}
	pub := &fakePublisher{fail: map[uuid.UUID]error{batch[1].ID: errors.New("cancelado")}, onPublish: cancel}
	r := newRelay(store, pub, &fakeRelayObserver{}, 10)
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(store.marks) != 1 || store.marks[0].ctxErr != nil {
		t.Fatalf("marcação deve usar contexto sem cancelamento: %+v", store.marks)
	}
	if len(store.releases) != 1 || store.releases[0].ctxErr != nil {
		t.Fatalf("liberação deve usar contexto sem cancelamento: %+v", store.releases)
	}
}

func TestNewRelayUsesAtLeastOneEventPerBatch(t *testing.T) {
	store := &fakeRelayStore{}
	r := newRelay(store, &fakePublisher{}, &fakeRelayObserver{}, 0)
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claims[0].batch != 1 {
		t.Fatalf("lote mínimo deve ser 1, obtive %d", store.claims[0].batch)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{0, time.Second},
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{9, 256 * time.Second},
		{10, 5 * time.Minute}, // 512s limitado a 5 minutos
		{100, 5 * time.Minute},
		{1 << 30, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := outbox.RetryDelay(c.attempts); got != c.want {
			t.Errorf("RetryDelay(%d)=%v, esperado %v", c.attempts, got, c.want)
		}
	}
	prev := time.Duration(0)
	for a := -5; a <= 1000; a++ {
		d := outbox.RetryDelay(a)
		if d < prev || d > 5*time.Minute || d < time.Second {
			t.Fatalf("RetryDelay(%d)=%v fora da progressão limitada", a, d)
		}
		prev = d
	}
}

func TestRelayRunLoopsUntilContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		full := records(0, 0)
		partial := records(0)
		store := &fakeRelayStore{batches: [][]outbox.Record{full, partial}, claimed: make(chan struct{})}
		pub := &fakePublisher{}
		r := newRelay(store, pub, &fakeRelayObserver{}, 2)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()

		start := time.Now()
		<-store.claimed // primeiro lote, cheio
		<-store.claimed // lote cheio indica mais trabalho: nova reivindicação imediata
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("após lote cheio o relay não deve esperar o tick: %v", waited)
		}
		<-store.claimed // o lote anterior foi parcial: esta reivindicação só ocorre no tick
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("após lote parcial o relay deve aguardar o tick de 1s: %v", waited)
		}
		cancel()
		<-done
		if len(pub.published) != 3 {
			t.Fatalf("eventos publicados: %d", len(pub.published))
		}
	})
}

func TestRelayRunKeepsGoingAfterErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &fakeRelayStore{claimErr: errors.New("banco indisponível"), claimed: make(chan struct{})}
		r := newRelay(store, &fakePublisher{}, &fakeRelayObserver{}, 2)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.Run(ctx)
			close(done)
		}()
		start := time.Now()
		<-store.claimed
		<-store.claimed
		if waited := time.Since(start); waited != time.Second {
			t.Fatalf("erro deve aguardar o próximo tick: %v", waited)
		}
		cancel()
		<-done
	})
}
