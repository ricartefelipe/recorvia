package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// Parâmetros da conciliação.
const (
	// ManualReconciliationWindow é a janela padrão da execução manual.
	ManualReconciliationWindow = 30 * 24 * time.Hour
	// ScheduledReconciliationWindow é a janela da execução agendada.
	ScheduledReconciliationWindow = 7 * 24 * time.Hour
	// ScheduledReconciliationEvery é o intervalo mínimo entre execuções
	// agendadas do mesmo tenant.
	ScheduledReconciliationEvery = 24 * time.Hour
	// reconciliationLease limita a duração de cada tentativa de execução;
	// depois dele, a execução é considerada interrompida e outro worker a
	// retoma.
	reconciliationLease = 15 * time.Minute
	// ReconciliationCollectionBudget é o prazo padrão da coleta (chamadas ao
	// provedor) de uma tentativa, manual ou agendada, com folga para a
	// comparação e o registro dentro do lease. Não depende de requisição
	// HTTP: a execução manual é feita pelo worker.
	ReconciliationCollectionBudget = reconciliationLease - 2*time.Minute
	// MaxReconciliationAttempts limita as vezes em que uma execução é
	// assumida: a interrompida (worker reiniciado, lease expirado) é retomada
	// até esse limite e depois encerrada como falha, com a causa.
	MaxReconciliationAttempts = 3
	// reconciliationClaimBatch é o número de execuções descobertas por
	// consulta do executor.
	reconciliationClaimBatch = 20
	providerPageSize         = 100
	maxProviderPages         = 20
	maxIndividualLookups     = 100
	// maxLocalCharges limita as cobranças locais da janela. As presentes na
	// listagem do provedor (no máximo duas por cobrança listada, pelo ID do
	// provedor e pela referência) são carregadas primeiro e sempre cabem no
	// limite, que só corta cobranças da janela ausentes da listagem.
	maxLocalCharges      = 5000
	maxUnknownOperations = 500
	// rotationLookups limita, por execução, as consultas individuais do
	// rodízio de cobranças fora da janela.
	rotationLookups = 100
	// rotationMaxFailures é o número de falhas seguidas de consulta que
	// indicam provedor indisponível (sem resposta, limite de taxa, erro 5xx)
	// e interrompem o rodízio.
	rotationMaxFailures = 3
	// rotationSpan é a idade máxima das cobranças do rodízio, próxima do
	// prazo de contestação de pagamentos com cartão.
	rotationSpan          = 180 * 24 * time.Hour
	maxReconciliationSpan = 93 * 24 * time.Hour
)

// Motivos de execução parcial.
const (
	partialProviderPages = "limite de páginas da listagem do provedor"
	partialLookups       = "limite de consultas individuais ao provedor"
	partialBudget        = "prazo da coleta esgotado"
	partialLocalCharges  = "limite de cobranças locais da janela"
	partialUnknownOps    = "limite de operações com resultado desconhecido"
)

var reconciliationActor = audit.System("reconciliation")

// RunInput parametriza uma execução. Janela vazia usa o padrão do gatilho.
type RunInput struct {
	Trigger     RunTrigger
	WindowStart time.Time
	WindowEnd   time.Time
	// RequestKey identifica o pedido manual (derivado do Idempotency-Key).
	RequestKey string
}

func (s *Service) runWindow(in RunInput) (time.Time, time.Time, error) {
	now := s.clock.Now().UTC()
	start, end := in.WindowStart.UTC(), in.WindowEnd.UTC()
	if end.IsZero() {
		end = now
	}
	if start.IsZero() {
		span := ManualReconciliationWindow
		if in.Trigger == TriggerScheduled {
			span = ScheduledReconciliationWindow
		}
		start = end.Add(-span)
	}
	if !start.Before(end) || end.Sub(start) > maxReconciliationSpan || end.After(now.Add(time.Minute)) {
		return time.Time{}, time.Time{}, apperr.Invalid("invalid_reconciliation_window",
			"Janela de conciliação inválida: o início deve anteceder o fim, sem datas futuras e com até 93 dias.")
	}
	return start, end, nil
}

// RunReconciliation registra a execução da conciliação do provedor padrão do
// tenant sobre a janela e a executa em seguida, no próprio chamador (tarefa
// agendada): lista as cobranças do provedor, consulta individualmente as
// locais ausentes da listagem, compara e registra divergências. As chamadas
// ao provedor ocorrem sem transação aberta. A execução agendada exige o
// recurso reconciliation_automation do plano da plataforma. Há no máximo uma
// execução em andamento por conta. A execução manual pedida pela API é
// apenas registrada (StartReconciliation) e executada pelo worker
// (RunPendingReconciliations).
func (s *Service) RunReconciliation(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput) (ReconciliationRun, error) {
	run, account, existing, err := s.registerRun(ctx, tenantID, actor, userID, in, true)
	if err != nil || existing {
		return run, err
	}
	done, _, err := s.executeRun(ctx, tenantID, run, account)
	return done, err
}

// StartReconciliation valida o pedido e registra a execução em andamento,
// sem lease, para o worker assumir (ou devolve a já registrada para a mesma
// chave do pedido, na situação atual). Não chama o provedor: quem pede não
// espera a coleta.
func (s *Service) StartReconciliation(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput) (ReconciliationRun, error) {
	run, _, _, err := s.registerRun(ctx, tenantID, actor, userID, in, false)
	return run, err
}

