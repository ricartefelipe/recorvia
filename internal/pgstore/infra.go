package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
)

// --- ledger (consulta) ---

// Ledger implementa ledger.Reader.
type Ledger struct{ s *Store }

// Ledger retorna o leitor do ledger.
func (s *Store) Ledger() *Ledger { return &Ledger{s: s} }

// Balances calcula os saldos por conta.
func (l *Ledger) Balances(ctx context.Context, tenantID uuid.UUID) ([]ledger.AccountBalance, error) {
	var out []ledger.AccountBalance
	err := l.s.inTenant(ctx, tenantID, func(b base) error {
		if err := b.ensureLedgerAccounts(ctx); err != nil {
			return err
		}
		rows, err := b.q.LedgerBalances(ctx, tenantID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			cur, err := money.ParseCurrency(r.Currency)
			if err != nil {
				return err
			}
			kind := ledger.AccountKind(r.Kind)
			d, c := money.New(r.Debits, cur), money.New(r.Credits, cur)
			bal, err := ledger.NaturalBalance(kind, d, c)
			if err != nil {
				return err
			}
			out = append(out, ledger.AccountBalance{Code: ledger.AccountCode(r.Code), Name: r.Name, Kind: kind, Debits: d, Credits: c, Balance: bal})
		}
		return nil
	})
	return out, err
}

// Transactions lista lançamentos com partidas.
func (l *Ledger) Transactions(ctx context.Context, tenantID uuid.UUID, refType string, refID *uuid.UUID, limit int, cursor string) ([]ledger.TransactionView, string, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []ledger.TransactionView
	next := ""
	err = l.s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListLedgerTransactions(ctx, dbgen.ListLedgerTransactionsParams{TenantID: tenantID, ReferenceType: strPtr(refType),
			ReferenceID: refID, CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		if len(rows) > limit {
			next = timeCursor(rows[limit-1].CreatedAt, rows[limit-1].ID)
			rows = rows[:limit]
		}
		txIDs := make([]uuid.UUID, len(rows))
		for i, r := range rows {
			txIDs[i] = r.ID
		}
		entries, err := b.q.ListLedgerEntriesForTransactions(ctx, dbgen.ListLedgerEntriesForTransactionsParams{TenantID: tenantID, Ids: txIDs})
		if err != nil {
			return err
		}
		byTx := map[uuid.UUID][]ledger.Entry{}
		for _, e := range entries {
			cur, err := money.ParseCurrency(e.Currency)
			if err != nil {
				return err
			}
			byTx[e.TransactionID] = append(byTx[e.TransactionID], ledger.Entry{Account: ledger.AccountCode(e.AccountCode),
				Direction: ledger.Direction(e.Direction), Amount: money.New(e.Amount, cur)})
		}
		for _, r := range rows {
			out = append(out, ledger.TransactionView{Transaction: ledger.Transaction{ID: r.ID, TenantID: r.TenantID, Kind: r.Kind,
				Description: r.Description, OccurredAt: r.OccurredAt, ReferenceType: r.ReferenceType, ReferenceID: r.ReferenceID,
				IdempotencyKey: r.IdempotencyKey, ReversesID: r.ReversesTransactionID, Entries: byTx[r.ID]}, CreatedAt: r.CreatedAt})
		}
		return nil
	})
	return out, next, err
}

// --- auditoria (consulta) ---

// AuditFilter filtra a trilha.
type AuditFilter struct {
	ResourceType string
	ResourceID   string
	Action       string
	ActorID      string
	From         *time.Time
	To           *time.Time
	Limit        int
	Cursor       string
}

// AuditEvent é uma entrada para exibição.
type AuditEvent struct {
	ID           uuid.UUID
	OccurredAt   time.Time
	ActorType    string
	ActorID      string
	ActorLabel   string
	Action       string
	ResourceType string
	ResourceID   string
	RequestID    string
	Details      map[string]any
}

