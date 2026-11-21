package payments

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/money"
)

// RefundTx reúne as operações de reembolso e de operações externas por
// cobrança, dentro da transação do tenant.
type RefundTx interface {
	InsertRefund(ctx context.Context, r Refund) error
	Refund(ctx context.Context, id uuid.UUID, forUpdate bool) (Refund, error)
	RefundByRequestKey(ctx context.Context, key string) (Refund, error)
	UpdateRefund(ctx context.Context, r *Refund) error
	ChargeRefunds(ctx context.Context, chargeID uuid.UUID) ([]Refund, error)
	ChargeOperations(ctx context.Context, chargeID uuid.UUID, kind OperationKind) ([]Operation, error)
	// LockChargeRefundClaims serializa, até o fim da transação, a
	// reivindicação das operações de reembolso da cobrança (bloqueio
	// consultivo, sem bloquear a linha da cobrança, que a sincronização por
	// webhook bloqueia antes das operações).
	LockChargeRefundClaims(ctx context.Context, chargeID uuid.UUID) error
	// EnsureOperation grava a operação se ainda não houver outra equivalente
	// (por exemplo, um cancelamento por cobrança); false indica repetição.
	EnsureOperation(ctx context.Context, op Operation) (bool, error)
	// OverpaymentCreditBase devolve o excedente do pagamento da cobrança que
	// virou crédito do cliente e quanto dele já foi devolvido ao pagador: por
	// reembolsos (refunds.credit_amount) e por chargebacks de disputas
	// perdidas (crédito consumido e valor a recuperar).
	OverpaymentCreditBase(ctx context.Context, chargeID uuid.UUID) (overpaid, returned money.Money, err error)
	// LockCustomerCredits bloqueia, até o fim da transação, as movimentações
	// de crédito do cliente (a mesma serialização do consumo do crédito), para
	// que o saldo lido em seguida não mude antes do consumo.
	LockCustomerCredits(ctx context.Context, customerID uuid.UUID) error
	CustomerCreditBalance(ctx context.Context, customerID uuid.UUID) (money.Money, error)
}

// DisputeTx reúne as operações de disputas e chargebacks.
type DisputeTx interface {
	LatestDispute(ctx context.Context, chargeID uuid.UUID) (*Dispute, error)
	InsertDispute(ctx context.Context, d Dispute) error
	UpdateDispute(ctx context.Context, d *Dispute) error
	InsertDisputeEvent(ctx context.Context, e DisputeEvent) error
	// ReverseLedger estorna o lançamento identificado pela chave original.
	ReverseLedger(ctx context.Context, originalKey, description string, at time.Time) error
}

// ReconciliationTx reúne as operações de conciliação.
type ReconciliationTx interface {
	ReconciliationRun(ctx context.Context, id uuid.UUID, forUpdate bool) (ReconciliationRun, error)
	ReconciliationRunByRequestKey(ctx context.Context, key string) (ReconciliationRun, error)
	// UpdateReconciliationRunLease grava o lease e a contagem de tentativas
	// da execução em andamento (assumida, retomada ou devolvida).
	UpdateReconciliationRunLease(ctx context.Context, r ReconciliationRun) error
	InsertReconciliationRun(ctx context.Context, r ReconciliationRun) error
	FinishReconciliationRun(ctx context.Context, r ReconciliationRun) error
	// ChargesForReconciliation lista as cobranças locais da execução: as
	// presentes na listagem do provedor primeiro e depois as criadas ou
	// atualizadas na janela, até o limite.
	ChargesForReconciliation(ctx context.Context, q ReconciliationScope) ([]Charge, error)
	// ChargesForRotation lista as cobranças do rodízio: fora da janela, ainda
	// sujeitas a mudança no provedor, a começar pelas verificadas há mais
	// tempo.
	ChargesForRotation(ctx context.Context, q RotationScope) ([]Charge, error)
	// MarkChargesReconciled registra a verificação das cobranças comparadas
	// numa execução concluída.
	MarkChargesReconciled(ctx context.Context, chargeIDs []uuid.UUID, at time.Time) error
	RefundsForCharges(ctx context.Context, chargeIDs []uuid.UUID) ([]Refund, error)
	// UnknownOperations lista as operações com resultado desconhecido:
	// situação unknown ou em execução com o lease expirado em now.
	UnknownOperations(ctx context.Context, now time.Time, limit int) ([]Operation, error)
	DiscrepancyByKey(ctx context.Context, key string) (*Discrepancy, error)
	InsertDiscrepancy(ctx context.Context, d Discrepancy) error
	UpdateDiscrepancy(ctx context.Context, d *Discrepancy) error
	Discrepancy(ctx context.Context, id uuid.UUID, forUpdate bool) (Discrepancy, error)
	OpenDiscrepanciesForSubjects(ctx context.Context, provider string, chargeIDs []uuid.UUID, providerChargeIDs []string) ([]Discrepancy, error)
}

// ReconciliationScope delimita as cobranças locais de uma execução.
type ReconciliationScope struct {
	AccountID         uuid.UUID
	WindowStart       time.Time
	WindowEnd         time.Time
	ProviderChargeIDs []string
	ReferenceIDs      []uuid.UUID
	Limit             int
}

// RotationScope delimita o rodízio de cobranças fora da janela da execução:
// cobranças da conta vinculadas ao provedor, aguardando pagamento, pagas,
// parcialmente reembolsadas ou em disputa, criadas desde Since.
type RotationScope struct {
	AccountID   uuid.UUID
	Since       time.Time
	WindowStart time.Time
	WindowEnd   time.Time
	Limit       int
}
