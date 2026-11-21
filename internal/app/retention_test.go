package app

import (
	"encoding/base64"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/telemetry"
	"github.com/ricartefelipe/recorvia/internal/retention"
	"github.com/ricartefelipe/recorvia/internal/scheduler"
)

func workerConfig(t *testing.T, extra map[string]string) *config.Config {
	t.Helper()
	vars := map[string]string{
		"RECORVIA_ENV": "test", "RECORVIA_DATABASE_URL": "postgres://app@localhost/recorvia", "RECORVIA_AMQP_URL": "amqp://localhost",
		"RECORVIA_ENCRYPTION_KEYS":       "k1:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		"RECORVIA_ENCRYPTION_ACTIVE_KEY": "k1", "RECORVIA_SMTP_ADDR": "localhost:1025", "RECORVIA_SMTP_FROM": "cobranca@recorvia.test",
		"RECORVIA_SMTP_TLS": "none", "RECORVIA_COOKIE_SECURE": "false", "RECORVIA_COOKIE_NAME": "recorvia_session",
	}
	for k, v := range extra {
		vars[k] = v
	}
	cfg, err := config.Load(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Os prazos padrão da configuração são os da política padrão do domínio, e
// os mínimos aceitos pela configuração são os do domínio: o worker recusa na
// partida exatamente o que o serviço de retenção recusaria.
func TestRetentionPolicyFollowsConfiguration(t *testing.T) {
	cfg := workerConfig(t, nil)
	if err := cfg.ValidateWorker(); err != nil {
		t.Fatal(err)
	}
	if got, want := RetentionPolicy(cfg), retention.DefaultPolicy(); got != want {
		t.Fatalf("política padrão: %+v, esperado %+v", got, want)
	}
	if cfg.Retention.BatchSize != retention.DefaultBatchSize {
		t.Fatalf("lote padrão: %d", cfg.Retention.BatchSize)
	}

	vars := map[retention.Kind]string{
		retention.KindInboxMessages:           "RECORVIA_RETENTION_INBOX_DAYS",
		retention.KindDeadLetters:             "RECORVIA_RETENTION_DEAD_LETTERS_DAYS",
		retention.KindWebhookDeliveries:       "RECORVIA_RETENTION_WEBHOOKS_DAYS",
		retention.KindWebhookEvents:           "RECORVIA_RETENTION_WEBHOOKS_DAYS",
		retention.KindWebhookEndpointSecrets:  "RECORVIA_RETENTION_WEBHOOKS_DAYS",
		retention.KindProviderWebhookPayloads: "RECORVIA_RETENTION_PROVIDER_WEBHOOK_PAYLOADS_DAYS",
		retention.KindNotificationContents:    "RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS",
		retention.KindNotifications:           "RECORVIA_RETENTION_NOTIFICATIONS_DAYS",
		retention.KindPortalLinks:             "RECORVIA_RETENTION_PORTAL_LINKS_DAYS",
		retention.KindUsageEvents:             "RECORVIA_RETENTION_USAGE_EVENTS_DAYS",
		retention.KindInvitations:             "RECORVIA_RETENTION_INVITATIONS_DAYS",
	}
	for _, k := range retention.Kinds() {
		name, ok := vars[k]
		if !ok {
			t.Fatalf("tipo %s sem variável de ambiente", k)
		}
		minDays := int(retention.Minimum(k) / (24 * time.Hour))
		// O conteúdo acompanha o mínimo para não violar a ordem entre os dois prazos.
		extra := map[string]string{"RECORVIA_RETENTION_NOTIFICATION_CONTENT_DAYS": "7", name: strconv.Itoa(minDays)}
		atMin := workerConfig(t, extra)
		if err := atMin.ValidateWorker(); err != nil {
			t.Errorf("%s=%d deveria ser aceito: %v", name, minDays, err)
		}
		p := RetentionPolicy(atMin)
		if err := p.Validate(); err != nil || p.For(k) != retention.Minimum(k) {
			t.Errorf("%s no mínimo: política %s, %v", k, p.For(k), err)
		}
		extra[name] = strconv.Itoa(minDays - 1)
		below := workerConfig(t, extra)
		if err := below.ValidateWorker(); err == nil {
			t.Errorf("%s=%d deveria ser recusado pela configuração", name, minDays-1)
		}
		if err := RetentionPolicy(below).Validate(); err == nil {
			t.Errorf("%s=%d deveria ser recusado pela política", name, minDays-1)
		}
	}
	for _, size := range []int{retention.MinBatchSize - 1, retention.MaxBatchSize + 1} {
		c := workerConfig(t, map[string]string{"RECORVIA_RETENTION_BATCH_SIZE": strconv.Itoa(size)})
		if c.ValidateWorker() == nil || RetentionPolicy(c).Validate() == nil {
			t.Errorf("lote %d deveria ser recusado pela configuração e pela política", size)
		}
	}
	above := workerConfig(t, map[string]string{"RECORVIA_RETENTION_INBOX_DAYS": strconv.Itoa(int(retention.MaxRetention/(24*time.Hour)) + 1)})
	if above.ValidateWorker() == nil || RetentionPolicy(above).Validate() == nil {
		t.Error("prazo acima do máximo deveria ser recusado pela configuração e pela política")
	}
}

// O expurgo é uma tarefa agendada (com lease, como as demais) e os tipos das
// métricas são os do domínio.
func TestRetentionJobAndMetricKinds(t *testing.T) {
	w := &Worker{Core: &Core{}, Metrics: telemetry.NewMetrics("teste")}
	jobs := w.Jobs()
	i := slices.IndexFunc(jobs, func(j scheduler.Job) bool { return j.Name == JobRetention })
	if i < 0 {
		t.Fatal("tarefa retention.purge ausente")
	}
	if j := jobs[i]; j.Interval != time.Hour || j.Timeout < retention.DefaultRunBudget || j.Run == nil {
		t.Fatalf("tarefa de expurgo: intervalo %s, limite %s", j.Interval, j.Timeout)
	}
	var kinds []string
	for _, k := range retention.Kinds() {
		kinds = append(kinds, string(k))
	}
	if !slices.Equal(kinds, telemetry.RetentionKinds) {
		t.Fatalf("tipos das métricas %v diferentes dos tipos do domínio %v", telemetry.RetentionKinds, kinds)
	}
	w.initMetrics(jobs)
	for _, c := range []prometheus.Collector{w.Metrics.RetentionPurged, w.Metrics.RetentionPurgeFailures, w.Metrics.RetentionPurgeIncomplete} {
		if n := testutil.CollectAndCount(c); n != len(kinds) {
			t.Fatalf("séries do expurgo criadas com zero: %d, esperado %d", n, len(kinds))
		}
	}
}

// Os resultados do expurgo alimentam contadores por tipo, sem rótulos de
// tenant; resultados parciais (com falha em algum tipo) também contam.
func TestRetentionResultFeedsMetrics(t *testing.T) {
	m := telemetry.NewMetrics("teste")
	m.InitRetention()
	recordRetention(m, retention.Result{
		Purged:     map[retention.Kind]int{retention.KindInboxMessages: 120, retention.KindUsageEvents: 5000, retention.KindDeadLetters: 0},
		Failed:     []retention.Kind{retention.KindNotifications},
		Incomplete: []retention.Kind{retention.KindUsageEvents},
	})
	recordRetention(m, retention.Result{Purged: map[retention.Kind]int{retention.KindInboxMessages: 30},
		Incomplete: []retention.Kind{retention.KindUsageEvents}})
	for name, c := range map[string]struct{ got, want float64 }{
		"inbox removidos":        {testutil.ToFloat64(m.RetentionPurged.WithLabelValues("inbox_messages")), 150},
		"consumo removido":       {testutil.ToFloat64(m.RetentionPurged.WithLabelValues("usage_events")), 5000},
		"dead letters removidas": {testutil.ToFloat64(m.RetentionPurged.WithLabelValues("dead_letters")), 0},
		"falhas de notificações": {testutil.ToFloat64(m.RetentionPurgeFailures.WithLabelValues("notifications")), 1},
		"falhas de inbox":        {testutil.ToFloat64(m.RetentionPurgeFailures.WithLabelValues("inbox_messages")), 0},
		"consumo sem vazão":      {testutil.ToFloat64(m.RetentionPurgeIncomplete.WithLabelValues("usage_events")), 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, esperado %v", name, c.got, c.want)
		}
	}
	if n := testutil.CollectAndCount(m.RetentionPurged); n != len(telemetry.RetentionKinds) {
		t.Fatalf("o resultado não pode criar séries fora dos tipos declarados: %d", n)
	}
}
