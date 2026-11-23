package pgbackup

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
)

// PGServer lê do PostgreSQL a linha do tempo e o estado do arquivamento. A
// conexão usa as variáveis padrão do libpq (PGHOST, PGPORT, PGUSER,
// PGDATABASE) e a senha da configuração, se informada; o papel precisa ler
// pg_ls_archive_statusdir (superusuário ou pg_monitor) e, para o backup base,
// executar pg_backup_start (superusuário ou permissão concedida).
type PGServer struct {
	pool *pgxpool.Pool
}

// OpenServer cria o pool (sem conectar: a primeira consulta conecta).
func OpenServer(ctx context.Context, cfg Config) (*PGServer, error) {
	pc, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("configuração da conexão com o PostgreSQL: %w", err)
	}
	if cfg.PGPassword != "" {
		pc.ConnConfig.Password = cfg.PGPassword
	}
	pc.MaxConns = 2
	pc.ConnConfig.RuntimeParams["application_name"] = "recorvia-pgbackup"
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("conexão com o PostgreSQL: %w", err)
	}
	return &PGServer{pool: pool}, nil
}

// Close encerra o pool.
func (s *PGServer) Close() { s.pool.Close() }

// Timeline devolve a linha do tempo atual do servidor.
func (s *PGServer) Timeline(ctx context.Context) (uint32, error) {
	var tl int64
	if err := s.pool.QueryRow(ctx, "SELECT timeline_id FROM pg_control_checkpoint()").Scan(&tl); err != nil {
		return 0, fmt.Errorf("linha do tempo do servidor: %w", err)
	}
	return uint32(tl), nil
}

// archiverSQL lê pg_stat_archiver e os segmentos completos ainda não
// arquivados (arquivos .ready), com a idade do mais antigo.
const archiverSQL = `
SELECT COALESCE(extract(epoch FROM a.last_archived_time), 0)::float8,
       a.failed_count,
       s.pending,
       COALESCE(extract(epoch FROM now() - s.oldest), 0)::float8
  FROM pg_stat_archiver a,
       LATERAL (SELECT count(*) AS pending, min(modification) AS oldest
                  FROM pg_ls_archive_statusdir() WHERE name LIKE '%.ready') s`

// Archiver devolve o estado do arquivamento de WAL.
func (s *PGServer) Archiver(ctx context.Context) (telemetry.ArchiverStats, error) {
	var last, oldest float64
	var st telemetry.ArchiverStats
	if err := s.pool.QueryRow(ctx, archiverSQL).Scan(&last, &st.Failures, &st.Pending, &oldest); err != nil {
		return telemetry.ArchiverStats{}, fmt.Errorf("estado do arquivamento: %w", err)
	}
	if last > 0 {
		st.LastArchived = time.Unix(0, int64(last*1e9)).UTC()
	}
	st.OldestPending = time.Duration(oldest * float64(time.Second))
	return st, nil
}
