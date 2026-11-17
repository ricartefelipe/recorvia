// Package postgres gerencia o pool pgx e as transações com contexto de
// tenant. O contexto é definido com set_config(..., true), que vale apenas
// para a transação corrente: ao término (commit ou rollback) o PostgreSQL
// descarta o valor, e nenhuma conexão volta ao pool com tenant associado.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrMissingTenant indica tentativa de abrir transação de tenant sem tenant.
var ErrMissingTenant = errors.New("postgres: tenant obrigatório")

// Options ajusta o pool.
type Options struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	Tracer          pgx.QueryTracer
	ApplicationName string
}

// DB encapsula o pool.
type DB struct {
	pool *pgxpool.Pool
}

// Open cria e verifica o pool.
func Open(ctx context.Context, url string, opts Options) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("postgres: configuração inválida: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	cfg.MaxConnLifetime = valueOr(opts.MaxConnLifetime, time.Hour)
	cfg.MaxConnIdleTime = valueOr(opts.MaxConnIdleTime, 15*time.Minute)
	if opts.Tracer != nil {
		cfg.ConnConfig.Tracer = opts.Tracer
	}
	if opts.ApplicationName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = opts.ApplicationName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: criação do pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: conexão: %w", err)
	}
	return &DB{pool: pool}, nil
}

// New encapsula um pool existente (usado em testes de integração).
func New(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// Pool expõe o pool para leituras específicas e métricas.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close fecha o pool.
func (db *DB) Close() { db.pool.Close() }

// Ping verifica a conectividade (readiness).
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// InTenantTx executa fn numa transação com o contexto do tenant. Sem tenant
// válido a função não é executada.
func (db *DB) InTenantTx(ctx context.Context, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	return db.InTenantTxOpts(ctx, tenantID, pgx.TxOptions{}, fn)
}

// InTenantTxOpts permite escolher isolamento e modo de acesso.
func (db *DB) InTenantTxOpts(ctx context.Context, tenantID uuid.UUID, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	if tenantID == uuid.Nil {
		return ErrMissingTenant
	}
	return db.inTx(ctx, opts, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return fmt.Errorf("postgres: definição do tenant: %w", err)
		}
		return fn(tx)
	})
}

// InTx executa fn numa transação sem contexto de tenant. Tabelas com RLS
// ficam invisíveis; use apenas para tabelas globais e funções de catálogo.
func (db *DB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.inTx(ctx, pgx.TxOptions{}, fn)
}

func (db *DB) inTx(ctx context.Context, opts pgx.TxOptions, fn func(pgx.Tx) error) (err error) {
	tx, err := db.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("postgres: início de transação: %w", err)
	}
	defer func() {
		if err != nil {
			// O rollback usa contexto próprio: o contexto original pode já
			// estar cancelado e a conexão precisa ser limpa mesmo assim.
			rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if rbErr := tx.Rollback(rbCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
			}
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func valueOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Códigos SQLSTATE relevantes.
const (
	codeUniqueViolation      = "23505"
	codeForeignKeyViolation  = "23503"
	codeCheckViolation       = "23514"
	codeSerializationFailure = "40001"
	codeInsufficientPriv     = "42501"
	codeRaiseException       = "P0001"
)

func pgCode(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr, true
	}
	return nil, false
}

// IsUniqueViolation informa se err é violação de unicidade; com constraint
// não vazia, exige também o nome da constraint.
func IsUniqueViolation(err error, constraint string) bool {
	e, ok := pgCode(err)
	return ok && e.Code == codeUniqueViolation && (constraint == "" || e.ConstraintName == constraint)
}

// IsForeignKeyViolation informa se err é violação de chave estrangeira.
func IsForeignKeyViolation(err error) bool {
	e, ok := pgCode(err)
	return ok && e.Code == codeForeignKeyViolation
}

// IsCheckViolation informa se err é violação de CHECK.
func IsCheckViolation(err error) bool {
	e, ok := pgCode(err)
	return ok && e.Code == codeCheckViolation
}

// IsInsufficientPrivilege informa se err decorre de RLS ou falta de GRANT.
func IsInsufficientPrivilege(err error) bool {
	e, ok := pgCode(err)
	return ok && e.Code == codeInsufficientPriv
}

// IsRaisedException informa se err veio de RAISE EXCEPTION em gatilho.
func IsRaisedException(err error) bool {
	e, ok := pgCode(err)
	return ok && e.Code == codeRaiseException
}

// IsNoRows informa se a consulta não retornou linhas.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