// AuditEvents lista a trilha de auditoria do tenant.
func (s *Store) AuditEvents(ctx context.Context, tenantID uuid.UUID, f AuditFilter) ([]AuditEvent, string, error) {
	cts, cid, err := parseTimeCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := pageLimit(f.Limit)
	var out []AuditEvent
	next := ""
	err = s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListAuditEvents(ctx, dbgen.ListAuditEventsParams{TenantID: tenantID, ResourceType: strPtr(f.ResourceType),
			ResourceID: strPtr(f.ResourceID), Action: strPtr(f.Action), ActorID: strPtr(f.ActorID), FromTs: f.From, ToTs: f.To,
			CursorTs: cts, CursorID: cid, RowLimit: int32(limit + 1)})
		if err != nil {
			return err
		}
		for i, r := range rows {
			if i == limit {
				next = timeCursor(rows[i-1].OccurredAt, rows[i-1].ID)
				break
			}
			d, err := jsonMap(r.Details)
			if err != nil {
				return err
			}
			out = append(out, AuditEvent{ID: r.ID, OccurredAt: r.OccurredAt, ActorType: r.ActorType, ActorID: deref(r.ActorID),
				ActorLabel: deref(r.ActorLabel), Action: r.Action, ResourceType: r.ResourceType, ResourceID: deref(r.ResourceID),
				RequestID: deref(r.RequestID), Details: d})
		}
		return nil
	})
	return out, next, err
}

// --- outbox (relay) ---

// OutboxRecord é um evento reivindicado para publicação.
type OutboxRecord struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	EventType     string
	AggregateType string
	AggregateID   uuid.UUID
	Payload       []byte
	Headers       map[string]string
	OccurredAt    time.Time
	Attempts      int
}

// ClaimOutbox reivindica um lote de eventos pendentes com lease.
func (s *Store) ClaimOutbox(ctx context.Context, worker string, batch int, lease time.Duration) ([]OutboxRecord, error) {
	var out []OutboxRecord
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.OutboxClaim(ctx, dbgen.OutboxClaimParams{Worker: worker, BatchSize: int32(batch), LeaseSeconds: int32(lease.Seconds())})
		if err != nil {
			return err
		}
		for _, r := range rows {
			h := map[string]string{}
			if len(r.Headers) > 0 {
				if err := json.Unmarshal(r.Headers, &h); err != nil {
					return err
				}
			}
			out = append(out, OutboxRecord{ID: r.ID, TenantID: r.TenantID, EventType: r.EventType, AggregateType: r.AggregateType,
				AggregateID: r.AggregateID, Payload: r.Payload, Headers: h, OccurredAt: r.OccurredAt, Attempts: int(r.Attempts)})
		}
		return nil
	})
	return out, err
}

// MarkOutboxPublished confirma a publicação dos eventos do lote.
func (s *Store) MarkOutboxPublished(ctx context.Context, worker string, eventIDs []uuid.UUID) (int, error) {
	var n int32
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		n, err = q.OutboxMarkPublished(ctx, dbgen.OutboxMarkPublishedParams{Worker: worker, Ids: eventIDs})
		return err
	})
	return int(n), err
}

// ReleaseOutbox devolve um evento com atraso após falha de publicação.
func (s *Store) ReleaseOutbox(ctx context.Context, worker string, id uuid.UUID, delay time.Duration, cause string) error {
	return s.inGlobal(ctx, func(q *dbgen.Queries) error {
		return q.OutboxRelease(ctx, dbgen.OutboxReleaseParams{Worker: worker, ID: id, DelaySeconds: int32(delay.Seconds()), Error: cause})
	})
}

// OutboxStats retorna tamanho e idade da outbox (métricas).
func (s *Store) OutboxStats(ctx context.Context) (pending int64, oldestSeconds float64, err error) {
	err = s.inGlobal(ctx, func(q *dbgen.Queries) error {
		r, err := q.OutboxStats(ctx)
		pending, oldestSeconds = r.Pending, r.OldestPendingSeconds
		return err
	})
	return pending, oldestSeconds, err
}

// PurgeOutbox remove eventos publicados antigos (retenção).
func (s *Store) PurgeOutbox(ctx context.Context, keepDays int) (int, error) {
	var n int32
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		n, err = q.OutboxPurge(ctx, int32(keepDays))
		return err
	})
	return int(n), err
}

// PaymentOperationStats retorna contagens agregadas de operações pendentes.
func (s *Store) PaymentOperationStats(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		rows, err := q.PaymentOperationStats(ctx)
		for _, r := range rows {
			out[r.Status] = r.Total
		}
		return err
	})
	return out, err
}

// --- agendamentos ---

// ScheduledJob é uma tarefa reivindicada.
type ScheduledJob struct {
	Name     string
	Interval time.Duration
}

// EnsureJob registra (ou atualiza o intervalo de) uma tarefa periódica.
func (s *Store) EnsureJob(ctx context.Context, name string, interval time.Duration) error {
	return s.inGlobal(ctx, func(q *dbgen.Queries) error {
		return q.EnsureScheduledJob(ctx, dbgen.EnsureScheduledJobParams{Name: name, IntervalSeconds: int32(interval.Seconds()), NextRunAt: s.clock.Now()})
	})
}

