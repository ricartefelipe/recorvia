package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// reconciliationLease limita a duração de uma execução; depois dele,
	// outra execução pode encerrá-la como interrompida.
	reconciliationLease   = 15 * time.Minute
	providerPageSize      = 100
	maxProviderPages      = 20
	maxIndividualLookups  = 100
	maxLocalCharges       = 5000
	maxUnknownOperations  = 500
	maxReconciliationSpan = 93 * 24 * time.Hour
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

// RunReconciliation executa a conciliação do provedor padrão do tenant sobre
// a janela: lista as cobranças do provedor, consulta individualmente as
// locais ausentes da listagem, compara e registra divergências. As chamadas
// ao provedor ocorrem sem transação aberta. Execução manual é sempre
// permitida; a agendada exige o recurso reconciliation_automation do plano
// da plataforma. Há no máximo uma execução em andamento por conta.
func (s *Service) RunReconciliation(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput) (ReconciliationRun, error) {
	st, err := s.StartReconciliation(ctx, tenantID, actor, userID, in)
	if err != nil || st.Existing {
		return st.Run, err
	}
	return s.FinishReconciliation(ctx, tenantID, st)
}

// StartedRun é uma execução registrada pela StartReconciliation.
type StartedRun struct {
	Run ReconciliationRun
	// Existing indica execução já registrada para a mesma chave do pedido;
	// não há o que concluir.
	Existing bool
	account  ProviderAccount
}

// StartReconciliation valida o pedido e registra a execução em andamento
// (ou devolve a já registrada para a mesma chave). A conclusão
// (FinishReconciliation) pode ocorrer fora da requisição que a iniciou.
func (s *Service) StartReconciliation(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput) (StartedRun, error) {
	if in.Trigger != TriggerScheduled {
		in.Trigger = TriggerManual
	}
	start, end, err := s.runWindow(in)
	if err != nil {
		return StartedRun{}, err
	}
	run, account, existing, err := s.startRun(ctx, tenantID, actor, userID, in, start, end)
	if err != nil {
		return StartedRun{}, err
	}
	return StartedRun{Run: run, Existing: existing, account: account}, nil
}

// startRun registra a execução em andamento numa única transação: devolve a
// execução já registrada para a mesma chave do pedido, exige o recurso
// reconciliation_automation na execução agendada, encerra como falha as
// execuções da conta com lease expirado e grava a nova execução com lease. O
// índice único parcial das execuções em andamento recusa a segunda execução
// simultânea da mesma conta (reconciliation_running).
func (s *Service) startRun(ctx context.Context, tenantID uuid.UUID, actor audit.Actor, userID *uuid.UUID, in RunInput,
	start, end time.Time) (ReconciliationRun, ProviderAccount, bool, error) {
	var (
		run      ReconciliationRun
		account  ProviderAccount
		existing bool
	)
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
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
		stale, err := tx.FailStaleReconciliationRuns(ctx, account.ID, now)
		if err != nil {
			return err
		}
		lease := now.Add(reconciliationLease)
		run = ReconciliationRun{ID: ids.New(), TenantID: tenantID, ProviderAccountID: account.ID, Provider: account.Provider,
			Trigger: in.Trigger, Status: RunRunning, WindowStart: start, WindowEnd: end, RequestedBy: userID, RequestKey: in.RequestKey,
			LeaseUntil: &lease, StartedAt: now}
		if err := tx.InsertReconciliationRun(ctx, run); err != nil {
			return err
		}
		return tx.Audit(ctx, audit.New(tenantID, now, actor, "reconciliation.run_started", "reconciliation_run", run.ID.String(),
			map[string]any{"trigger": string(run.Trigger), "provider": run.Provider, "window_start": start, "window_end": end,
				"stale_runs_failed": stale}))
	})
	if err != nil {
		return ReconciliationRun{}, ProviderAccount{}, false, err
	}
	return run, account, existing, nil
}

// FinishReconciliation coleta, compara e encerra a execução iniciada. A
// coleta dura no máximo o lease da execução; falha do provedor ou da leitura
// encerra a execução como failed (sem erro devolvido).
func (s *Service) FinishReconciliation(ctx context.Context, tenantID uuid.UUID, st StartedRun) (ReconciliationRun, error) {
	if st.Existing {
		return st.Run, nil
	}
	run := st.Run
	callCtx, cancel := context.WithTimeout(ctx, reconciliationLease)
	result, err := s.collectAndCompare(callCtx, tenantID, st.account, run)
	cancel()
	if err != nil {
		return s.failRun(context.WithoutCancel(ctx), tenantID, run, err)
	}
	done, err := s.completeRun(context.WithoutCancel(ctx), tenantID, run, result)
	if err != nil && !apperr.IsKind(err, apperr.KindConflict) {
		if _, ferr := s.failRun(context.WithoutCancel(ctx), tenantID, run, err); ferr != nil {
			return run, errors.Join(err, ferr)
		}
	}
	return done, err
}

