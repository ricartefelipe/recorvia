package pgstore

import (
	"context"
	"time"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
)

// DatabaseConnections retorna as conexões de clientes abertas no servidor
// PostgreSQL (todas as aplicações) e o limite disponível para papéis comuns,
// para as métricas de saturação do banco.
func (s *Store) DatabaseConnections(ctx context.Context) (open, available int64, err error) {
	err = s.inGlobal(ctx, func(q *dbgen.Queries) error {
		r, err := q.DatabaseConnectionStats(ctx)
		open, available = r.Connections, r.AvailableConnections
		return err
	})
	return open, available, err
}

// BacklogStat é o tamanho de uma fila e a idade, em segundos, do item mais
// antigo (zero com a fila vazia). O significado da idade depende da fila e
// está descrito em cada consulta.
type BacklogStat struct {
	Total         int64
	OldestSeconds float64
}

// Situações da fila de notificações exportadas nas métricas.
const (
	// NotificationBacklogDue são as agendadas com o horário de envio vencido.
	NotificationBacklogDue = "due"
	// NotificationBacklogSending são as reivindicadas para envio.
	NotificationBacklogSending = "sending"
	// NotificationBacklogFailed são as encerradas com falha definitiva na
	// janela consultada.
	NotificationBacklogFailed = "failed"
)

// DeadLetterStats conta as dead letters abertas por consumidor em todos os
// tenants, inclusive as registradas sem tenant, sem identificá-los. A idade
// é o tempo desde a falha da mais antiga, medido em now. Consumidores sem
// dead letter aberta não aparecem.
func (s *Store) DeadLetterStats(ctx context.Context, now time.Time) (map[string]BacklogStat, error) {
	out := map[string]BacklogStat{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.DeadLetterStats(ctx, now)
		for _, r := range rows {
			out[r.Consumer] = BacklogStat{Total: r.OpenTotal, OldestSeconds: r.OldestOpenSeconds}
		}
		return err
	})
	return out, err
}

// NotificationBacklogStats mede a fila de notificações de todos os tenants,
// sem identificá-los, nas situações NotificationBacklogDue (idade: atraso em
// relação ao horário de envio), NotificationBacklogSending (idade: tempo
// desde a reivindicação) e NotificationBacklogFailed (falhas definitivas
// depois de failedSince; idade: tempo desde a mais antiga da janela).
func (s *Store) NotificationBacklogStats(ctx context.Context, now, failedSince time.Time) (map[string]BacklogStat, error) {
	out := map[string]BacklogStat{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.NotificationBacklogStats(ctx, dbgen.NotificationBacklogStatsParams{Now: now, FailedSince: failedSince})
		for _, r := range rows {
			out[r.State] = BacklogStat{Total: r.Total, OldestSeconds: r.OldestSeconds}
		}
		return err
	})
	return out, err
}

// WebhookDeliveryBacklogStats mede a fila de entregas de webhooks enviados de
// todos os tenants, sem identificá-los, por situação (pending e retrying). A
// idade é o atraso da entrega devida mais antiga em relação ao horário da
// tentativa; entregas em espera de backoff contam no total e não no atraso.
// Situações sem entregas não aparecem.
func (s *Store) WebhookDeliveryBacklogStats(ctx context.Context, now time.Time) (map[string]BacklogStat, error) {
	out := map[string]BacklogStat{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.WebhookDeliveryBacklogStats(ctx, now)
		for _, r := range rows {
			out[r.State] = BacklogStat{Total: r.Total, OldestSeconds: r.OldestDueSeconds}
		}
		return err
	})
	return out, err
}
