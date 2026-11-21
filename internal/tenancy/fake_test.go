package tenancy

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/kernel/address"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/outbox"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

var errBoom = errors.New("falha simulada")

// tenantData é o estado de um tenant no armazenamento em memória.
type tenantData struct {
	tenant      *Tenant
	members     map[uuid.UUID]Membership
	emails      map[uuid.UUID]string
	invitations map[uuid.UUID]Invitation
	plan        platformbilling.Plan
	platformSub *platformbilling.Subscription
	initialized bool
	audits      []audit.Entry
	events      []outbox.Event
}

func newTenantData() *tenantData {
	return &tenantData{members: map[uuid.UUID]Membership{}, emails: map[uuid.UUID]string{}, invitations: map[uuid.UUID]Invitation{}}
}

func (d *tenantData) clone() *tenantData {
	c := *d
	if d.tenant != nil {
		t := *d.tenant
		c.tenant = &t
	}
	if d.platformSub != nil {
		s := *d.platformSub
		c.platformSub = &s
	}
	c.members = maps.Clone(d.members)
	c.emails = maps.Clone(d.emails)
	c.invitations = maps.Clone(d.invitations)
	c.audits = slices.Clone(d.audits)
	c.events = slices.Clone(d.events)
	return &c
}

// fakeStore implementa Store e Tx em memória. Cada transação trabalha sobre
// uma cópia do estado do tenant e só a confirma quando fn retorna nil, como
// um rollback real.
type fakeStore struct {
	tenants         map[uuid.UUID]*tenantData
	takenSlugs      map[string]bool
	defaultPlan     platformbilling.Plan
	userMemberships map[uuid.UUID][]UserMembership
	fail            map[string]error
	calls           map[string]int
	checkedSlug     string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tenants: map[uuid.UUID]*tenantData{}, takenSlugs: map[string]bool{},
		userMemberships: map[uuid.UUID][]UserMembership{}, fail: map[string]error{}, calls: map[string]int{},
	}
}

func (s *fakeStore) hit(op string) error {
	s.calls[op]++
	return s.fail[op]
}

func (s *fakeStore) data(id uuid.UUID) *tenantData {
	d, ok := s.tenants[id]
	if !ok {
		d = newTenantData()
		s.tenants[id] = d
	}
	return d
}

func (s *fakeStore) InTenant(_ context.Context, tenantID uuid.UUID, fn func(Tx) error) error {
	if err := s.hit("InTenant"); err != nil {
		return err
	}
	work := s.data(tenantID).clone()
	if err := fn(&fakeTx{store: s, d: work, tenantID: tenantID}); err != nil {
		return err
	}
	s.tenants[tenantID] = work
	return nil
}

func (s *fakeStore) SlugAvailable(_ context.Context, slug string) (bool, error) {
	s.checkedSlug = slug
	if err := s.hit("SlugAvailable"); err != nil {
		return false, err
	}
	return !s.takenSlugs[slug], nil
}

func (s *fakeStore) FindInvitation(_ context.Context, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	if err := s.hit("FindInvitation"); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	for tid, d := range s.tenants {
		for _, inv := range d.invitations {
			if bytes.Equal(inv.TokenHash, tokenHash) {
				return tid, inv.ID, nil
			}
		}
	}
	return uuid.Nil, uuid.Nil, apperr.NotFound("invitation_not_found", "Convite não encontrado.")
}

func (s *fakeStore) UserMemberships(_ context.Context, userID uuid.UUID) ([]UserMembership, error) {
	if err := s.hit("UserMemberships"); err != nil {
		return nil, err
	}
	return s.userMemberships[userID], nil
}

type fakeTx struct {
	store    *fakeStore
	d        *tenantData
	tenantID uuid.UUID
}

func (t *fakeTx) Tenant(context.Context) (Tenant, error) {
	if err := t.store.hit("Tenant"); err != nil {
		return Tenant{}, err
	}
	if t.d.tenant == nil {
		return Tenant{}, apperr.NotFound("organization_not_found", "Organização não encontrada.")
	}
	return *t.d.tenant, nil
}

func (t *fakeTx) InsertTenant(_ context.Context, tn Tenant) error {
	if err := t.store.hit("InsertTenant"); err != nil {
		return err
	}
	t.d.tenant = &tn
	return nil
}

func (t *fakeTx) UpdateTenant(_ context.Context, tn Tenant) error {
	if err := t.store.hit("UpdateTenant"); err != nil {
		return err
	}
	if t.d.tenant == nil || t.d.tenant.Version != tn.Version {
		return apperr.PreconditionFailed("version_mismatch", "A organização foi alterada por outra pessoa.")
	}
	// Como no banco, o endereço fica em outra tabela e não é gravado aqui.
	tn.Version++
	tn.Address = t.d.tenant.Address
	t.d.tenant = &tn
	return nil
}

func (t *fakeTx) UpsertTenantAddress(_ context.Context, a address.Address, _ time.Time) error {
	if err := t.store.hit("UpsertTenantAddress"); err != nil {
		return err
	}
	if t.d.tenant == nil {
		return apperr.NotFound("organization_not_found", "Organização não encontrada.")
	}
	t.d.tenant.Address = &a
	return nil
}

