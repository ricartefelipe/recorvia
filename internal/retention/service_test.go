package retention

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/clock"
)

var start = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func tenantID(n byte) uuid.UUID {
	var id uuid.UUID
	id[15] = n
	return id
}

type purgeCall struct {
	kind   Kind
	tenant uuid.UUID
	cutoff time.Time
	now    time.Time
	limit  int
}

// fakeStore guarda, por tipo e tenant, quantos registros vencidos existem.
type fakeStore struct {
	clock      *clock.Manual
	expired    map[Kind]map[uuid.UUID]int
	calls      []purgeCall
	cutoffs    map[Kind]time.Time
	costPerOp  time.Duration // avanço do relógio a cada lote
	tenantsErr map[Kind]error
	purgeErr   map[uuid.UUID]error
	onPurge    func()
}

func newFakeStore(c *clock.Manual) *fakeStore {
	return &fakeStore{clock: c, expired: map[Kind]map[uuid.UUID]int{}, cutoffs: map[Kind]time.Time{}, tenantsErr: map[Kind]error{},
		purgeErr: map[uuid.UUID]error{}}
}

func (f *fakeStore) add(k Kind, tenant uuid.UUID, n int) {
	if f.expired[k] == nil {
		f.expired[k] = map[uuid.UUID]int{}
	}
	f.expired[k][tenant] += n
}

