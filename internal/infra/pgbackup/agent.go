package pgbackup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

// Motivos de um backup base (log).
const (
	ReasonNone     = "nenhum backup base no armazenamento"
	ReasonInterval = "intervalo desde o último backup base vencido"
	ReasonTimeline = "servidor numa linha do tempo posterior à do último backup base (restauração ou promoção)"
)

// NextBackup decide quando o próximo backup base deve começar e por quê:
// imediatamente sem backup, com o último numa linha do tempo anterior à do
// servidor (depois de uma restauração para um instante, o WAL da linha nova
// só é útil a partir de um backup dela) ou com o intervalo vencido; senão, no
// fim do último mais o intervalo. serverTimeline zero (desconhecida) não
// antecipa o backup.
func NextBackup(now time.Time, latest *Backup, serverTimeline uint32, interval time.Duration) (time.Time, string) {
	switch {
	case latest == nil:
		return now, ReasonNone
	case serverTimeline > 0 && latest.Timeline() > 0 && latest.Timeline() < serverTimeline:
		return now, ReasonTimeline
	}
	next := latest.FinishTime.Add(interval)
	if !next.After(now) {
		return now, ReasonInterval
	}
	return next, ""
}

// Server é o acesso do agente ao PostgreSQL.
type Server interface {
	Timeline(ctx context.Context) (uint32, error)
}

// Agent executa os backups base agendados, aplica a retenção e mantém as
// métricas dos backups.
type Agent struct {
	Config  Config
	Runner  Runner
	Server  Server
	Clock   clock.Clock
	Metrics *telemetry.BackupMetrics
	Log     *slog.Logger
	// Lock impede dois backups base simultâneos (o agente e um comando
	// avulso no mesmo container).
	Lock func() (unlock func(), err error)
	// Sleep espera d ou até o contexto terminar (erro).
	Sleep func(ctx context.Context, d time.Duration) error
	// RefreshEvery é o intervalo máximo entre leituras do armazenamento
	// (métricas atualizadas mesmo sem backups novos).
	RefreshEvery time.Duration
}

// Refresh lê a lista de backups do armazenamento e atualiza as métricas.
func (a *Agent) Refresh(ctx context.Context) ([]Backup, error) {
	list, err := ListBackups(ctx, a.Runner)
	if err != nil {
		a.Metrics.DependencyUp.WithLabelValues("storage").Set(0)
		return nil, err
	}
	a.Metrics.DependencyUp.WithLabelValues("storage").Set(1)
	a.Metrics.BaseRetained.Set(float64(len(list)))
	last := 0.0
	if b := Latest(list); b != nil {
		last = float64(b.FinishTime.UnixNano()) / 1e9
	}
	a.Metrics.BaseLastSuccess.Set(last)
	return list, nil
}

// BackupNow faz um backup base, aplica a retenção e atualiza as métricas. A
// falha da retenção não invalida o backup feito, mas é devolvida.
func (a *Agent) BackupNow(ctx context.Context, reason string) error {
	unlock, err := a.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	start := a.Clock.Now()
	a.Log.Info("backup base iniciado", slog.String("reason", reason), slog.String("pgdata", a.Config.PGData))
	if err := PushBackup(ctx, a.Runner, a.Config.PGData, a.Config.KeyFingerprint()); err != nil {
		a.Metrics.BaseRuns.WithLabelValues("failure").Inc()
		a.Log.Error("backup base falhou", slog.String("error", err.Error()))
		return err
	}
	took := a.Clock.Now().Sub(start)
	a.Metrics.BaseRuns.WithLabelValues("success").Inc()
	a.Metrics.BaseLastDuration.Set(took.Seconds())
	a.Log.Info("backup base concluído", slog.Duration("duration", took))
	retErr := ApplyRetention(ctx, a.Runner, a.Clock.Now(), a.Config.RetainFull, a.Config.RetainDays)
	if retErr != nil {
		a.Log.Error("retenção falhou", slog.String("error", retErr.Error()))
		retErr = fmt.Errorf("retenção: %w", retErr)
	} else {
		a.Log.Info("retenção aplicada", slog.Int("retain_full", a.Config.RetainFull), slog.Int("retain_days", a.Config.RetainDays))
	}
	if _, err := a.Refresh(ctx); err != nil {
		a.Log.Warn("listagem do armazenamento falhou depois do backup", slog.String("error", err.Error()))
	}
	return retErr
}

// Run executa o agendamento até o contexto terminar. Depois de uma falha
// (armazenamento, servidor ou backup), a próxima tentativa espera
// RetryInterval.
func (a *Agent) Run(ctx context.Context) error {
	a.Metrics.BaseInterval.Set(a.Config.BaseInterval.Seconds())
	var retryAt time.Time
	for {
		wait := a.Config.RetryInterval
		list, err := a.Refresh(ctx)
		switch {
		case err != nil:
			a.Log.Error("listagem dos backups falhou", slog.String("error", err.Error()))
		default:
			tl, terr := a.Server.Timeline(ctx)
			if terr != nil {
				a.Log.Error("linha do tempo do servidor indisponível", slog.String("error", terr.Error()))
				break
			}
			now := a.Clock.Now()
			due, reason := NextBackup(now, Latest(list), tl, a.Config.BaseInterval)
			if due.Before(retryAt) {
				due = retryAt
			}
			if !due.After(now) {
				err := a.BackupNow(ctx, reason)
				if ctx.Err() != nil {
					return nil //nolint:nilerr // término pedido: o backup interrompido não é falha do agente
				}
				// Depois de qualquer tentativa, a seguinte espera ao menos
				// RetryInterval: uma falha não vira repetição contínua, e uma
				// listagem atrasada do armazenamento não dispara outro backup.
				retryAt = a.Clock.Now().Add(a.Config.RetryInterval)
				if err != nil {
					a.Log.Warn("próxima tentativa de backup base", slog.Time("at", retryAt))
				}
				continue
			}
			wait = due.Sub(now)
		}
		if a.RefreshEvery > 0 && wait > a.RefreshEvery {
			wait = a.RefreshEvery
		}
		if err := a.Sleep(ctx, wait); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

// SleepContext espera d ou até o contexto terminar.
func SleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
