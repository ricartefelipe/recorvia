package apicredentials

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

var errBoom = errors.New("falha simulada")

type member struct {
	role   tenancy.Role
	active bool
}

// tenantData é o estado de um tenant no armazenamento em memória.
type tenantData struct {
	status      tenancy.TenantStatus
	plan        platformbilling.Plan
	members     map[uuid.UUID]member
	credentials map[uuid.UUID]Credential
	audits      []audit.Entry
	touches     int
}

func (d *tenantData) clone() *tenantData {
	c := *d
	c.members = maps.Clone(d.members)
	c.credentials = maps.Clone(d.credentials)
	c.audits = slices.Clone(d.audits)
	return &c
}

// fakeStore implementa Store e Tx em memória. Cada transação trabalha sobre
// uma cópia do estado do tenant e só a confirma quando fn retorna nil.
type fakeStore struct {
	tenants map[uuid.UUID]*tenantData
	fail    map[string]error
	calls   map[string]int
	// order registra as operações na sequência em que foram chamadas.
	order []string
	// listedAt, listedLimit e listedCursor são os argumentos recebidos pela
	// última listagem.
	listedAt     time.Time
	listedLimit  int
	listedCursor string
}

func newFakeStore() *fakeStore {
	return &fakeStore{tenants: map[uuid.UUID]*tenantData{}, fail: map[string]error{}, calls: map[string]int{}}
}

func (s *fakeStore) hit(op string) error {
	s.calls[op]++
	s.order = append(s.order, op)
	return s.fail[op]
}

func (s *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	d, ok := s.tenants[tenantID]
	if !ok {
		d = &tenantData{members: map[uuid.UUID]member{}, credentials: map[uuid.UUID]Credential{}}
	}
	work := d.clone()
	if err := fn(&fakeTx{store: s, tenantID: tenantID, d: work}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

func (s *fakeStore) FindCredential(_ context.Context, keyID string) (uuid.UUID, uuid.UUID, error) {
	if err := s.hit("FindCredential"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	for tenantID, d := range s.tenants {
		for _, c := range d.credentials {
			if c.KeyID == keyID {
				return tenantID, c.ID, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, apperr.NotFound("api_credential_not_found", "Credencial não encontrada.")
}

type fakeTx struct {
	store    *fakeStore
	tenantID uuid.UUID
	d        *tenantData
}

func (tx *fakeTx) PlatformPlan(context.Context) (platformbilling.Plan, error) {
	return tx.d.plan, tx.store.hit("PlatformPlan")
}

func (tx *fakeTx) LockIssuance(context.Context) error {
	return tx.store.hit("LockIssuance")
}

func (tx *fakeTx) CountActiveCredentials(_ context.Context, now time.Time) (int, error) {
	n := 0
	for _, c := range tx.d.credentials {
		if c.State(now) == StateActive {
			n++
		}
	}
	return n, tx.store.hit("CountActiveCredentials")
}

func (tx *fakeTx) InsertCredential(_ context.Context, c Credential) error {
	if err := tx.store.hit("InsertCredential"); err != nil {
		return err
	}
	if c.TenantID != tx.tenantID {
		return errors.New("credencial de outro tenant")
	}
	tx.d.credentials[c.ID] = c
	return nil
}

func (tx *fakeTx) Credential(_ context.Context, id uuid.UUID, _ bool) (Credential, error) {
	if err := tx.store.hit("Credential"); err != nil {
		return Credential{}, err
	}
	c, ok := tx.d.credentials[id]
	if !ok {
		return Credential{}, apperr.NotFound("api_credential_not_found", "Credencial não encontrada.")
	}
	return c, nil
}

func (tx *fakeTx) Credentials(_ context.Context, now time.Time, limit int, cursor string) (Page, error) {
	tx.store.listedAt, tx.store.listedLimit, tx.store.listedCursor = now, limit, cursor
	out := slices.Collect(maps.Values(tx.d.credentials))
	sort.Slice(out, func(i, j int) bool {
		if ai, aj := out[i].State(now) == StateActive, out[j].State(now) == StateActive; ai != aj {
			return ai
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return Page{Items: out}, tx.store.hit("Credentials")
}

func (tx *fakeTx) RotateCredential(_ context.Context, c Credential) error {
	if err := tx.store.hit("RotateCredential"); err != nil {
		return err
	}
	cur := tx.d.credentials[c.ID]
	if cur.Version != c.Version || cur.RevokedAt != nil {
		return apperr.PreconditionFailed("version_mismatch", "A credencial foi alterada por outra pessoa.")
	}
	c.Version++
	tx.d.credentials[c.ID] = c
	return nil
}

func (tx *fakeTx) RevokeCredential(_ context.Context, c Credential) error {
	if err := tx.store.hit("RevokeCredential"); err != nil {
		return err
	}
	cur := tx.d.credentials[c.ID]
	if cur.Version != c.Version || cur.RevokedAt != nil {
		return apperr.PreconditionFailed("version_mismatch", "A credencial foi alterada por outra pessoa.")
	}
	c.Version++
	tx.d.credentials[c.ID] = c
	return nil
}

func (tx *fakeTx) AuthContext(_ context.Context, id uuid.UUID) (AuthContext, error) {
	if err := tx.store.hit("AuthContext"); err != nil {
		return AuthContext{}, err
	}
	c, ok := tx.d.credentials[id]
	if !ok {
		return AuthContext{}, apperr.NotFound("api_credential_not_found", "Credencial não encontrada.")
	}
	m := tx.d.members[c.Issuer()]
	return AuthContext{Credential: c, TenantStatus: tx.d.status, IssuerRole: m.role, IssuerActive: m.active,
		PlanFeatures: slices.Clone(tx.d.plan.Features)}, nil
}

func (tx *fakeTx) TouchCredential(_ context.Context, id uuid.UUID, at, staleBefore time.Time) error {
	if err := tx.store.hit("TouchCredential"); err != nil {
		return err
	}
	c := tx.d.credentials[id]
	if c.LastUsedAt == nil || !c.LastUsedAt.After(staleBefore) {
		c.LastUsedAt = &at
		tx.d.credentials[id] = c
		tx.d.touches++
	}
	return nil
}

func (tx *fakeTx) Audit(_ context.Context, e audit.Entry) error {
	if err := tx.store.hit("Audit"); err != nil {
		return err
	}
	tx.d.audits = append(tx.d.audits, e)
	return nil
}

// fakeCounter implementa FailureCounter em memória.
type fakeCounter struct {
	counts  map[string]int
	windows map[string]time.Duration
	readErr error
	recErr  error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{counts: map[string]int{}, windows: map[string]time.Duration{}}
}

func (f *fakeCounter) Failures(_ context.Context, key string) (int, error) {
	return f.counts[key], f.readErr
}

func (f *fakeCounter) RecordFailure(_ context.Context, key string, window time.Duration) error {
	if f.recErr != nil {
		return f.recErr
	}
	f.counts[key]++
	f.windows[key] = window
	return nil
}