func (f *fakeStore) Tenants(_ context.Context, k Kind, cutoff time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	if err := f.tenantsErr[k]; err != nil {
		return nil, err
	}
	f.cutoffs[k] = cutoff
	var ids []uuid.UUID
	for id, n := range f.expired[k] {
		if n > 0 && (after == nil || bytes.Compare(id[:], after[:]) > 0) {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (f *fakeStore) Purge(_ context.Context, tenant uuid.UUID, k Kind, cutoff, now time.Time, limit int) (int, bool, error) {
	f.calls = append(f.calls, purgeCall{kind: k, tenant: tenant, cutoff: cutoff, now: now, limit: limit})
	if f.onPurge != nil {
		f.onPurge()
	}
	f.clock.Advance(f.costPerOp)
	if err := f.purgeErr[tenant]; err != nil {
		return 0, false, err
	}
	n := min(f.expired[k][tenant], limit)
	f.expired[k][tenant] -= n
	return n, n == limit, nil
}

func (f *fakeStore) remaining(k Kind) int {
	n := 0
	for _, v := range f.expired[k] {
		n += v
	}
	return n
}

func newService(t *testing.T, store Store, c clock.Clock, change func(*Policy), out io.Writer) *Service {
	t.Helper()
	p := DefaultPolicy()
	p.BatchSize = 10
	if change != nil {
		change(&p)
	}
	if out == nil {
		out = io.Discard
	}
	s, err := NewService(store, c, p, slog.New(slog.NewTextHandler(out, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewServiceRejectsInvalidPolicy(t *testing.T) {
	p := DefaultPolicy()
	p.Inbox = Days(1)
	if _, err := NewService(newFakeStore(clock.NewManual(start)), clock.NewManual(start), p, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("política abaixo do mínimo deveria ser recusada")
	}
}

// Cada tipo é removido em lotes do tamanho configurado, tenant a tenant, com
// o corte calculado pelo relógio injetado e pelo prazo do tipo.
func TestRunPurgesEveryKindInBatchesPerTenant(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	a, b := tenantID(1), tenantID(2)
	for _, k := range Kinds() {
		store.add(k, a, 25)
		store.add(k, b, 3)
	}
	var logs bytes.Buffer
	s := newService(t, store, c, nil, &logs)
	if got := s.Policy().BatchSize; got != 10 {
		t.Fatalf("política em uso: lote %d", got)
	}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Total() != 28*len(Kinds()) || len(res.Incomplete) != 0 || len(res.Failed) != 0 {
		t.Fatalf("resultado: %+v", res)
	}
	for _, k := range Kinds() {
		if res.Purged[k] != 28 || store.remaining(k) != 0 {
			t.Errorf("%s: removidos %d, restantes %d", k, res.Purged[k], store.remaining(k))
		}
		if want := start.Add(-s.Policy().For(k)); !store.cutoffs[k].Equal(want) {
			t.Errorf("%s: corte %s, esperado %s", k, store.cutoffs[k], want)
		}
	}
	// Tenant a: lotes de 10, 10 e 5 (o último, incompleto, encerra); tenant b: um lote.
	var inbox []purgeCall
	for _, call := range store.calls {
		if call.kind == KindInboxMessages {
			inbox = append(inbox, call)
		}
		if call.limit != 10 || !call.now.Equal(start) || !call.cutoff.Equal(start.Add(-s.Policy().For(call.kind))) {
			t.Fatalf("chamada com parâmetros inesperados: %+v", call)
		}
	}
	if len(inbox) != 4 || inbox[0].tenant != a || inbox[2].tenant != a || inbox[3].tenant != b {
		t.Fatalf("lotes do inbox: %+v", inbox)
	}
	if !strings.Contains(logs.String(), "expurgo por retenção executado") || !strings.Contains(logs.String(), "inbox_messages=28") {
		t.Fatalf("resumo da execução não registrado: %s", logs.String())
	}
	if strings.Contains(logs.String(), a.String()) {
		t.Fatal("o resumo não deve trazer identificadores de tenant")
	}

	// Sem nada vencido, a execução seguinte não remove nem registra nada.
	logs.Reset()
	res, err = s.Run(context.Background())
	if err != nil || res.Total() != 0 || logs.Len() != 0 {
		t.Fatalf("execução sem trabalho: %+v %v %q", res, err, logs.String())
	}
	if len(res.Purged) != len(Kinds()) {
		t.Fatalf("todos os tipos devem aparecer no resultado: %v", res.Purged)
	}
}

// O orçamento é dividido entre os tipos: o acúmulo de um não impede os
// demais, e a execução seguinte continua do tenant em que a anterior parou.
func TestRunBudgetIsSplitAcrossKindsAndResumesByRotation(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	a, b, d := tenantID(1), tenantID(2), tenantID(3)
	store.add(KindInboxMessages, a, 10) // um lote cheio e outro vazio
	store.add(KindInboxMessages, b, 5)
	store.add(KindInboxMessages, d, 5)
	store.add(KindInvitations, a, 4)
	store.costPerOp = 4 * time.Second
	// 11 tipos: fatia de 10 s por tipo; cada lote custa 4 s.
	s := newService(t, store, c, func(p *Policy) { p.RunBudget = 110 * time.Second }, nil)

	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Inbox: a (2 lotes, 8 s), b (1 lote, 12 s): a fatia acabou antes de d.
	if res.Purged[KindInboxMessages] != 15 || !slices.Equal(res.Incomplete, []Kind{KindInboxMessages}) {
		t.Fatalf("primeira execução: %+v", res)
	}
	if res.Purged[KindInvitations] != 4 {
		t.Fatalf("o último tipo deveria ser atendido apesar do acúmulo do primeiro: %+v", res)
	}
	if store.remaining(KindInboxMessages) != 5 {
		t.Fatalf("restantes: %d", store.remaining(KindInboxMessages))
	}

	store.calls = nil
	res, err = s.Run(context.Background())
	if err != nil || res.Purged[KindInboxMessages] != 5 || len(res.Incomplete) != 0 {
		t.Fatalf("segunda execução: %+v %v", res, err)
	}
	if len(store.calls) != 1 || store.calls[0].tenant != d {
		t.Fatalf("a segunda execução deveria continuar do tenant seguinte: %+v", store.calls)
	}
}

// O orçamento também interrompe um tenant no meio dos lotes.
func TestRunBudgetStopsInsideTenant(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	store.add(KindDeadLetters, tenantID(1), 100)
	store.costPerOp = 6 * time.Second
	s := newService(t, store, c, func(p *Policy) { p.RunBudget = 110 * time.Second }, nil)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Fatia de 10 s: o segundo lote termina aos 12 s e encerra o tenant.
	if res.Purged[KindDeadLetters] != 20 || !slices.Equal(res.Incomplete, []Kind{KindDeadLetters}) {
		t.Fatalf("resultado: %+v", res)
	}
}

// Um tenant com muito acúmulo é limitado por execução e não retém os demais.
func TestRunLimitsBatchesPerTenant(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	big, small := tenantID(1), tenantID(2)
	store.add(KindUsageEvents, big, 1000)
	store.add(KindUsageEvents, small, 7)
	s := newService(t, store, c, func(p *Policy) { p.MaxBatchesPerTenant = 3 }, nil)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Purged[KindUsageEvents] != 37 || !slices.Equal(res.Incomplete, []Kind{KindUsageEvents}) {
		t.Fatalf("resultado: %+v", res)
	}
	if store.expired[KindUsageEvents][small] != 0 || store.expired[KindUsageEvents][big] != 970 {
		t.Fatalf("restantes: %v", store.expired[KindUsageEvents])
	}
}

// A falha de um tenant ou da descoberta de um tipo não interrompe os demais:
// o que foi removido é devolvido junto com o erro.
func TestRunContinuesAfterFailures(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	bad, good := tenantID(1), tenantID(2)
	store.add(KindPortalLinks, bad, 5)
	store.add(KindPortalLinks, good, 5)
	store.add(KindNotifications, good, 2)
	store.add(KindInvitations, good, 1)
	store.purgeErr[bad] = errors.New("falha de gravação")
	store.tenantsErr[KindNotifications] = errors.New("descoberta indisponível")
	s := newService(t, store, c, nil, nil)
	res, err := s.Run(context.Background())
	if err == nil {
		t.Fatal("esperava erro")
	}
	for _, want := range []string{"portal_links: tenant " + bad.String() + ": falha de gravação", "notifications: descoberta indisponível"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("erro sem %q: %v", want, err)
		}
	}
	if res.Purged[KindPortalLinks] != 5 || res.Purged[KindInvitations] != 1 || res.Purged[KindNotifications] != 0 {
		t.Fatalf("removidos: %v", res.Purged)
	}
	if !slices.Equal(res.Failed, []Kind{KindNotifications, KindPortalLinks}) {
		t.Fatalf("tipos com falha: %v", res.Failed)
	}
	if !slices.Contains(res.Incomplete, KindPortalLinks) {
		t.Fatalf("tipo com tenant sem expurgo deveria ficar incompleto: %v", res.Incomplete)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	c := clock.NewManual(start)
	store := newFakeStore(c)
	store.add(KindInboxMessages, tenantID(1), 100)
	store.add(KindInvitations, tenantID(1), 5)
	ctx, cancel := context.WithCancel(context.Background())
	store.onPurge = cancel
	s := newService(t, store, c, nil, nil)
	res, err := s.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erro: %v", err)
	}
	if res.Purged[KindInboxMessages] != 10 || res.Purged[KindInvitations] != 0 {
		t.Fatalf("removidos após o cancelamento: %v", res.Purged)
	}
	if len(store.calls) != 1 {
		t.Fatalf("lotes após o cancelamento: %d", len(store.calls))
	}

	// Contexto já cancelado: nada é descoberto nem removido.
	store.calls = nil
	if _, err := s.Run(ctx); !errors.Is(err, context.Canceled) || len(store.calls) != 0 {
		t.Fatalf("execução com contexto cancelado: %v, %d lotes", err, len(store.calls))
	}
}
