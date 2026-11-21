package payments

import (
	"context"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/ledger"
)

// Ciclo do pagamento pelo provedor no ledger (ADR-080):
//
//   - confirmação (qualquer situação paga): o valor bruto sai de contas a
//     receber (e o excedente vira crédito do cliente) para pagamentos
//     confirmados no provedor;
//   - recebimento (situação recebida ou posterior a ela): o bruto confirmado
//     passa aos valores em liquidação, a taxa é reconhecida e o líquido é
//     liquidado no disponível;
//   - movimentações posteriores com o provedor (reembolso, retenção e
//     liberação de chargeback, ajuste de conciliação): lançadas contra os
//     valores em liquidação e liquidadas no disponível.

// postProviderMovement grava a movimentação com o provedor e a liquidação do
// seu efeito nos valores em liquidação no disponível (ledger.Settlement). As
// duas chaves de idempotência derivam da movimentação.
func postProviderMovement(ctx context.Context, tx Tx, t ledger.Transaction) error {
	if err := tx.PostLedger(ctx, t); err != nil {
		return err
	}
	st, ok, err := ledger.Settlement(ids.New(), t)
	if err != nil || !ok {
		return err
	}
	return tx.PostLedger(ctx, st)
}

// postChargeMovement registra o recebimento do pagamento da cobrança, se
// ainda não registrado, e grava a movimentação com o provedor. Reembolso e
// chargeback só existem sobre valor que o provedor processou: o pagamento
// ainda apenas confirmado é dado como recebido antes da movimentação, para
// que reembolsos e disputas sempre movimentem os valores em liquidação e o
// valor confirmado não fique esquecido em pagamentos confirmados.
func (s *Service) postChargeMovement(ctx context.Context, tx Tx, c *Charge, t ledger.Transaction, now time.Time) error {
	if err := s.ensureReceived(ctx, tx, c, now); err != nil {
		return err
	}
	return postProviderMovement(ctx, tx, t)
}

// ensureReceived registra o recebimento do pagamento da cobrança pelo
// provedor: transfere o valor ainda em pagamentos confirmados para os valores
// em liquidação, reconhece a taxa do pagamento e liquida o líquido no
// disponível. É idempotente e independe da ordem dos eventos: depois do
// primeiro recebimento não resta valor confirmado, e as chaves dos
// lançamentos derivam do pagamento. Pagamentos confirmados antes da ADR-080
// (lançados direto nos valores em liquidação) não têm valor confirmado e não
// geram lançamento.
func (s *Service) ensureReceived(ctx context.Context, tx Tx, c *Charge, now time.Time) error {
	pay, pending, ok, err := tx.PaymentPendingReceipt(ctx, c.ID)
	if err != nil || !ok || !pending.IsPositive() {
		return err
	}
	t, posted, err := ledger.ProviderPaymentReceived(ids.New(), c.TenantID, pay.ID, pending, pay.Fee, now)
	if err != nil || !posted {
		return err
	}
	return postProviderMovement(ctx, tx, t)
}