// registerRun registra a execução em andamento numa única transação: devolve
// a execução já registrada para a mesma chave do pedido, exige o recurso
// reconciliation_automation na execução agendada e grava a nova execução. O
// índice único parcial das execuções em andamento recusa a segunda execução
// simultânea da mesma conta (reconciliation_running), inclusive enquanto a
// anterior aguarda um worker ou a retomada. claimed registra a execução já
// assumida por quem chama (lease e primeira tentativa); sem ele, a execução
// fica aguardando o worker.
func (s *Service) registerRun(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput,
	claimed bool) (ReconciliationRun, ProviderAccount, bool, error) {
	if in.Trigger != TriggerScheduled {
		in.Trigger = TriggerManual
	}
	start, end, err := s.runWindow(in)
	if err != nil {
		return ReconciliationRun{}, ProviderAccount{}, false, err
	}
	var (
		run      ReconciliationRun
		account  ProviderAccount
		existing bool
	)
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		existing = false
		if in.RequestKey != "" {
			prev, err := tx.ReconciliationRunByRequestKey(ctx, in.RequestKey)
			switch {
			case err == nil:
				run, existing = prev, true
				return nil
			case !apperr.IsKind(err, apperr.KindNotFound):
				return err
			}
		}
		if in.Trigger == TriggerScheduled {
			plan, err := tx.PlatformPlan(ctx)
			if err != nil {
				return err
			}
			if err := platformbilling.RequireFeature(plan, platformbilling.FeatureReconciliationRun); err != nil {
				return err
			}
		}
		var err error
		account, err = tx.DefaultAccount(ctx)
		if apperr.IsKind(err, apperr.KindNotFound) {
			return apperr.Conflict("provider_not_configured",
				"Nenhum provedor de pagamento padrão ativo está configurado; a conciliação exige um provedor.")
		}
		if err != nil {
			return err
		}
		now := s.clock.Now()
		run = ReconciliationRun{ID: ids.New(), TenantID: tenantID, ProviderAccountID: account.ID, Provider: account.Provider,
			Trigger: in.Trigger, Status: RunRunning, WindowStart: start, WindowEnd: end, RequestedBy: userID, RequestKey: in.RequestKey,
			StartedAt: now}
		if claimed {
			lease := now.Add(reconciliationLease)
			run.LeaseUntil, run.Attempts = &lease, 1
		}
		if err := tx.InsertReconciliationRun(ctx, run); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "reconciliation.run_started", "reconciliation_run", run.ID.String(),
			map[string]any{"trigger": string(run.Trigger), "provider": run.Provider, "window_start": start, "window_end": end,
				"queued": !claimed}))
	})
	if err != nil {
		return ReconciliationRun{}, ProviderAccount{}, false, err
	}
	return run, account, existing, nil
}

// RunOutcome é o resultado de uma tentativa de assumir e executar uma
// execução registrada.
type RunOutcome string

const (
	// RunOutcomeSkipped: a execução não pôde ser assumida (já encerrada ou
	// com lease vigente de outra instância).
	RunOutcomeSkipped RunOutcome = "skipped"
	// RunOutcomeCompleted: concluída (inclusive com aviso de parcial).
	RunOutcomeCompleted RunOutcome = "completed"
	// RunOutcomeFailed: encerrada como falha, com a causa.
	RunOutcomeFailed RunOutcome = "failed"
	// RunOutcomeReleased: interrompida por quem executava (worker
	// encerrando) e devolvida para ser retomada.
	RunOutcomeReleased RunOutcome = "released"
)

// ExecuteReconciliationRun assume a execução registrada, se ela puder ser
// assumida agora (aguardando ou com o lease expirado), e a executa: coleta,
// compara e encerra. A execução interrompida é retomada do início; como as
// divergências só são gravadas na transação que encerra a execução, e pela
// chave de deduplicação, a retomada não as duplica. Depois de
// MaxReconciliationAttempts tentativas interrompidas, a execução é encerrada
// como falha, com a causa.
func (s *Service) ExecuteReconciliationRun(ctx context.Context, tenantID, runID uuid.UUID) (ReconciliationRun, RunOutcome, error) {
	run, account, claim, err := s.claimRun(ctx, tenantID, runID)
	if err != nil {
		return ReconciliationRun{}, RunOutcomeSkipped, err
	}
	switch claim {
	case claimExhausted:
		return run, RunOutcomeFailed, nil
	case claimSkipped:
		return run, RunOutcomeSkipped, nil
	}
	done, released, err := s.executeRun(ctx, tenantID, run, account)
	switch {
	case released:
		return done, RunOutcomeReleased, nil
	case err != nil:
		return done, RunOutcomeSkipped, err
	case done.Status == RunCompleted:
		return done, RunOutcomeCompleted, nil
	case done.Status == RunFailed:
		return done, RunOutcomeFailed, nil
	}
	return done, RunOutcomeSkipped, nil
}

// claimState é o resultado da tentativa de assumir uma execução.
type claimState int

const (
	// claimSkipped: já encerrada ou com lease vigente de outra instância.
	claimSkipped claimState = iota
	// claimTaken: assumida por quem chamou.
	claimTaken
	// claimExhausted: esgotou as tentativas e foi encerrada como falha.
	claimExhausted
)

// claimRun assume a execução numa transação: novo lease e mais uma
// tentativa. Não assume a execução já encerrada ou com lease vigente
// (claimSkipped); a que esgotou as tentativas é encerrada aqui como falha
// (claimExhausted).
func (s *Service) claimRun(ctx context.Context, tenantID, runID uuid.UUID) (ReconciliationRun, ProviderAccount, claimState, error) {
	var (
		run     ReconciliationRun
		account ProviderAccount
		state   claimState
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		state = claimSkipped
		cur, err := tx.ReconciliationRun(ctx, runID, true)
		if err != nil {
			return err
		}
		run = cur
		now := s.clock.Now()
		if !cur.Claimable(now) {
			return nil
		}
		if cur.Attempts >= MaxReconciliationAttempts {
			cur.Status, cur.FinishedAt, cur.LeaseUntil = RunFailed, &now, nil
			cur.Error = fmt.Sprintf("Execução interrompida %d vezes antes da conclusão (worker reiniciado ou prazo da tentativa esgotado); "+
				"nenhuma divergência desta execução foi registrada. Execute a conciliação novamente.", cur.Attempts)
			if err := tx.FinishReconciliationRun(ctx, cur); err != nil {
				return err
			}
			run, state = cur, claimExhausted
			return tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.run_failed", "reconciliation_run", cur.ID.String(),
				map[string]any{"error": cur.Error, "attempts": cur.Attempts}))
		}
		resumed := cur.Attempts > 0
		lease := now.Add(reconciliationLease)
		cur.Attempts, cur.LeaseUntil = cur.Attempts+1, &lease
		if err := tx.UpdateReconciliationRunLease(ctx, cur); err != nil {
			return err
		}
		if account, err = tx.Account(ctx, cur.ProviderAccountID); err != nil {
			return err
		}
		run, state = cur, claimTaken
		if !resumed {
			return nil
		}
		return tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.run_resumed", "reconciliation_run", cur.ID.String(),
			map[string]any{"attempt": cur.Attempts}))
	})
	if err != nil {
		return ReconciliationRun{}, ProviderAccount{}, claimSkipped, err
	}
	return run, account, state, nil
}