// ClaimJob reivindica a próxima tarefa vencida com lease. Várias instâncias
// do worker competem com SKIP LOCKED; somente uma executa cada vez.
func (s *Store) ClaimJob(ctx context.Context, worker string, lease time.Duration) (*ScheduledJob, error) {
	var out *ScheduledJob
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		now := s.clock.Now()
		r, err := q.ClaimScheduledJob(ctx, dbgen.ClaimScheduledJobParams{Worker: &worker, LockedUntil: ptrTime(now.Add(lease)), Now: &now})
		if postgres.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		out = &ScheduledJob{Name: r.Name, Interval: time.Duration(r.IntervalSeconds) * time.Second}
		return nil
	})
	return out, err
}

// CompleteJob libera a tarefa e agenda a próxima execução.
func (s *Store) CompleteJob(ctx context.Context, worker, name string, next time.Time, runErr error) error {
	var msg *string
	if runErr != nil {
		m := runErr.Error()
		if len(m) > 1000 {
			m = m[:1000]
		}
		msg = &m
	}
	return s.inGlobal(ctx, func(q *dbgen.Queries) error {
		return q.CompleteScheduledJob(ctx, dbgen.CompleteScheduledJobParams{Now: ptrTime(s.clock.Now()), NextRunAt: next, LastError: msg, Name: name, Worker: &worker})
	})
}

// --- idempotência ---

// IdempotencyRecord é o estado registrado de uma chave.
type IdempotencyRecord struct {
	RequestHash    []byte
	Completed      bool
	LockedUntil    *time.Time
	ResponseStatus int
	ResponseBody   []byte
	ResponseCT     string
	// ResponseLocation é o header Location da resposta registrada (vazio
	// quando a resposta não o trazia).
	ResponseLocation string
	CreatedAt        time.Time
	ExpiresAt        time.Time
}

// IdempotencyKey identifica a chave no escopo de tenant, cliente e operação.
type IdempotencyKey struct {
	TenantID  uuid.UUID
	ClientID  string
	Operation string
	Key       string
}

// IdempotencyBegin tenta registrar a chave em processamento. Retorna
// (nil, true) se este pedido venceu a disputa; caso contrário, o registro
// existente.
func (s *Store) IdempotencyBegin(ctx context.Context, k IdempotencyKey, hash []byte, lease, ttl time.Duration) (*IdempotencyRecord, bool, error) {
	var rec *IdempotencyRecord
	won := false
	err := s.inTenant(ctx, k.TenantID, func(b base) error {
		now := s.clock.Now()
		n, err := b.q.InsertIdempotencyKey(ctx, dbgen.InsertIdempotencyKeyParams{TenantID: k.TenantID, ClientID: k.ClientID, Operation: k.Operation,
			IdempotencyKey: k.Key, RequestHash: hash, LockedUntil: ptrTime(now.Add(lease)), Now: now, ExpiresAt: now.Add(ttl)})
		if err != nil {
			return err
		}
		if n == 1 {
			won = true
			return nil
		}
		r, err := b.q.GetIdempotencyKey(ctx, dbgen.GetIdempotencyKeyParams{TenantID: k.TenantID, ClientID: k.ClientID, Operation: k.Operation, IdempotencyKey: k.Key})
		if err != nil {
			return err
		}
		rec = &IdempotencyRecord{RequestHash: r.RequestHash, Completed: r.Status == "completed", LockedUntil: r.LockedUntil,
			ResponseBody: r.ResponseBody, ResponseCT: deref(r.ResponseContentType), ResponseLocation: deref(r.ResponseLocation),
			CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}
		if r.ResponseStatus != nil {
			rec.ResponseStatus = int(*r.ResponseStatus)
		}
		return nil
	})
	return rec, won, err
}

// IdempotencyTakeOver assume uma chave cujo processamento anterior foi
// interrompido (lease expirado).
func (s *Store) IdempotencyTakeOver(ctx context.Context, k IdempotencyKey, lease time.Duration) (bool, error) {
	var ok bool
	err := s.inTenant(ctx, k.TenantID, func(b base) error {
		now := s.clock.Now()
		n, err := b.q.TakeOverIdempotencyKey(ctx, dbgen.TakeOverIdempotencyKeyParams{LockedUntil: ptrTime(now.Add(lease)), TenantID: k.TenantID,
			ClientID: k.ClientID, Operation: k.Operation, IdempotencyKey: k.Key, Now: &now})
		ok = n == 1
		return err
	})
	return ok, err
}

// maxIdempotencyLocation é o tamanho máximo do header Location guardado com
// o resultado (constraint idempotency_keys_response_location_length).
const maxIdempotencyLocation = 2048

