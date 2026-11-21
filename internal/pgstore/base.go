// Package pgstore implementa as portas de persistência dos módulos sobre
// PostgreSQL, usando as consultas geradas pelo sqlc (internal/dbgen).
//
// Cada módulo tem um tipo de transação próprio (customersTx, billingTx...)
// que implementa a interface Tx definida pelo módulo. Todos compartilham a
// mesma transação pgx com contexto de tenant, de modo que efeitos de vários
// módulos (fatura, ledger, outbox, auditoria) são atômicos.
//
// Toda consulta filtra tenant_id explicitamente, mesmo com RLS ativa.
package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/calendar"
	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/ledger"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

// HeaderInjector acrescenta cabeçalhos de rastreamento (trace context) aos
// eventos da outbox.
type HeaderInjector func(ctx context.Context, headers map[string]string)

// Store é a raiz dos adaptadores.
type Store struct {
	db      *postgres.DB
	keyring *secrets.Keyring
	clock   clock.Clock
	inject  HeaderInjector
}

// New cria o store.
func New(db *postgres.DB, keyring *secrets.Keyring, c clock.Clock, inject HeaderInjector) *Store {
	return &Store{db: db, keyring: keyring, clock: c, inject: inject}
}

// DB expõe o banco (health checks e métricas).
func (s *Store) DB() *postgres.DB { return s.db }

// base reúne o acesso à transação corrente e os efeitos compartilhados.
type base struct {
	q        *dbgen.Queries
	tenantID uuid.UUID
	store    *Store
}

func (s *Store) inTenant(ctx context.Context, tenantID uuid.UUID, fn func(base) error) error {
	return s.db.InTenantTx(ctx, tenantID, func(tx pgx.Tx) error {
		return fn(base{q: dbgen.New(tx), tenantID: tenantID, store: s})
	})
}

func (s *Store) inGlobal(ctx context.Context, fn func(*dbgen.Queries) error) error {
	return s.db.InTx(ctx, func(tx pgx.Tx) error { return fn(dbgen.New(tx)) })
}

// Audit grava uma entrada na trilha de auditoria da transação.
func (b base) Audit(ctx context.Context, e audit.Entry) error {
	details, err := json.Marshal(e.Details)
	if err != nil {
		return err
	}
	return b.q.InsertAuditEntry(ctx, dbgen.InsertAuditEntryParams{
		ID: e.ID, TenantID: b.tenantID, OccurredAt: e.OccurredAt, ActorType: string(e.Actor.Type),
		ActorID: strPtr(e.Actor.ID), ActorLabel: strPtr(e.Actor.Label), Action: e.Action, ResourceType: e.ResourceType,
		ResourceID: strPtr(e.ResourceID), RequestID: strPtr(logging.AttrValue(ctx, "request_id")), Details: details,
	})
}

