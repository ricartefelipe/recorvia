package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func gatherBackup(t *testing.T, m *BackupMetrics) map[string]*dto.MetricFamily {
	t.Helper()
	mf, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, f := range mf {
		out[f.GetName()] = f
	}
	return out
}

func TestBackupArchiverIsReadOnEachGather(t *testing.T) {
	m := NewBackupMetrics()
	last := time.Date(2026, 11, 22, 12, 0, 0, 500000000, time.UTC)
	var fail error
	m.ObserveArchiver(time.Second, func(ctx context.Context) (ArchiverStats, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("leitura sem prazo")
		}
		return ArchiverStats{LastArchived: last, Failures: 3, Pending: 2, OldestPending: 90 * time.Second, CurrentSegmentBytes: 8192}, fail
	})
	got := gatherBackup(t, m)
	svc := map[string]string{"service": "pgbackup"}
	for name, want := range map[string]float64{
		"recorvia_backup_wal_last_archived_timestamp_seconds": float64(last.UnixNano()) / 1e9,
		"recorvia_backup_wal_archive_failures_total":          3,
		"recorvia_backup_wal_pending_segments":                2,
		"recorvia_backup_wal_oldest_pending_seconds":          90,
		"recorvia_backup_wal_current_segment_bytes":           8192,
	} {
		if v := value(t, got[name], svc); v != want {
			t.Errorf("%s = %v, esperado %v", name, v, want)
		}
	}
	if v := value(t, got["recorvia_backup_dependency_up"], map[string]string{"dependency": "postgres"}); v != 1 {
		t.Errorf("postgres = %v", v)
	}
	for _, o := range BackupRunOutcomes {
		if v := value(t, got["recorvia_backup_base_runs_total"], map[string]string{"outcome": o}); v != 0 {
			t.Errorf("runs %s = %v", o, v)
		}
	}

	// Falha na leitura: sem séries do arquivamento e postgres indisponível
	// na mesma coleta.
	fail = errors.New("recusado")
	got = gatherBackup(t, m)
	if _, ok := got["recorvia_backup_wal_last_archived_timestamp_seconds"]; ok {
		t.Error("séries do arquivamento expostas sem leitura")
	}
	if v := value(t, got["recorvia_backup_dependency_up"], map[string]string{"dependency": "postgres"}); v != 0 {
		t.Errorf("postgres = %v depois da falha", v)
	}

	// Nunca arquivado: zero (a idade calculada pelo alerta é a maior possível).
	fail = nil
	last = time.Time{}
	got = gatherBackup(t, m)
	if v := value(t, got["recorvia_backup_wal_last_archived_timestamp_seconds"], svc); v != 0 {
		t.Errorf("nunca arquivado = %v", v)
	}
}