// IdempotencyComplete grava o resultado: status, corpo, Content-Type e o
// header Location da resposta (vazio quando ela não o traz), que a repetição
// reproduz. Um Location acima do limite da coluna não é guardado, para não
// perder o registro do resultado.
func (s *Store) IdempotencyComplete(ctx context.Context, k IdempotencyKey, status int, body []byte, contentType, location string) error {
	return s.inTenant(ctx, k.TenantID, func(b base) error {
		st := int32(status)
		var loc *string
		if location != "" && len(location) <= maxIdempotencyLocation {
			loc = &location
		}
		return b.q.CompleteIdempotencyKey(ctx, dbgen.CompleteIdempotencyKeyParams{ResponseStatus: &st, ResponseBody: body,
			ResponseContentType: &contentType, ResponseLocation: loc, Now: ptrTime(s.clock.Now()), TenantID: k.TenantID, ClientID: k.ClientID,
			Operation: k.Operation, IdempotencyKey: k.Key})
	})
}

// IdempotencyRelease remove a chave em processamento quando o pedido falhou
// sem efeito (por exemplo, validação), permitindo nova tentativa.
func (s *Store) IdempotencyRelease(ctx context.Context, k IdempotencyKey) error {
	return s.inTenant(ctx, k.TenantID, func(b base) error {
		return b.q.ReleaseIdempotencyKey(ctx, dbgen.ReleaseIdempotencyKeyParams{TenantID: k.TenantID, ClientID: k.ClientID, Operation: k.Operation, IdempotencyKey: k.Key})
	})
}

// PurgeIdempotency remove chaves concluídas e expiradas. A expiração não
// elimina as demais proteções (constraints de unicidade de negócio).
func (s *Store) PurgeIdempotency(ctx context.Context) (int, error) {
	var n int32
	err := s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		n, err = q.PurgeExpiredIdempotencyKeys(ctx)
		return err
	})
	return int(n), err
}

// --- inbox genérico para consumidores ---

// InTenantInbox executa fn na transação do tenant somente se a mensagem
// ainda não foi processada pelo consumidor (registro atômico com os efeitos).
func (s *Store) InTenantInbox(ctx context.Context, tenantID uuid.UUID, consumer string, messageID uuid.UUID, fn func(ctx context.Context) error) (bool, error) {
	processed := false
	err := s.inTenant(ctx, tenantID, func(b base) error {
		first, err := b.MarkInbox(ctx, consumer, messageID)
		if err != nil || !first {
			return err
		}
		processed = true
		return fn(ctx)
	})
	return processed, err
}

// --- dead letters ---

// DeadLetter é uma mensagem não processada.
type DeadLetter struct {
	ID             uuid.UUID
	TenantID       *uuid.UUID
	Consumer       string
	MessageID      string
	EventType      string
	Payload        []byte
	Headers        map[string]string
	Error          string
	Attempts       int
	Status         string
	FailedAt       time.Time
	ResolvedAt     *time.Time
	ResolutionNote string
}

// RecordDeadLetter persiste a mensagem rejeitada. Mensagens sem tenant válido
// ficam visíveis apenas para operação da plataforma.
func (s *Store) RecordDeadLetter(ctx context.Context, d DeadLetter) error {
	hdr, err := json.Marshal(d.Headers)
	if err != nil {
		return err
	}
	if d.TenantID == nil {
		return s.inGlobal(ctx, func(q *dbgen.Queries) error {
			return q.RecordUntenantedDeadLetter(ctx, dbgen.RecordUntenantedDeadLetterParams{ID: d.ID, Consumer: d.Consumer, MessageID: d.MessageID,
				EventType: d.EventType, Headers: hdr, Error: d.Error, Attempts: int32(d.Attempts)})
		})
	}
	payload := d.Payload
	if !json.Valid(payload) {
		payload = nil
	}
	return s.inTenant(ctx, *d.TenantID, func(b base) error {
		return b.q.InsertDeadLetter(ctx, dbgen.InsertDeadLetterParams{ID: d.ID, TenantID: d.TenantID, Consumer: d.Consumer, MessageID: d.MessageID,
			EventType: strPtr(d.EventType), Payload: payload, Headers: hdr, Error: d.Error, Attempts: int32(d.Attempts), FailedAt: d.FailedAt})
	})
}

// DeadLetters lista mensagens não processadas do tenant.
func (s *Store) DeadLetters(ctx context.Context, tenantID uuid.UUID, status string) ([]DeadLetter, error) {
	var out []DeadLetter
	err := s.inTenant(ctx, tenantID, func(b base) error {
		rows, err := b.q.ListDeadLetters(ctx, dbgen.ListDeadLettersParams{TenantID: &tenantID, Status: strPtr(status)})
		for _, r := range rows {
			out = append(out, deadLetterFromRow(r))
		}
		return err
	})
	return out, err
}

