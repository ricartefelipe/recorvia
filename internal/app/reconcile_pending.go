package app

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/payments"
)

// ReconcilePendingCommand é o subcomando do worker que executa uma vez as
// conciliações registradas e termina, sem iniciar o agendador, os
// consumidores nem o publicador da outbox. Serve à recuperação de desastre:
// a janela perdida é conciliada pela API antes de religar o worker, que
// voltaria a emitir faturas e criar cobranças (ADR-064).
const ReconcilePendingCommand = "reconcile-pending"

// PendingReconciliationRunner executa as conciliações registradas que
// aguardam um worker.
type PendingReconciliationRunner interface {
	RunPendingReconciliations(ctx context.Context) (payments.PendingRunsResult, error)
}

// RunReconcilePending executa o subcomando reconcile-pending: abre o banco
// com a configuração do processo, confere o keyring como na partida do
// worker, executa as conciliações registradas e escreve o resumo em out.
// Devolve erro quando alguma execução falhou ou foi devolvida sem terminar,
// para que o procedimento de recuperação não siga com a janela sem conciliar.
func RunReconcilePending(ctx context.Context, getenv config.Getenv, out io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}
	if err := cfg.ValidateStore(); err != nil {
		return err
	}
	log := logging.New(os.Stderr, cfg.LogLevel, "reconcile-pending")
	core, err := Open(ctx, cfg, log, "recorvia-reconcile-pending")
	if err != nil {
		return err
	}
	defer core.Close()
	if err := VerifyKeyring(ctx, core.Store, cfg.EncryptionStartupCheck, log); err != nil {
		return err
	}
	return reconcilePending(ctx, core.Payments, out)
}

func reconcilePending(ctx context.Context, r PendingReconciliationRunner, out io.Writer) error {
	res, err := r.RunPendingReconciliations(ctx)
	if _, werr := fmt.Fprintf(out, "conciliações registradas: %d concluídas, %d com falha, %d devolvidas sem terminar\n",
		res.Completed, res.Failed, res.Released); werr != nil {
		return werr
	}
	switch {
	case err != nil:
		return fmt.Errorf("execução das conciliações registradas: %w", err)
	case res.Failed > 0:
		return fmt.Errorf("%d conciliação(ões) terminaram com falha; consulte a causa em GET /api/v1/reconciliation/runs", res.Failed)
	case res.Released > 0:
		return fmt.Errorf("%d conciliação(ões) foram devolvidas sem terminar; execute o subcomando de novo", res.Released)
	}
	return nil
}