// executeRun coleta, compara e encerra a execução assumida (run.Attempts
// identifica a tentativa). As chamadas ao provedor têm prazo próprio
// (Config.ReconciliationBudget, uma fração do lease); esgotado o prazo, a
// execução é concluída com o que foi coletado e com aviso de execução
// parcial. Falha do provedor ou da leitura encerra a execução como failed
// (sem erro devolvido). O cancelamento de quem executa (worker encerrando ou
// prazo da tarefa agendada) não é falha da conciliação: a execução é
// devolvida (released) para ser retomada.
func (s *Service) executeRun(ctx context.Context, tenantID uuid.UUID, run ReconciliationRun, account ProviderAccount) (ReconciliationRun, bool, error) {
	result, err := s.collectAndCompare(ctx, s.cfg.ReconciliationBudget, tenantID, account, run)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			released, rerr := s.releaseRun(context.WithoutCancel(ctx), tenantID, run, cause)
			if rerr != nil {
				return run, false, errors.Join(cause, rerr)
			}
			return released, true, fmt.Errorf("conciliação interrompida e devolvida para retomada: %w", cause)
		}
		failed, ferr := s.failRun(context.WithoutCancel(ctx), tenantID, run, err)
		return failed, false, ferr
	}
	done, err := s.completeRun(context.WithoutCancel(ctx), tenantID, run, result)
	if err != nil && !apperr.IsKind(err, apperr.KindConflict) {
		if _, ferr := s.failRun(context.WithoutCancel(ctx), tenantID, run, err); ferr != nil {
			return run, false, errors.Join(err, ferr)
		}
	}
	return done, false, err
}

// releaseRun devolve a execução interrompida por quem a executava: remove o
// lease, para que o próximo worker a retome sem esperar a expiração. Não faz
// nada se outra instância já a assumiu ou encerrou.
func (s *Service) releaseRun(ctx context.Context, tenantID uuid.UUID, run ReconciliationRun, cause error) (ReconciliationRun, error) {
	if s.cfg.Log != nil {
		s.cfg.Log.WarnContext(ctx, "conciliação interrompida e devolvida para retomada", slog.String("run_id", run.ID.String()),
			slog.Int("attempt", run.Attempts), slog.String("error", cause.Error()))
	}
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cur, err := tx.ReconciliationRun(ctx, run.ID, true)
		if err != nil {
			return err
		}
		if !cur.ownedBy(run) {
			run = cur
			return nil
		}
		cur.LeaseUntil = nil
		if err := tx.UpdateReconciliationRunLease(ctx, cur); err != nil {
			return err
		}
		run = cur
		return tx.Audit(ctx, audit.New(tenantID, s.clock.Now(), reconciliationActor, "reconciliation.run_released", "reconciliation_run",
			cur.ID.String(), map[string]any{"attempt": cur.Attempts}))
	})
	return run, err
}

// PendingRunsResult resume uma passada do executor de conciliações.
type PendingRunsResult struct {
	Completed int
	Failed    int
	Released  int
}

// RunPendingReconciliations assume e executa, uma a uma, as execuções de
// conciliação registradas que aguardam um worker ou cujo lease expirou
// (worker interrompido), de todos os tenants, das mais antigas para as mais
// novas. É a tarefa com lease da conciliação manual: a execução registrada é
// o próprio registro da tarefa, e várias instâncias podem chamar este método
// ao mesmo tempo (cada execução é assumida por uma só). Cada execução é
// tentada no máximo uma vez por chamada; a falha de uma não impede as demais.
func (s *Service) RunPendingReconciliations(ctx context.Context) (PendingRunsResult, error) {
	var (
		res  PendingRunsResult
		errs []error
	)
	tried := map[uuid.UUID]bool{}
	for {
		if err := ctx.Err(); err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		refs, err := s.store.ClaimableReconciliationRuns(ctx, s.clock.Now(), reconciliationClaimBatch)
		if err != nil {
			return res, errors.Join(append(errs, err)...)
		}
		progressed := false
		for _, ref := range refs {
			if tried[ref.RunID] {
				continue
			}
			tried[ref.RunID], progressed = true, true
			_, outcome, err := s.ExecuteReconciliationRun(ctx, ref.TenantID, ref.RunID)
			if err != nil {
				errs = append(errs, fmt.Errorf("conciliação %s do tenant %s: %w", ref.RunID, ref.TenantID, err))
			}
			switch outcome {
			case RunOutcomeCompleted:
				res.Completed++
			case RunOutcomeFailed:
				res.Failed++
			case RunOutcomeReleased:
				res.Released++
			}
			if ctx.Err() != nil {
				break
			}
		}
		if !progressed {
			return res, errors.Join(errs...)
		}
	}
}

