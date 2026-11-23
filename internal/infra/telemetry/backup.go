package telemetry

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Resultados de um backup base (rótulo outcome) e dependências do agente de
// backup contínuo (rótulo dependency), conjuntos fechados.
var (
	BackupRunOutcomes      = []string{"success", "failure"}
	BackupDependencies     = []string{"postgres", "storage"}
	backupServiceLabelName = "service"
)

// BackupMetrics reúne as métricas do agente de backup contínuo
// (cmd/pgbackup, ADR-087). Sem rótulos de tenant: o backup é do servidor
// inteiro.
type BackupMetrics struct {
	Registry *prometheus.Registry

	// BaseRuns conta os backups base executados pelo agente por resultado.
	BaseRuns *prometheus.CounterVec
	// BaseLastSuccess é o fim do backup base mais recente existente no
	// armazenamento (zero sem backup), lido da listagem do próprio
	// armazenamento: vale também para backups feitos por outro processo e
	// some quando a retenção remove o último.
	BaseLastSuccess prometheus.Gauge
	// BaseLastDuration é a duração do último backup base deste processo.
	BaseLastDuration prometheus.Gauge
	// BaseRetained é a quantidade de backups base no armazenamento.
	BaseRetained prometheus.Gauge
	// BaseInterval é o intervalo configurado entre backups base, para alertas
	// proporcionais a ele.
	BaseInterval prometheus.Gauge
	// DependencyUp informa se a última leitura do armazenamento (storage) e
	// do estado do arquivamento no PostgreSQL (postgres) funcionou.
	DependencyUp *prometheus.GaugeVec
}

// NewBackupMetrics registra os coletores do agente de backup.
func NewBackupMetrics() *BackupMetrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	cl := prometheus.Labels{backupServiceLabelName: "pgbackup"}
	m := &BackupMetrics{Registry: reg,
		BaseRuns: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "recorvia_backup_base_runs_total",
			Help: "Backups base executados pelo agente por resultado (success, failure).", ConstLabels: cl}, []string{"outcome"}),
		BaseLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_backup_base_last_success_timestamp_seconds",
			Help: "Fim do backup base mais recente no armazenamento (zero sem backup).", ConstLabels: cl}),
		BaseLastDuration: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_backup_base_last_duration_seconds",
			Help: "Duração do último backup base executado por este processo.", ConstLabels: cl}),
		BaseRetained: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_backup_base_retained",
			Help: "Backups base existentes no armazenamento.", ConstLabels: cl}),
		BaseInterval: prometheus.NewGauge(prometheus.GaugeOpts{Name: "recorvia_backup_base_interval_seconds",
			Help: "Intervalo configurado entre backups base.", ConstLabels: cl}),
		DependencyUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "recorvia_backup_dependency_up",
			Help: "Última leitura do armazenamento (storage) e do arquivamento no PostgreSQL (postgres) bem-sucedida (1) ou não (0).", ConstLabels: cl},
			[]string{"dependency"}),
	}
	for _, o := range BackupRunOutcomes {
		m.BaseRuns.WithLabelValues(o)
	}
	reg.MustRegister(m.BaseRuns, m.BaseLastSuccess, m.BaseLastDuration, m.BaseRetained, m.BaseInterval, m.DependencyUp)
	return m
}

// ArchiverStats é o estado do arquivamento de WAL no servidor
// (pg_stat_archiver e arquivos .ready de pg_wal/archive_status).
type ArchiverStats struct {
	// LastArchived é o instante do último segmento arquivado com sucesso
	// (zero se nenhum desde a última reinicialização das estatísticas).
	LastArchived time.Time
	// Failures é a quantidade de falhas do archive_command desde a última
	// reinicialização das estatísticas.
	Failures int64
	// Pending é a quantidade de segmentos completos aguardando arquivamento;
	// OldestPending, a idade do mais antigo deles (zero sem pendências).
	Pending       int64
	OldestPending time.Duration
	// CurrentSegmentBytes são os bytes já escritos no segmento atual, ainda
	// incompleto e não arquivável (informativo: um servidor ocioso pode
	// manter nele um registro pequeno sem trocar de segmento).
	CurrentSegmentBytes int64
}

// ObserveArchiver registra o coletor do arquivamento de WAL, que lê o estado
// do servidor a cada coleta (com prazo). Se a leitura falhar, as séries do
// arquivamento não são expostas e recorvia_backup_dependency_up{dependency="postgres"}
// vale 0.
func (m *BackupMetrics) ObserveArchiver(timeout time.Duration, read func(ctx context.Context) (ArchiverStats, error)) {
	// A disponibilidade passa a ser exposta pelo coletor, depois da leitura
	// da mesma coleta.
	m.Registry.Unregister(m.DependencyUp)
	m.Registry.MustRegister(newArchiverCollector(m, timeout, read))
}

type archiverCollector struct {
	m                                             *BackupMetrics
	timeout                                       time.Duration
	read                                          func(ctx context.Context) (ArchiverStats, error)
	lastArchived, failures, pending, age, current *prometheus.Desc
}

func newArchiverCollector(m *BackupMetrics, timeout time.Duration, read func(ctx context.Context) (ArchiverStats, error)) *archiverCollector {
	cl := prometheus.Labels{backupServiceLabelName: "pgbackup"}
	return &archiverCollector{m: m, timeout: timeout, read: read,
		lastArchived: prometheus.NewDesc("recorvia_backup_wal_last_archived_timestamp_seconds",
			"Instante do último segmento de WAL arquivado com sucesso (zero se nenhum).", nil, cl),
		failures: prometheus.NewDesc("recorvia_backup_wal_archive_failures_total",
			"Falhas do archive_command desde a última reinicialização das estatísticas do servidor.", nil, cl),
		pending: prometheus.NewDesc("recorvia_backup_wal_pending_segments",
			"Segmentos de WAL completos aguardando arquivamento.", nil, cl),
		age: prometheus.NewDesc("recorvia_backup_wal_oldest_pending_seconds",
			"Idade do segmento de WAL mais antigo aguardando arquivamento (zero sem pendências).", nil, cl),
		current: prometheus.NewDesc("recorvia_backup_wal_current_segment_bytes",
			"Bytes escritos no segmento de WAL atual, ainda incompleto e não arquivável.", nil, cl),
	}
}

// Describe implementa prometheus.Collector.
func (c *archiverCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.lastArchived, c.failures, c.pending, c.age, c.current} {
		ch <- d
	}
	c.m.DependencyUp.Describe(ch)
}

// Collect implementa prometheus.Collector.
func (c *archiverCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	s, err := c.read(ctx)
	if err != nil {
		c.m.DependencyUp.WithLabelValues("postgres").Set(0)
		c.m.DependencyUp.Collect(ch)
		return
	}
	c.m.DependencyUp.WithLabelValues("postgres").Set(1)
	c.m.DependencyUp.Collect(ch)
	last := 0.0
	if !s.LastArchived.IsZero() {
		last = float64(s.LastArchived.UnixNano()) / 1e9
	}
	ch <- prometheus.MustNewConstMetric(c.lastArchived, prometheus.GaugeValue, last)
	ch <- prometheus.MustNewConstMetric(c.failures, prometheus.CounterValue, float64(s.Failures))
	ch <- prometheus.MustNewConstMetric(c.pending, prometheus.GaugeValue, float64(s.Pending))
	ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, s.OldestPending.Seconds())
	ch <- prometheus.MustNewConstMetric(c.current, prometheus.GaugeValue, float64(s.CurrentSegmentBytes))
}