// Emit grava o evento na outbox na mesma transação.
func (b base) Emit(ctx context.Context, e outbox.Event) error {
	if e.TenantID != b.tenantID {
		return fmt.Errorf("pgstore: evento de outro tenant")
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	headers := map[string]string{}
	if rid := logging.AttrValue(ctx, "request_id"); rid != "" {
		headers["request_id"] = rid
	}
	if b.store.inject != nil {
		b.store.inject(ctx, headers)
	}
	hdr, err := json.Marshal(headers)
	if err != nil {
		return err
	}
	available := e.AvailableAt
	if available.IsZero() {
		available = e.OccurredAt
	}
	return b.q.InsertOutboxEvent(ctx, dbgen.InsertOutboxEventParams{
		ID: e.ID, TenantID: b.tenantID, EventType: e.Type, AggregateType: e.AggregateType, AggregateID: e.AggregateID,
		Payload: payload, Headers: hdr, OccurredAt: e.OccurredAt, AvailableAt: available,
	})
}

// Cabeçalhos de reentrega direcionada (reprocessamento de dead letter).
const (
	HeaderTargetQueue       = "recorvia-target-queue"
	HeaderOriginalMessageID = "recorvia-original-message-id"
)

func (b base) emitReplay(ctx context.Context, targetQueue, originalMessageID, eventType string, aggregateID uuid.UUID, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	headers, err := json.Marshal(map[string]string{HeaderTargetQueue: targetQueue, HeaderOriginalMessageID: originalMessageID})
	if err != nil {
		return err
	}
	now := b.store.clock.Now()
	return b.q.InsertOutboxEvent(ctx, dbgen.InsertOutboxEventParams{ID: ids.New(), TenantID: b.tenantID, EventType: eventType,
		AggregateType: "dead_letter_replay", AggregateID: aggregateID, Payload: body, Headers: headers, OccurredAt: now, AvailableAt: now})
}

// MarkInbox registra o processamento de uma mensagem; false indica repetição.
func (b base) MarkInbox(ctx context.Context, consumer string, messageID uuid.UUID) (bool, error) {
	n, err := b.q.InsertInbox(ctx, dbgen.InsertInboxParams{TenantID: b.tenantID, Consumer: consumer, MessageID: messageID, ProcessedAt: b.store.clock.Now()})
	return n == 1, err
}

// PostLedger grava um lançamento balanceado. A chave de idempotência torna o
// lançamento seguro em reprocessamentos.
func (b base) PostLedger(ctx context.Context, t ledger.Transaction) error {
	if err := t.Validate(); err != nil {
		return err
	}
	inserted, err := b.q.InsertLedgerTransaction(ctx, dbgen.InsertLedgerTransactionParams{
		ID: t.ID, TenantID: b.tenantID, Kind: t.Kind, Description: t.Description, OccurredAt: t.OccurredAt,
		ReferenceType: t.ReferenceType, ReferenceID: t.ReferenceID, IdempotencyKey: t.IdempotencyKey,
		ReversesTransactionID: t.ReversesID, CreatedAt: b.store.clock.Now(),
	})
	if err != nil {
		return err
	}
	if inserted == 0 {
		return nil
	}
	for _, e := range t.Entries {
		accountID, err := b.ledgerAccount(ctx, e.Account, e.Amount.Currency())
		if err != nil {
			return err
		}
		if err := b.q.InsertLedgerEntry(ctx, dbgen.InsertLedgerEntryParams{
			ID: ids.New(), TenantID: b.tenantID, TransactionID: t.ID, AccountID: accountID,
			Direction: string(e.Direction), Amount: e.Amount.Amount(), Currency: e.Amount.Currency().Code(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b base) ledgerAccount(ctx context.Context, code ledger.AccountCode, cur money.Currency) (uuid.UUID, error) {
	id, err := b.q.GetLedgerAccountID(ctx, dbgen.GetLedgerAccountIDParams{TenantID: b.tenantID, Code: string(code), Currency: cur.Code()})
	if err == nil {
		return id, nil
	}
	if !postgres.IsNoRows(err) {
		return uuid.Nil, err
	}
	if err := b.ensureLedgerAccounts(ctx); err != nil {
		return uuid.Nil, err
	}
	return b.q.GetLedgerAccountID(ctx, dbgen.GetLedgerAccountIDParams{TenantID: b.tenantID, Code: string(code), Currency: cur.Code()})
}

func (b base) ensureLedgerAccounts(ctx context.Context) error {
	for _, a := range ledger.Chart {
		if err := b.q.EnsureLedgerAccount(ctx, dbgen.EnsureLedgerAccountParams{
			ID: ids.New(), TenantID: b.tenantID, Code: string(a.Code), Name: a.Name, Kind: string(a.Kind), Currency: money.BRL.Code(),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReverseLedger estorna o lançamento identificado pela chave original.
func (b base) ReverseLedger(ctx context.Context, originalKey, description string, at time.Time) error {
	orig, err := b.q.GetLedgerTransactionByKey(ctx, dbgen.GetLedgerTransactionByKeyParams{TenantID: b.tenantID, IdempotencyKey: originalKey})
	if postgres.IsNoRows(err) {
		return nil // nada foi lançado (por exemplo, fatura de valor zero)
	}
	if err != nil {
		return err
	}
	entries, err := b.q.ListLedgerEntriesForTransactions(ctx, dbgen.ListLedgerEntriesForTransactionsParams{TenantID: b.tenantID, Ids: []uuid.UUID{orig.ID}})
	if err != nil {
		return err
	}
	t := ledger.Transaction{ID: orig.ID, TenantID: b.tenantID, Kind: orig.Kind, ReferenceType: orig.ReferenceType,
		ReferenceID: orig.ReferenceID, IdempotencyKey: orig.IdempotencyKey}
	for _, e := range entries {
		cur, err := money.ParseCurrency(e.Currency)
		if err != nil {
			return err
		}
		t.Entries = append(t.Entries, ledger.Entry{Account: ledger.AccountCode(e.AccountCode), Direction: ledger.Direction(e.Direction), Amount: money.New(e.Amount, cur)})
	}
	return b.PostLedger(ctx, t.Reversal(ids.New(), at, description))
}

// TenantLocation retorna o fuso da organização corrente.
func (b base) TenantLocation(ctx context.Context) (*time.Location, error) {
	t, err := b.q.GetTenant(ctx)
	if err != nil {
		return nil, notFound(err, "organization_not_found", "Organização não encontrada.")
	}
	return time.LoadLocation(t.Timezone)
}

// --- conversões e mapeamento de erros ---

func notFound(err error, code, msg string) error {
	if postgres.IsNoRows(err) {
		return apperr.NotFound(code, msg)
	}
	return err
}

func versionConflict(rows int64, msg string) error {
	if rows == 0 {
		return apperr.PreconditionFailed("version_mismatch", msg)
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func ptrTime(t time.Time) *time.Time { return &t }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func intPtr32(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p)
	return &v
}

func intFromPtr32(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

func brl(minor int64) money.Money { return money.New(minor, money.BRL) }

// numericFromRat converte um racional finito em NUMERIC (até 10 casas).
func numericFromRat(r *big.Rat) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if r == nil {
		return n, nil
	}
	if err := n.Scan(money.FormatRat(r, money.MaxDecimalScale)); err != nil {
		return n, err
	}
	return n, nil
}

func numericPtr(r *big.Rat) (pgtype.Numeric, error) {
	if r == nil {
		return pgtype.Numeric{}, nil
	}
	return numericFromRat(r)
}

// ratFromNumeric converte NUMERIC em racional exato.
func ratFromNumeric(n pgtype.Numeric) (*big.Rat, error) {
	if !n.Valid {
		return nil, nil
	}
	if n.NaN || n.InfinityModifier != pgtype.Finite {
		return nil, errors.New("pgstore: valor numérico não finito")
	}
	r := new(big.Rat).SetInt(n.Int)
	if n.Exp > 0 {
		r.Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n.Exp)), nil)))
	} else if n.Exp < 0 {
		r.Quo(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-n.Exp)), nil)))
	}
	return r, nil
}

func dateToPg(d calendar.Date) pgtype.Date {
	return pgtype.Date{Time: time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, time.UTC), Valid: true}
}

func dateFromPg(d pgtype.Date) calendar.Date {
	return calendar.Date{Year: d.Time.Year(), Month: d.Time.Month(), Day: d.Time.Day()}
}

func jsonMap(b []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(b) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("pgstore: JSON inválido: %w", err)
	}
	if m == nil {
		// Detalhes ausentes gravados como JSON null (mapa nil serializado):
		// a API expõe sempre um objeto, como declara o contrato.
		return map[string]any{}, nil
	}
	return m, nil
}

// EscapeLike protege curingas de LIKE em buscas textuais.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