func deadLetterFromRow(r dbgen.DeadLetter) DeadLetter {
	h := map[string]string{}
	if err := json.Unmarshal(r.Headers, &h); err != nil {
		h = map[string]string{"raw": string(r.Headers)}
	}
	return DeadLetter{ID: r.ID, TenantID: r.TenantID, Consumer: r.Consumer, MessageID: r.MessageID, EventType: deref(r.EventType), Payload: r.Payload,
		Headers: h, Error: r.Error, Attempts: int(r.Attempts), Status: r.Status, FailedAt: r.FailedAt, ResolvedAt: r.ResolvedAt,
		ResolutionNote: deref(r.ResolutionNote)}
}

// ReplayEmitter grava na outbox a reentrega direcionada à fila do
// consumidor que falhou, preservando o identificador original da mensagem
// (o inbox desse consumidor continua deduplicando).
type ReplayEmitter func(targetQueue, originalMessageID, eventType string, aggregateID uuid.UUID, payload []byte) error

// DeadLetterResolution descreve o tratamento manual de uma mensagem não
// processada.
type DeadLetterResolution struct {
	// Status é DeadLetterReplayed ou DeadLetterDiscarded.
	Status string
	Actor  audit.Actor
	// By é o usuário que tratou a mensagem; nil quando o tratamento vem de
	// uma credencial de API (a autoria fica na auditoria, pelo ator).
	By *uuid.UUID
	// Note é a justificativa do tratamento, gravada na resolução e na
	// auditoria.
	Note string
}

// Situações finais de uma dead letter tratada manualmente.
const (
	DeadLetterReplayed  = "replayed"
	DeadLetterDiscarded = "discarded"
)

// ResolveDeadLetter marca a mensagem como reprocessada ou descartada e, na
// mesma transação, grava a reentrega (no reprocessamento) e a auditoria, com a
// justificativa. A auditoria é a única autoria das resoluções feitas por
// credencial de API, por isso não pode ser perdida separadamente da resolução.
func (s *Store) ResolveDeadLetter(ctx context.Context, tenantID, id uuid.UUID, res DeadLetterResolution,
	replay func(ctx context.Context, emit ReplayEmitter, d DeadLetter) error) (DeadLetter, error) {
	var action string
	switch res.Status {
	case DeadLetterReplayed:
		action = "operations.dead_letter_replayed"
	case DeadLetterDiscarded:
		action = "operations.dead_letter_discarded"
	default:
		return DeadLetter{}, fmt.Errorf("pgstore: situação de dead letter inválida: %q", res.Status)
	}
	var out DeadLetter
	err := s.inTenant(ctx, tenantID, func(b base) error {
		r, err := b.q.GetDeadLetterForUpdate(ctx, dbgen.GetDeadLetterForUpdateParams{TenantID: &tenantID, ID: id})
		if err != nil {
			return notFound(err, "dead_letter_not_found", "Mensagem não encontrada.")
		}
		if r.Status != "open" {
			return apperr.Conflict("dead_letter_resolved", "A mensagem já foi tratada.")
		}
		out = deadLetterFromRow(r)
		if replay != nil {
			emit := func(targetQueue, originalMessageID, eventType string, aggregateID uuid.UUID, payload []byte) error {
				var p any
				if err := json.Unmarshal(payload, &p); err != nil {
					return apperr.Conflict("dead_letter_payload_invalid", "Conteúdo da mensagem não pode ser reprocessado.")
				}
				return b.emitReplay(ctx, targetQueue, originalMessageID, eventType, aggregateID, p)
			}
			if err := replay(ctx, emit, out); err != nil {
				return err
			}
		}
		now := s.clock.Now()
		if err := b.q.ResolveDeadLetter(ctx, dbgen.ResolveDeadLetterParams{Status: res.Status, ResolvedAt: &now, ResolvedBy: res.By,
			ResolutionNote: &res.Note, TenantID: &tenantID, ID: id}); err != nil {
			return err
		}
		out.Status, out.ResolvedAt, out.ResolutionNote = res.Status, &now, res.Note
		return b.Audit(ctx, audit.New(tenantID, now, res.Actor, action, "dead_letter", id.String(),
			map[string]any{"consumer": out.Consumer, "event_type": out.EventType, "reason": res.Note}))
	})
	if err != nil {
		return DeadLetter{}, err
	}
	return out, nil
}