// collectAndCompare busca os dois lados e compara. As chamadas ao provedor
// respeitam o prazo da coleta (budget); esgotado o prazo, a coleta para e a
// comparação usa o que foi obtido, com a execução marcada como parcial.
// Cobranças locais não comparadas (fora da listagem e sem consulta
// individual) não geram divergência nem têm divergências resolvidas.
func (s *Service) collectAndCompare(ctx context.Context, budget time.Duration, tenantID uuid.UUID, account ProviderAccount,
	run ReconciliationRun) (runResult, error) {
	gw, err := s.resolver.Gateway(account)
	if err != nil {
		return runResult{}, &providerStageError{stage: "configuração do provedor", err: err}
	}
	collect, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// expired informa se o prazo da coleta acabou sem o cancelamento de quem
	// chamou: a falha da chamada em andamento é do prazo, não do provedor.
	expired := func() bool { return collect.Err() != nil && ctx.Err() == nil }
	var res runResult
	var provider []ProviderCharge
	for page := 0; ; page++ {
		if page == maxProviderPages {
			res.markPartial(partialProviderPages)
			break
		}
		if expired() {
			res.markPartial(partialBudget)
			break
		}
		callCtx, cancel := context.WithTimeout(collect, s.cfg.CallTimeout)
		lr, err := gw.ListCharges(callCtx, ListRequest{UpdatedFrom: run.WindowStart, UpdatedTo: run.WindowEnd, Offset: page * providerPageSize,
			Limit: providerPageSize})
		cancel()
		if err != nil && expired() {
			res.markPartial(partialBudget)
			break
		}
		if err != nil {
			return runResult{}, &providerStageError{stage: "listagem do provedor", err: err}
		}
		provider = append(provider, lr.Charges...)
		if !lr.HasMore || len(lr.Charges) == 0 {
			break
		}
	}
	listed := map[string]bool{}
	var pids []string
	var refs []uuid.UUID
	for _, p := range provider {
		listed[p.ProviderChargeID] = true
		pids = append(pids, p.ProviderChargeID)
		if ref, err := uuid.Parse(p.ExternalReference); err == nil {
			refs = append(refs, ref)
		}
	}
	local, rotation, unknown, err := s.loadLocalSide(ctx, tenantID, account, run, pids, refs, &res)
	if err != nil {
		return runResult{}, err
	}
	missing := map[string]bool{}
	// lookup consulta a cobrança no provedor. Devolve false, sem erro, quando
	// o prazo da coleta acabou antes ou durante a chamada.
	lookup := func(pid string) (bool, error) {
		if expired() {
			return false, nil
		}
		callCtx, cancel := context.WithTimeout(collect, s.cfg.CallTimeout)
		pc, err := gw.GetCharge(callCtx, pid)
		cancel()
		switch {
		case GatewayErrorKind(err) == ErrKindNotFound:
			missing[pid] = true
		case err != nil && expired():
			return false, nil
		case err != nil:
			return false, &providerStageError{stage: "consulta da cobrança " + pid + " no provedor", err: err}
		default:
			listed[pid] = true
			provider = append(provider, pc)
		}
		return true, nil
	}
	lookups := 0
	for _, l := range local {
		pid := l.Charge.ProviderChargeID
		if pid == "" || listed[pid] || missing[pid] {
			continue
		}
		if lookups == maxIndividualLookups {
			res.markPartial(partialLookups)
			break
		}
		lookups++
		ok, err := lookup(pid)
		if err != nil {
			return runResult{}, err
		}
		if !ok {
			res.markPartial(partialBudget)
			break
		}
	}
	s.rotate(ctx, run, rotation, listed, missing, lookup, &res)
	res.compare = Compare(CompareInput{ProviderCharges: provider, Local: append(local, rotation...), MissingAtProvider: missing, UnknownOps: unknown,
		Now: s.clock.Now()})
	return res, nil
}

// rotate consulta as cobranças do rodízio (fora da janela e que ainda podem
// mudar no provedor, por exemplo com chargeback ou reembolso tardio e o
// webhook perdido) em melhor esforço: não marca a execução como parcial nem
// a faz falhar, e a janela já verificada é concluída normalmente. A consulta
// que falha é pulada (a cobrança não é comparada nem registrada como
// verificada e volta na próxima execução). Interrompem o rodízio o prazo da
// coleta esgotado, o cancelamento de quem chamou e rotationMaxFailures falhas
// seguidas que indicam provedor indisponível (recusa definitiva, própria da
// cobrança consultada, não conta); o que não foi consultado fica para as
// próximas execuções.
func (s *Service) rotate(ctx context.Context, run ReconciliationRun, rotation []LocalCharge, listed, missing map[string]bool,
	lookup func(string) (bool, error), res *runResult) {
	unavailable := 0
	for _, l := range rotation {
		pid := l.Charge.ProviderChargeID
		if listed[pid] || missing[pid] {
			continue
		}
		ok, err := lookup(pid)
		switch {
		case ctx.Err() != nil, err == nil && !ok:
			return
		case err == nil:
			unavailable = 0
			continue
		}
		res.rotationFailures++
		if s.cfg.Log != nil {
			s.cfg.Log.WarnContext(ctx, "consulta do rodízio da conciliação falhou", slog.String("run_id", run.ID.String()),
				slog.String("charge_id", l.Charge.ID.String()), slog.String("error", err.Error()))
		}
		if GatewayErrorKind(err) == ErrKindPermanent {
			continue
		}
		if unavailable++; unavailable == rotationMaxFailures {
			return
		}
	}
}

// loadLocalSide lê, numa transação, as cobranças locais da janela e as
// presentes na listagem do provedor, as cobranças do rodízio (fora da janela,
// sem repetir as já lidas) com seus reembolsos e as operações com resultado
// desconhecido. Limites atingidos marcam a execução como parcial.
func (s *Service) loadLocalSide(ctx context.Context, tenantID uuid.UUID, account ProviderAccount, run ReconciliationRun, pids []string,
	refs []uuid.UUID, res *runResult) (local, rotation []LocalCharge, unknown []Operation, err error) {
	var localCut, unknownCut bool
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		local, rotation, unknown, localCut, unknownCut = nil, nil, nil, false, false
		now := s.clock.Now()
		charges, err := tx.ChargesForReconciliation(ctx, ReconciliationScope{AccountID: account.ID, WindowStart: run.WindowStart,
			WindowEnd: run.WindowEnd, ProviderChargeIDs: pids, ReferenceIDs: refs, Limit: maxLocalCharges + 1})
		if err != nil {
			return err
		}
		if len(charges) > maxLocalCharges {
			charges, localCut = charges[:maxLocalCharges], true
		}
		extra, err := tx.ChargesForRotation(ctx, RotationScope{AccountID: account.ID, Since: now.Add(-rotationSpan), WindowStart: run.WindowStart,
			WindowEnd: run.WindowEnd, Limit: rotationLookups})
		if err != nil {
			return err
		}
		seen := make(map[uuid.UUID]bool, len(charges))
		ids := make([]uuid.UUID, 0, len(charges)+len(extra))
		for _, c := range charges {
			seen[c.ID] = true
			ids = append(ids, c.ID)
		}
		var rotated []Charge
		for _, c := range extra {
			if !seen[c.ID] {
				seen[c.ID] = true
				rotated = append(rotated, c)
				ids = append(ids, c.ID)
			}
		}
		refunds, err := tx.RefundsForCharges(ctx, ids)
		if err != nil {
			return err
		}
		byCharge := map[uuid.UUID][]Refund{}
		for _, r := range refunds {
			byCharge[r.ChargeID] = append(byCharge[r.ChargeID], r)
		}
		local = make([]LocalCharge, 0, len(charges))
		for _, c := range charges {
			local = append(local, LocalCharge{Charge: c, Refunds: byCharge[c.ID]})
		}
		for _, c := range rotated {
			rotation = append(rotation, LocalCharge{Charge: c, Refunds: byCharge[c.ID]})
		}
		if unknown, err = tx.UnknownOperations(ctx, now, maxUnknownOperations+1); err != nil {
			return err
		}
		if len(unknown) > maxUnknownOperations {
			unknown, unknownCut = unknown[:maxUnknownOperations], true
		}
		return nil
	})
	if localCut {
		res.markPartial(partialLocalCharges)
	}
	if unknownCut {
		res.unknownTruncated = true
		res.markPartial(partialUnknownOps)
	}
	return local, rotation, unknown, err
}