// collectAndCompare busca os dois lados e compara.
func (s *Service) collectAndCompare(ctx context.Context, tenantID uuid.UUID, account ProviderAccount, run ReconciliationRun) (runResult, error) {
	gw, err := s.resolver.Gateway(account)
	if err != nil {
		return runResult{}, &providerStageError{stage: "configuração do provedor", err: err}
	}
	var res runResult
	var provider []ProviderCharge
	for page := 0; ; page++ {
		if page == maxProviderPages {
			res.truncated = true
			break
		}
		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		lr, err := gw.ListCharges(callCtx, ListRequest{UpdatedFrom: run.WindowStart, UpdatedTo: run.WindowEnd, Offset: page * providerPageSize,
			Limit: providerPageSize})
		cancel()
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
	var local []LocalCharge
	var unknown []Operation
	err = s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		charges, err := tx.ChargesForReconciliation(ctx, ReconciliationScope{AccountID: account.ID, WindowStart: run.WindowStart,
			WindowEnd: run.WindowEnd, ProviderChargeIDs: pids, ReferenceIDs: refs, Limit: maxLocalCharges})
		if err != nil {
			return err
		}
		chargeIDs := make([]uuid.UUID, 0, len(charges))
		for _, c := range charges {
			chargeIDs = append(chargeIDs, c.ID)
		}
		refunds, err := tx.RefundsForCharges(ctx, chargeIDs)
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
		unknown, err = tx.UnknownOperations(ctx, s.clock.Now(), maxUnknownOperations)
		return err
	})
	if err != nil {
		return runResult{}, err
	}
	missing := map[string]bool{}
	lookups := 0
	for _, l := range local {
		pid := l.Charge.ProviderChargeID
		if pid == "" || listed[pid] {
			continue
		}
		if lookups == maxIndividualLookups {
			res.truncated = true
			break
		}
		lookups++
		callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
		pc, err := gw.GetCharge(callCtx, pid)
		cancel()
		switch {
		case GatewayErrorKind(err) == ErrKindNotFound:
			missing[pid] = true
		case err != nil:
			return runResult{}, &providerStageError{stage: "consulta da cobrança " + pid + " no provedor", err: err}
		default:
			listed[pid] = true
			provider = append(provider, pc)
		}
	}
	res.compare = Compare(CompareInput{ProviderCharges: provider, Local: local, MissingAtProvider: missing, UnknownOps: unknown, Now: s.clock.Now()})
	return res, nil
}

type runResult struct {
	compare   CompareResult
	truncated bool
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
// A execução e a auditoria guardam somente a mensagem segura da falha.
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
		if cur.Status != RunRunning {
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
// encerra a execução. Divergência repetida já resolvida manualmente não é
// reaberta: a resolução cobre exatamente os mesmos valores.
func (s *Service) completeRun(ctx context.Context, tenantID uuid.UUID, run ReconciliationRun, res runResult) (ReconciliationRun, error) {
	err := s.store.InTenant(ctx, tenantID, func(tx Tx) error {
		cur, err := tx.ReconciliationRun(ctx, run.ID, true)
		if err != nil {
			return err
		}
		if cur.Status != RunRunning {
			return apperr.Conflict("reconciliation_run_interrupted", "A execução foi encerrada por outra instância antes da conclusão.")
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
		cur.Status, cur.CheckedCount, cur.DiscrepancyCount, cur.FinishedAt, cur.LeaseUntil = RunCompleted, res.compare.Checked(), count, &now, nil
		if res.truncated {
			cur.Error = "Execução parcial: limite de consultas ao provedor atingido; nem todas as cobranças da janela foram verificadas."
		}
		if err := tx.FinishReconciliationRun(ctx, cur); err != nil {
			return err
		}
		run = cur
		return tx.Audit(ctx, audit.New(tenantID, now, reconciliationActor, "reconciliation.run_completed", "reconciliation_run", cur.ID.String(),
			map[string]any{"checked": cur.CheckedCount, "discrepancies": cur.DiscrepancyCount, "partial": res.truncated}))
	})
	return run, err
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
	_, err = s.executeOperation(ctx, tenantID, *d.OperationID)
	return err
}
