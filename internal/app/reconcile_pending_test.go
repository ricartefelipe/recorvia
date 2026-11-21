package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ricartefelipe/recorvia/internal/payments"
)

type fakePendingRunner struct {
	res payments.PendingRunsResult
	err error
}

func (f fakePendingRunner) RunPendingReconciliations(context.Context) (payments.PendingRunsResult, error) {
	return f.res, f.err
}

// O subcomando só termina sem erro quando todas as conciliações registradas
// foram concluídas: falha, devolução ou erro interrompem o procedimento de
// recuperação, que não pode religar o worker com a janela sem conciliar.
func TestReconcilePendingOutcomes(t *testing.T) {
	boom := errors.New("banco indisponível")
	cases := []struct {
		name    string
		runner  fakePendingRunner
		summary string
		wantErr string
	}{
		{"nada pendente", fakePendingRunner{}, "0 concluídas, 0 com falha, 0 devolvidas", ""},
		{"todas concluídas", fakePendingRunner{res: payments.PendingRunsResult{Completed: 2}}, "2 concluídas", ""},
		{"falha no provedor", fakePendingRunner{res: payments.PendingRunsResult{Completed: 1, Failed: 1}}, "1 com falha",
			"1 conciliação(ões) terminaram com falha"},
		{"devolvida", fakePendingRunner{res: payments.PendingRunsResult{Released: 1}}, "1 devolvidas", "devolvidas sem terminar"},
		{"erro", fakePendingRunner{res: payments.PendingRunsResult{Completed: 1}, err: boom}, "1 concluídas", "banco indisponível"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reconcilePending(context.Background(), tc.runner, &out)
			if !strings.Contains(out.String(), tc.summary) {
				t.Errorf("resumo %q, esperado conter %q", out.String(), tc.summary)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("erro inesperado: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("erro %v, esperado conter %q", err, tc.wantErr)
			}
			if tc.runner.err != nil && !errors.Is(err, boom) {
				t.Fatalf("a causa deve ser preservada: %v", err)
			}
		})
	}
}