type runResult struct {
	compare CompareResult
	// partial reúne os motivos de execução parcial (limites atingidos).
	partial []string
	// unknownTruncated indica que a leitura das operações com resultado
	// desconhecido atingiu o limite.
	unknownTruncated bool
	// rotationFailures conta as consultas do rodízio que falharam (não
	// tornam a execução parcial).
	rotationFailures int
}

func (r *runResult) markPartial(reason string) {
	if !slices.Contains(r.partial, reason) {
		r.partial = append(r.partial, reason)
	}
}

// providerStageError identifica a etapa da conciliação em que a chamada ao
// provedor falhou.
type providerStageError struct {
	stage string
	err   error
}

func (e *providerStageError) Error() string { return e.stage + ": " + e.err.Error() }

func (e *providerStageError) Unwrap() error { return e.err }

// runFailureMessage devolve o texto da falha que pode ser gravado na execução
// e exibido a quem consulta a conciliação: a etapa com a mensagem saneada do
// provedor (GatewayError não inclui a causa interna) ou a mensagem do erro de
// aplicação. Demais erros (banco, infraestrutura) recebem texto genérico; o
// detalhe vai somente para o log.
func runFailureMessage(err error, runID uuid.UUID) string {
	var ae *apperr.Error
	var stage *providerStageError
	if errors.As(err, &stage) {
		var ge *GatewayError
		if errors.As(stage.err, &ge) {
			return stage.stage + ": " + ge.Error()
		}
		return stage.stage + ": falha na comunicação com o provedor"
	}
	if errors.As(err, &ae) {
		return ae.Message
	}
	return "Falha interna ao executar a conciliação; o detalhe foi registrado no log da execução " + runID.String() + "."
}

// failRun encerra a execução como falha (provedor indisponível, por exemplo).
// A execução e a auditoria guardam somente a mensagem segura da falha. Não
// faz nada se a execução já foi encerrada ou retomada por outra instância.
func (s *Service) failRun(ctx context.Context, tenantID uuid.UUID, run ReconciliationRun, cause error) (ReconciliationRun, error) {
	if s.cfg.Log != nil {
		s.cfg.Log.WarnContext(ctx, "conciliação encerrada com falha", slog.String("run_id", run.ID.String()),
			slog.String("error", cause.Error()))
	}
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cur, err := tx.ReconciliationRun(ctx, run.ID, true)
		if err != nil {
			return err
		}
		if !cur.ownedBy(run) {
			run = cur
			return nil
		}
		now := s.clock.Now()
		cur.Status, cur.Error, cur.FinishedAt, cur.LeaseUntil = RunFailed, truncate(runFailureMessage(cause, cur.ID), 1000), &now, nil
		if err := tx.FinishReconciliationRun(ctx, cur); err != nil {
			return err
		}
		run = cur
		return tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.run_failed", "reconciliation_run", cur.ID.String(),
			map[string]any{"error": cur.Error}))
	})
	if err != nil {
		return run, err
	}
	return run, nil
}

// completeRun registra as divergências (deduplicadas pela chave), resolve
// automaticamente as divergências abertas não constatadas nesta execução e
// encerra a execução, tudo numa transação: uma execução interrompida antes
// dela não deixa divergência gravada, e a retomada não duplica nenhuma.
// Divergência repetida já resolvida manualmente não é reaberta: a resolução
// cobre exatamente os mesmos valores. Só conclui a tentativa que ainda detém
// a execução (mesma contagem de tentativas); a de uma instância cujo lease
// expirou e foi retomada por outra é recusada.
func (s *Service) completeRun(ctx context.Context, tenantID uuid.UUID, run ReconciliationRun, res runResult) (ReconciliationRun, error) {
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cur, err := tx.ReconciliationRun(ctx, run.ID, true)
		if err != nil {
			return err
		}
		if !cur.ownedBy(run) {
			return apperr.Conflict("reconciliation_run_interrupted", "A execução foi encerrada ou retomada por outra instância antes da conclusão.")
		}
		now := s.clock.Now()
		found := map[string]bool{}
		count := 0
		for _, f := range res.compare.Findings {
			found[f.DedupeKey] = true
			counted, err := s.recordFinding(ctx, tx, cur, f, now)
			if err != nil {
				return err
			}
			if counted {
				count++
			}
		}
		stale, err := tx.OpenDiscrepanciesForSubjects(ctx, cur.Provider, res.compare.CheckedCharges, res.compare.CheckedProviderIDs)
		if err != nil {
			return err
		}
		for i := range stale {
			d := &stale[i]
			if found[d.DedupeKey] || (d.Kind != DiscrepancyUnknownOperation && !checkedSubject(*d, res.compare)) {
				continue
			}
			if d.Kind == DiscrepancyUnknownOperation && res.unknownTruncated {
				// A operação pode ter ficado fora da leitura limitada: só é
				// resolvida se a situação atual não for mais desconhecida.
				pending, err := operationStillUnknown(ctx, tx, *d, now)
				if err != nil {
					return err
				}
				if pending {
					continue
				}
			}
			d.Status, d.Resolution, d.ResolvedAt, d.LastRunID = DiscrepancyResolved, ResolutionAutoResolved, &now, cur.ID
			d.Justification = "Divergência não constatada na execução de conciliação " + cur.ID.String() + "."
			if err := tx.UpdateDiscrepancy(ctx, d); err != nil {
				return err
			}
			if err := tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.discrepancy_auto_resolved",
				"reconciliation_discrepancy", d.ID.String(), map[string]any{"run_id": cur.ID, "kind": string(d.Kind)})); err != nil {
				return err
			}
		}
		if len(res.compare.CheckedCharges) > 0 {
			if err := tx.MarkChargesReconciled(ctx, res.compare.CheckedCharges, now); err != nil {
				return err
			}
		}
		cur.Status, cur.CheckedCount, cur.DiscrepancyCount, cur.FinishedAt, cur.LeaseUntil = RunCompleted, res.compare.Checked(), count, &now, nil
		if len(res.partial) > 0 {
			cur.Error = "Execução parcial (" + strings.Join(res.partial, "; ") +
				"): nem todas as cobranças e operações do escopo da execução foram verificadas."
		}
		if err := tx.FinishReconciliationRun(ctx, cur); err != nil {
			return err
		}
		run = cur
		return tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.run_completed", "reconciliation_run", cur.ID.String(),
			map[string]any{"checked": cur.CheckedCount, "discrepancies": cur.DiscrepancyCount, "partial": len(res.partial) > 0,
				"partial_reasons": res.partial, "rotation_failures": res.rotationFailures}))
	})
	return run, err
}

