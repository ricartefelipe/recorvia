package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Record é um evento reivindicado para publicação.
type Record struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	Type       string
	Payload    []byte
	Headers    map[string]string
	OccurredAt time.Time
	Attempts   int
}

// RelayStore é a persistência usada pelo relay.
type RelayStore interface {
	Claim(ctx context.Context, worker string, batch int, lease time.Duration) ([]Record, error)
	MarkPublished(ctx context.Context, worker string, ids []uuid.UUID) (int, error)
	Release(ctx context.Context, worker string, id uuid.UUID, delay time.Duration, cause string) error
}

// Publisher publica um evento e só retorna nil após confirmação do broker.
type Publisher interface {
	PublishEvent(ctx context.Context, r Record) error
}

// RelayObserver recebe sinais para métricas.
type RelayObserver interface {
	Published(n int)
	PublishFailed()
}

// Relay publica eventos da outbox. Reivindica lotes com lease (transação
// curta), publica fora de transação com publisher confirms e marca como
// publicados. Se o relay cair após publicar e antes de marcar, o lease expira
// e o evento é publicado de novo: consumidores deduplicam pelo id (inbox).
type Relay struct {
	store    RelayStore
	pub      Publisher
	worker   string
	batch    int
	lease    time.Duration
	interval time.Duration
	log      *slog.Logger
	obs      RelayObserver
}

// NewRelay cria o relay.
func NewRelay(store RelayStore, pub Publisher, worker string, batch int, lease, interval time.Duration, log *slog.Logger, obs RelayObserver) *Relay {
	return &Relay{store: store, pub: pub, worker: worker, batch: max(batch, 1), lease: lease, interval: interval, log: log.With("component", "outbox_relay"), obs: obs}
}

// Run executa até ctx terminar.
func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		n, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Warn("ciclo do relay falhou", slog.String("error", err.Error()))
		}
		if n == r.batch && err == nil {
			continue // há mais trabalho: segue sem esperar o próximo tick
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RetryDelay calcula a espera antes de nova tentativa de publicação.
func RetryDelay(attempts int) time.Duration {
	d := time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// RunOnce processa um lote e retorna quantos eventos foram reivindicados.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	records, err := r.store.Claim(ctx, r.worker, r.batch, r.lease)
	if err != nil || len(records) == 0 {
		return 0, err
	}
	published := make([]uuid.UUID, 0, len(records))
	for _, rec := range records {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := r.pub.PublishEvent(pctx, rec)
		cancel()
		if err != nil {
			r.obs.PublishFailed()
			if rerr := r.store.Release(context.WithoutCancel(ctx), r.worker, rec.ID, RetryDelay(rec.Attempts), err.Error()); rerr != nil {
				r.log.Warn("liberação de evento falhou; o lease expirará", slog.String("event_id", rec.ID.String()), slog.String("error", rerr.Error()))
			}
			continue
		}
		published = append(published, rec.ID)
	}
	if len(published) > 0 {
		n, err := r.store.MarkPublished(context.WithoutCancel(ctx), r.worker, published)
		if err != nil {
			return len(records), err
		}
		r.obs.Published(n)
	}
	return len(records), nil
}