func (t *fakeTx) InitTenantResources(context.Context) error {
	if err := t.store.hit("InitTenantResources"); err != nil {
		return err
	}
	t.d.initialized = true
	return nil
}

func (t *fakeTx) Membership(_ context.Context, userID uuid.UUID, _ bool) (Membership, error) {
	if err := t.store.hit("Membership"); err != nil {
		return Membership{}, err
	}
	m, ok := t.d.members[userID]
	if !ok {
		return Membership{}, apperr.NotFound("member_not_found", "Membro não encontrado.")
	}
	return m, nil
}

func (t *fakeTx) InsertMembership(_ context.Context, m Membership) error {
	if err := t.store.hit("InsertMembership"); err != nil {
		return err
	}
	if _, ok := t.d.members[m.UserID]; ok {
		return apperr.Conflict("membership_exists", "Associação já existe.")
	}
	t.d.members[m.UserID] = m
	return nil
}

func (t *fakeTx) UpdateMembership(_ context.Context, m Membership) error {
	if err := t.store.hit("UpdateMembership"); err != nil {
		return err
	}
	cur, ok := t.d.members[m.UserID]
	if !ok || cur.Version != m.Version {
		return apperr.PreconditionFailed("version_mismatch", "O membro foi alterado por outra pessoa.")
	}
	m.Version++
	t.d.members[m.UserID] = m
	return nil
}

func (t *fakeTx) Members(context.Context) ([]Member, error) {
	if err := t.store.hit("Members"); err != nil {
		return nil, err
	}
	out := make([]Member, 0, len(t.d.members))
	for id, m := range t.d.members {
		out = append(out, Member{Membership: m, Email: t.d.emails[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (t *fakeTx) CountActiveOwners(context.Context) (int, error) {
	if err := t.store.hit("CountActiveOwners"); err != nil {
		return 0, err
	}
	n := 0
	for _, m := range t.d.members {
		if m.Role == RoleOwner && m.Status == MembershipActive {
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) Invitation(_ context.Context, id uuid.UUID, _ bool) (Invitation, error) {
	if err := t.store.hit("Invitation"); err != nil {
		return Invitation{}, err
	}
	inv, ok := t.d.invitations[id]
	if !ok {
		return Invitation{}, apperr.NotFound("invitation_not_found", "Convite não encontrado.")
	}
	return inv, nil
}

func (t *fakeTx) InsertInvitation(_ context.Context, inv Invitation) error {
	if err := t.store.hit("InsertInvitation"); err != nil {
		return err
	}
	t.d.invitations[inv.ID] = inv
	return nil
}

func (t *fakeTx) UpdateInvitation(_ context.Context, inv Invitation) error {
	if err := t.store.hit("UpdateInvitation"); err != nil {
		return err
	}
	t.d.invitations[inv.ID] = inv
	return nil
}

func (t *fakeTx) Invitations(context.Context) ([]Invitation, error) {
	if err := t.store.hit("Invitations"); err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Values(t.d.invitations))
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (t *fakeTx) PendingInvitations(_ context.Context, now time.Time) (int, error) {
	if err := t.store.hit("PendingInvitations"); err != nil {
		return 0, err
	}
	n := 0
	for _, inv := range t.d.invitations {
		if inv.State(now) == InvitationPending {
			n++
		}
	}
	return n, nil
}

func (t *fakeTx) DefaultPlatformPlan(context.Context) (platformbilling.Plan, error) {
	if err := t.store.hit("DefaultPlatformPlan"); err != nil {
		return platformbilling.Plan{}, err
	}
	return t.store.defaultPlan, nil
}

func (t *fakeTx) LockPlatformPlan(context.Context) (platformbilling.Plan, error) {
	if err := t.store.hit("LockPlatformPlan"); err != nil {
		return platformbilling.Plan{}, err
	}
	return t.d.plan, nil
}

func (t *fakeTx) PlatformUsage(context.Context) (platformbilling.Usage, error) {
	if err := t.store.hit("PlatformUsage"); err != nil {
		return platformbilling.Usage{}, err
	}
	n := 0
	for _, m := range t.d.members {
		if m.Status == MembershipActive {
			n++
		}
	}
	return platformbilling.Usage{ActiveUsers: n}, nil
}

func (t *fakeTx) InsertPlatformSubscription(_ context.Context, s platformbilling.Subscription) error {
	if err := t.store.hit("InsertPlatformSubscription"); err != nil {
		return err
	}
	t.d.platformSub = &s
	return nil
}

func (t *fakeTx) Audit(_ context.Context, e audit.Entry) error {
	if err := t.store.hit("Audit"); err != nil {
		return err
	}
	t.d.audits = append(t.d.audits, e)
	return nil
}

func (t *fakeTx) Emit(_ context.Context, e outbox.Event) error {
	if err := t.store.hit("Emit"); err != nil {
		return err
	}
	t.d.events = append(t.d.events, e)
	return nil
}