// operationStillUnknown informa se a operação da divergência continua com
// resultado desconhecido (situação unknown ou em execução com o lease
// expirado).
func operationStillUnknown(ctx context.Context, tx Tx, d Discrepancy, now time.Time) (bool, error) {
	if d.OperationID == nil {
		return false, nil
	}
	op, err := tx.Operation(ctx, *d.OperationID, false)
	if apperr.IsKind(err, apperr.KindNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return op.Status == OpUnknown || op.Stalled(now), nil
}

// checkedSubject informa se o objeto da divergência foi verificado na execução.
func checkedSubject(d Discrepancy, r CompareResult) bool {
	if d.ChargeID != nil {
		for _, id := range r.CheckedCharges {
			if id == *d.ChargeID {
				return true
			}
		}
	}
	if d.Kind == DiscrepancyMissingLocal {
		for _, pid := range r.CheckedProviderIDs {
			if pid == d.ProviderChargeID {
				return true
			}
		}
	}
	return false
}

// recordFinding grava a divergência nova, atualiza a repetida ou reabre a
// resolvida automaticamente. Devolve se ela conta como aberta na execução.
func (s *Service) recordFinding(ctx context.Context, tx Tx, run ReconciliationRun, f Finding, now time.Time) (bool, error) {
	existing, err := tx.DiscrepancyByKey(ctx, f.DedupeKey)
	if err != nil {
		return false, err
	}
	if existing == nil {
		d := Discrepancy{ID: ids.New(), TenantID: run.TenantID, RunID: run.ID, LastRunID: run.ID, Kind: f.Kind, Status: DiscrepancyOpen,
			DedupeKey: f.DedupeKey, Provider: run.Provider, ChargeID: f.ChargeID, RefundID: f.RefundID, OperationID: f.OperationID,
			ProviderChargeID: f.ProviderChargeID, LocalAmount: f.LocalAmount, ProviderAmount: f.ProviderAmount, LocalValue: f.Local,
			ProviderValue: f.ProviderValue, Occurrences: 1, FirstDetectedAt: now, LastDetectedAt: now, CreatedAt: now, UpdatedAt: now, Version: 1}
		if err := tx.InsertDiscrepancy(ctx, d); err != nil {
			return false, err
		}
		if err := tx.Emit(ctx, outbox.New(run.TenantID, outbox.EventDiscrepancyDetected, "reconciliation_discrepancy", d.ID,
			map[string]any{"discrepancy_id": d.ID, "kind": string(d.Kind), "charge_id": d.ChargeID, "run_id": run.ID}, now)); err != nil {
			return false, err
		}
		return true, tx.Audit(ctx, audit.New(run.TenantID, now, reconciliationActor, "reconciliation.discrepancy_detected",
			"reconciliation_discrepancy", d.ID.String(), map[string]any{"run_id": run.ID, "kind": string(d.Kind), "charge_id": d.ChargeID}))
	}
	d := *existing
	switch {
	case d.Status == DiscrepancyOpen:
	case d.Resolution == ResolutionAutoResolved:
		d.Status, d.Resolution, d.Justification, d.ResolvedAt, d.ResolvedBy = DiscrepancyOpen, "", "", nil, nil
		if err := tx.Audit(ctx, audit.New(run.TenantID, now, reconciliationActor, "reconciliation.discrepancy_reopened",
			"reconciliation_discrepancy", d.ID.String(), map[string]any{"run_id": run.ID, "kind": string(d.Kind)})); err != nil {
			return false, err
		}
	default:
		return false, nil // resolvida ou explicada com os mesmos valores
	}
	d.LastRunID, d.LastDetectedAt, d.Occurrences = run.ID, now, d.Occurrences+1
	d.LocalValue, d.ProviderValue, d.LocalAmount, d.ProviderAmount = f.Local, f.ProviderValue, f.LocalAmount, f.ProviderAmount
	return true, tx.UpdateDiscrepancy(ctx, &d)
}

// RunScheduledReconciliations executa a conciliação dos tenants elegíveis
// (job agendado). Tenant com execução em andamento é ignorado.
func (s *Service) RunScheduledReconciliations(ctx context.Context) (int, error) {
	tenants, err := s.store.TenantsDueReconciliation(ctx, s.clock.Now(), ScheduledReconciliationEvery)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, t := range tenants {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		run, err := s.RunReconciliation(ctx, t, reconciliationActor, nil, RunInput{Trigger: TriggerScheduled})
		switch {
		case apperr.IsKind(err, apperr.KindConflict) || apperr.IsKind(err, apperr.KindForbidden):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("conciliação do tenant %s: %w", t, err))
		case run.Status == RunCompleted:
			n++
		default:
			errs = append(errs, fmt.Errorf("conciliação do tenant %s: %s", t, run.Error))
		}
	}
	return n, errors.Join(errs...)
}

// Discrepancy retorna uma divergência.
func (s *Service) Discrepancy(ctx context.Context, tenantID, id uuid.UUID) (Discrepancy, error) {
	var d Discrepancy
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		d, err = tx.Discrepancy(ctx, id, false)
		return err
	})
	return d, err
}

// ReconciliationRun retorna uma execução.
func (s *Service) ReconciliationRun(ctx context.Context, tenantID, id uuid.UUID) (ReconciliationRun, error) {
	var r ReconciliationRun
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		r, err = tx.ReconciliationRun(ctx, id, false)
		return err
	})
	return r, err
}

// ResolveDiscrepancy resolve uma divergência aberta com responsável,
// justificativa (10 a 2000 caracteres) e ação:
//   - dismiss: registra como explicada (situação dismissed);
//   - keep_local: mantém o registro local;
//   - adjust_ledger: lança ReconciliationAdjustment pela diferença de valor
//     ou de taxa, sem alterar a cobrança;
//   - accept_provider: adota o provedor (taxa com ajuste no ledger, valor
//     pago com ajuste, pagamento não refletido aplicado, reembolso ou
//     chargeback sincronizados, operação desconhecida reconsultada, cobrança
//     inexistente no provedor cancelada localmente).
//
// Consultas ao provedor ocorrem antes da transação; a divergência é
// bloqueada antes da cobrança.
func (s *Service) ResolveDiscrepancy(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in ResolveInput) (Discrepancy, error) {
	in, err := validateResolve(in)
	if err != nil {
		return Discrepancy{}, err
	}
	var (
		d       Discrepancy
		account ProviderAccount
		charge  *Charge
	)
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		var err error
		if d, err = tx.Discrepancy(ctx, in.ID, false); err != nil {
			return err
		}
		if d.Status != DiscrepancyOpen {
			return apperr.Conflict("discrepancy_not_open", "A divergência já foi resolvida.")
		}
		if !resolutionApplies(d.Kind, in.Resolution) {
			return errNotApplicable(d.Kind, in.Resolution)
		}
		if in.Resolution != ResolutionAcceptProvider || d.ChargeID == nil {
			return nil
		}
		c, err := tx.Charge(ctx, *d.ChargeID, false)
		if err != nil {
			return err
		}
		charge = &c
		account, err = tx.Account(ctx, c.ProviderAccountID)
		return err
	})
	if err != nil {
		return Discrepancy{}, err
	}
	if in.Resolution == ResolutionAcceptProvider && d.Kind == DiscrepancyUnknownOperation {
		if err := s.reconsultOperation(ctx, tenantID, d); err != nil {
			return Discrepancy{}, err
		}
	}
	var snapshot *ProviderCharge
	var notFound bool
	if in.Resolution == ResolutionAcceptProvider && needsProviderSnapshot(d.Kind) && charge != nil && charge.ProviderChargeID != "" {
		gw, err := s.resolver.Gateway(account)
		if err != nil {
			return Discrepancy{}, err
		}
		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		pc, err := gw.GetCharge(callCtx, charge.ProviderChargeID)
		cancel()
		switch {
		case GatewayErrorKind(err) == ErrKindNotFound:
			notFound = true
		case err != nil:
			return Discrepancy{}, apperr.Unavailable("provider_unavailable", "Não foi possível consultar o provedor; tente novamente.").Wrap(err)
		default:
			snapshot = &pc
		}
	}
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cur, err := tx.Discrepancy(ctx, in.ID, true)
		if err != nil {
			return err
		}
		if cur.Status != DiscrepancyOpen {
			return apperr.Conflict("discrepancy_not_open", "A divergência já foi resolvida.")
		}
		now := s.clock.Now()
		if err := s.applyResolution(ctx, tx, &cur, in.Resolution, snapshot, notFound, actor, now); err != nil {
			return err
		}
		cur.Status, cur.Resolution, cur.Justification, cur.ResolvedBy, cur.ResolvedAt = DiscrepancyResolved, in.Resolution, in.Justification, userID, &now
		if in.Resolution == ResolutionDismiss {
			cur.Status = DiscrepancyDismissed
		}
		if err := tx.UpdateDiscrepancy(ctx, &cur); err != nil {
			return err
		}
		d = cur
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "reconciliation.discrepancy_resolved", "reconciliation_discrepancy", cur.ID.String(),
			map[string]any{"kind": string(cur.Kind), "resolution": string(cur.Resolution), "justification": cur.Justification,
				"charge_id": cur.ChargeID, "ledger_transaction_id": cur.LedgerTransactionID}))
	})
	return d, err
}

func errNotApplicable(kind DiscrepancyKind, r Resolution) error {
	return apperr.Conflict("resolution_not_applicable", fmt.Sprintf("A resolução %s não se aplica a divergências do tipo %s.", r, kind))
}

func needsProviderSnapshot(kind DiscrepancyKind) bool {
	switch kind {
	case DiscrepancyPaymentNotReflected, DiscrepancyRefundPending, DiscrepancyChargebackPending, DiscrepancyMissingProvider:
		return true
	}
	return false
}

// applyResolution aplica o efeito da resolução na transação corrente.
func (s *Service) applyResolution(ctx context.Context, tx Tx, d *Discrepancy, r Resolution, snapshot *ProviderCharge, notFound bool, actor audit.Actor, now time.Time) error {
	switch r {
	case ResolutionDismiss, ResolutionKeepLocal:
		return nil
	case ResolutionAdjustLedger:
		return s.postAdjustment(ctx, tx, d, now)
	}
	switch d.Kind {
	case DiscrepancyFeeMismatch:
		c, err := tx.Charge(ctx, *d.ChargeID, true)
		if err != nil {
			return err
		}
		if d.ProviderAmount == nil || !IsSettledStatus(c.Status) {
			return errNotApplicable(d.Kind, r)
		}
		if err := s.postAdjustment(ctx, tx, d, now); err != nil {
			return err
		}
		c.Fee = *d.ProviderAmount
		return tx.UpdateCharge(ctx, &c)
	case DiscrepancyAmountMismatch:
		if d.LocalValue["basis"] != "paid" {
			return errNotApplicable(d.Kind, r)
		}
		return s.postAdjustment(ctx, tx, d, now)
	case DiscrepancyUnknownOperation:
		op, err := tx.Operation(ctx, *d.OperationID, false)
		if err != nil {
			return err
		}
		if op.Status == OpUnknown || op.Status == OpInFlight || op.Status == OpPending {
			return apperr.Conflict("operation_still_unknown", "A consulta ao provedor não resolveu a operação; tente novamente mais tarde.")
		}
		return nil
	case DiscrepancyMissingProvider:
		if !notFound {
			return apperr.Conflict("provider_charge_found", "A cobrança foi encontrada no provedor; execute a conciliação novamente.")
		}
		c, err := tx.Charge(ctx, *d.ChargeID, true)
		if err != nil {
			return err
		}
		if c.Status != ChargePending && c.Status != ChargeAwaitingPayment {
			return errNotApplicable(d.Kind, r)
		}
		if err := c.MarkCanceled(now); err != nil {
			return err
		}
		if err := tx.UpdateCharge(ctx, &c); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(c.TenantID, now, actor, "charge.canceled_missing_at_provider", "charge", c.ID.String(),
			map[string]any{"discrepancy_id": d.ID, "provider_charge_id": c.ProviderChargeID}))
	}
	// Pagamento não refletido, reembolso e chargeback: adota o retrato
	// atual do provedor.
	if snapshot == nil {
		return apperr.Conflict("provider_charge_not_found", "A cobrança não foi encontrada no provedor.")
	}
	c, err := tx.Charge(ctx, *d.ChargeID, true)
	if err != nil {
		return err
	}
	if d.Kind == DiscrepancyPaymentNotReflected && (!snapshot.Status.IsPaid() || IsSettledStatus(c.Status)) {
		return apperr.Conflict("payment_not_applicable", "O provedor não informa pagamento a aplicar nesta cobrança.")
	}
	if err := s.applyProviderStateFrom(ctx, tx, &c, *snapshot, now, actor, true); err != nil {
		return err
	}
	if d.Kind == DiscrepancyRefundPending && d.RefundID != nil {
		return s.closeUnconfirmedRefund(ctx, tx, &c, *d.RefundID, *snapshot, actor, now)
	}
	return s.requireConverged(ctx, tx, c, *snapshot, d.Kind, now)
}

// requireConverged recusa a adoção do provedor que não eliminou a
// divergência da cobrança (por exemplo, reembolso efetivado no provedor que
// não cabe no saldo elegível, ou situação de chargeback que não abre nem
// encerra a disputa): marcar a divergência como resolvida sem efeito a
// esconderia das execuções seguintes, que não reabrem resoluções manuais.
func (s *Service) requireConverged(ctx context.Context, tx Tx, c Charge, pc ProviderCharge, kind DiscrepancyKind, now time.Time) error {
	refunds, err := tx.ChargeRefunds(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, f := range compareCharge(LocalCharge{Charge: c, Refunds: refunds}, pc, now) {
		if f.Kind == kind && f.RefundID == nil {
			return apperr.Conflict("provider_state_not_applied",
				"O retrato atual do provedor não elimina a divergência; verifique a cobrança no provedor ou resolva mantendo o registro local.")
		}
	}
	return nil
}

// closeUnconfirmedRefund encerra o reembolso que o provedor não registra:
// sem evidência no retrato atual e sem operação em execução dentro do lease,
// o reembolso não foi executado e a reserva é liberada. Operação que ficou
// em execução depois do lease (processo interrompido) não impede o
// encerramento.
func (s *Service) closeUnconfirmedRefund(ctx context.Context, tx Tx, c *Charge, refundID uuid.UUID, pc ProviderCharge, actor audit.Actor, now time.Time) error {
	r, err := tx.Refund(ctx, refundID, true)
	if err != nil || !r.Open() {
		return err
	}
	refunds, err := tx.ChargeRefunds(ctx, c.ID)
	if err != nil {
		return err
	}
	ops, err := tx.ChargeOperations(ctx, c.ID, OpRefund)
	if err != nil {
		return err
	}
	dispatched := map[uuid.UUID]bool{}
	byRefund := map[uuid.UUID]Operation{}
	for _, o := range ops {
		if o.RefundID != nil {
			byRefund[*o.RefundID] = o
			dispatched[*o.RefundID] = o.Attempts > 0
		}
	}
	if op, ok := byRefund[r.ID]; ok && op.Running(now) {
		return apperr.Conflict("refund_in_flight", "O reembolso está em execução no provedor; tente novamente em instantes.")
	}
	if planRefundSync(pc, refunds, dispatched).covers(r.ID) {
		return apperr.Conflict("refund_pending_at_provider", "O provedor ainda processa o reembolso; aguarde a confirmação.")
	}
	if err := s.failRefund(ctx, tx, c, &r, "não registrado no provedor (resolução da conciliação)", now, actor); err != nil {
		return err
	}
	if err := s.finishRefundOperation(ctx, tx, byRefund, r, now); err != nil {
		return err
	}
	return tx.UpdateCharge(ctx, c)
}

// postAdjustment lança o ajuste de conciliação pela diferença registrada:
// taxa do provedor menos a local, ou valor pago local menos o do provedor.
// Diferença positiva reconhece despesa contra valores em liquidação.
func (s *Service) postAdjustment(ctx context.Context, tx Tx, d *Discrepancy, now time.Time) error {
	if d.LocalAmount == nil || d.ProviderAmount == nil {
		return errNotApplicable(d.Kind, ResolutionAdjustLedger)
	}
	var diff money.Money
	var err error
	switch d.Kind {
	case DiscrepancyFeeMismatch:
		diff, err = d.ProviderAmount.Sub(*d.LocalAmount)
	case DiscrepancyAmountMismatch:
		if d.LocalValue["basis"] != "paid" {
			return errNotApplicable(d.Kind, ResolutionAdjustLedger)
		}
		diff, err = d.LocalAmount.Sub(*d.ProviderAmount)
	default:
		return errNotApplicable(d.Kind, ResolutionAdjustLedger)
	}
	if err != nil {
		return err
	}
	t, ok, err := ledger.ReconciliationAdjustment(ids.New(), d.TenantID, d.ID, diff, now)
	if err != nil || !ok {
		return err
	}
	if err := tx.PostLedger(ctx, t); err != nil {
		return err
	}
	d.LedgerTransactionID = &t.ID
	return nil
}

// reconsultOperation tenta resolver a operação com resultado desconhecido
// consultando o provedor (mesmo caminho da recuperação, que nunca repete a
// operação sem consulta). Operação que ficou em execução depois do lease
// (processo interrompido durante a chamada) é tratada como desconhecida.
// Operação que esgotou as tentativas automáticas ganha uma nova consulta.
func (s *Service) reconsultOperation(ctx context.Context, tenantID uuid.UUID, d Discrepancy) error {
	if d.OperationID == nil {
		return errNotApplicable(d.Kind, ResolutionAcceptProvider)
	}
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		op, err := tx.Operation(ctx, *d.OperationID, true)
		if err != nil {
			return err
		}
		now := s.clock.Now()
		switch {
		case op.Status == OpUnknown:
		case op.Stalled(now):
			op.Status, op.LeaseUntil = OpUnknown, nil
		default:
			return nil
		}
		op.NextAttemptAt, op.UpdatedAt = now, now
		if op.Attempts >= MaxOperationAttempts {
			op.Attempts = MaxOperationAttempts - 1
		}
		return tx.UpdateOperation(ctx, op)
	})
	if err != nil {
		return err
	}
	_, err = s.executeOperation(ctx, tenantID, *d.OperationID, execRecovery)
	return err
}
