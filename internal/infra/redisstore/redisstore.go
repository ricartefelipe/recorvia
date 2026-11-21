// Package redisstore guarda sessões, fluxos de login e contadores de
// limite de taxa no Redis. Sessões são cifradas (AES-GCM) e indexadas pelo
// hash do identificador: o conteúdo do Redis não permite reconstruir um
// cookie válido. O Redis não guarda dados financeiros: sua falha afeta
// autenticação e limites temporários, nunca valores lançados.
package redisstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/ricartefelipe/recorvia/internal/identity"
	"github.com/ricartefelipe/recorvia/internal/infra/secrets"
	"github.com/ricartefelipe/recorvia/internal/portal"
)

const prefix = "recorvia:"

// Store implementa identity.SessionStore.
type Store struct {
	rdb     redis.UniversalClient
	keyring *secrets.Keyring
	ns      string
}

// New cria o store. ns separa sessões do painel e do portal.
func New(rdb redis.UniversalClient, keyring *secrets.Keyring, ns string) *Store {
	return &Store{rdb: rdb, keyring: keyring, ns: ns}
}

// Open conecta ao Redis a partir da URL.
func Open(ctx context.Context, url string) (*redis.Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redis: URL inválida: %w", err)
	}
	opt.DialTimeout, opt.ReadTimeout, opt.WriteTimeout = 3*time.Second, 2*time.Second, 2*time.Second
	rdb := redis.NewClient(opt)
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := rdb.Ping(pctx).Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("redis: conexão: %w", err), rdb.Close())
	}
	return rdb, nil
}

func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

func (s *Store) key(kind, id string) string { return prefix + s.ns + ":" + kind + ":" + hashID(id) }

func (s *Store) userKey(userID uuid.UUID) string {
	return prefix + s.ns + ":user_sessions:" + userID.String()
}

func (s *Store) seal(v any, purpose string) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	ct, kid, err := s.keyring.Encrypt(b, []byte(s.ns+"/"+purpose))
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{KeyID: kid, Data: ct})
}

type envelope struct {
	KeyID string `json:"k"`
	Data  []byte `json:"d"`
}

func (s *Store) open(raw []byte, purpose string, v any) error {
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return err
	}
	pt, err := s.keyring.Decrypt(e.Data, e.KeyID, []byte(s.ns+"/"+purpose))
	if err != nil {
		return err
	}
	return json.Unmarshal(pt, v)
}

// SaveLoginFlow guarda o estado de login com expiração curta.
func (s *Store) SaveLoginFlow(ctx context.Context, f identity.LoginFlow, ttl time.Duration) error {
	b, err := s.seal(f, "login")
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, s.key("login", f.State), b, ttl).Err()
}

// TakeLoginFlow lê e remove atomicamente o estado (uso único).
func (s *Store) TakeLoginFlow(ctx context.Context, state string) (identity.LoginFlow, error) {
	raw, err := s.rdb.GetDel(ctx, s.key("login", state)).Bytes()
	if errors.Is(err, redis.Nil) {
		return identity.LoginFlow{}, identity.ErrLoginFlowNotFound
	}
	if err != nil {
		return identity.LoginFlow{}, err
	}
	var f identity.LoginFlow
	if err := s.open(raw, "login", &f); err != nil {
		return identity.LoginFlow{}, identity.ErrLoginFlowNotFound
	}
	return f, nil
}

// Save grava a sessão e a indexa por usuário (para revogação em massa).
func (s *Store) Save(ctx context.Context, sess identity.Session, ttl time.Duration) error {
	if ttl <= 0 {
		return identity.ErrSessionNotFound
	}
	b, err := s.seal(sess, "session")
	if err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.key("session", sess.ID), b, ttl)
	pipe.SAdd(ctx, s.userKey(sess.UserID), hashID(sess.ID))
	pipe.Expire(ctx, s.userKey(sess.UserID), 24*time.Hour)
	_, err = pipe.Exec(ctx)
	return err
}

// Get lê a sessão.
func (s *Store) Get(ctx context.Context, id string) (identity.Session, error) {
	raw, err := s.rdb.Get(ctx, s.key("session", id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return identity.Session{}, identity.ErrSessionNotFound
	}
	if err != nil {
		return identity.Session{}, err
	}
	var sess identity.Session
	if err := s.open(raw, "session", &sess); err != nil {
		return identity.Session{}, identity.ErrSessionNotFound
	}
	if sess.ID != id {
		return identity.Session{}, identity.ErrSessionNotFound
	}
	return sess, nil
}

// Delete remove a sessão.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, s.key("session", id)).Err()
}

// DeleteAllForUser revoga todas as sessões do usuário.
func (s *Store) DeleteAllForUser(ctx context.Context, userID uuid.UUID) error {
	hashes, err := s.rdb.SMembers(ctx, s.userKey(userID)).Result()
	if err != nil {
		return err
	}
	pipe := s.rdb.TxPipeline()
	for _, h := range hashes {
		pipe.Del(ctx, prefix+s.ns+":session:"+h)
	}
	pipe.Del(ctx, s.userKey(userID))
	_, err = pipe.Exec(ctx)
	return err
}

// PutJSON guarda um valor temporário cifrado (por exemplo, sessão do portal).
func (s *Store) PutJSON(ctx context.Context, kind, id string, v any, ttl time.Duration) error {
	b, err := s.seal(v, kind)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, s.key(kind, id), b, ttl).Err()
}

// GetJSON lê um valor temporário cifrado. Retorna false se ausente.
func (s *Store) GetJSON(ctx context.Context, kind, id string, v any) (bool, error) {
	raw, err := s.rdb.Get(ctx, s.key(kind, id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Conteúdo que não decifra (chave rotacionada ou adulteração) equivale a ausente.
	return s.open(raw, kind, v) == nil, nil
}

// DeleteKey remove um valor temporário.
func (s *Store) DeleteKey(ctx context.Context, kind, id string) error {
	return s.rdb.Del(ctx, s.key(kind, id)).Err()
}

// Allow aplica limite de taxa em janela fixa. Retorna o tempo até a próxima
// janela quando o limite foi atingido.
func (s *Store) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, time.Duration, error) {
	now := time.Now()
	slot := now.Truncate(window)
	k := prefix + "ratelimit:" + bucket + ":" + slot.Format("20060102T150405")
	pipe := s.rdb.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return true, 0, err
	}
	if incr.Val() > int64(limit) {
		return false, slot.Add(window).Sub(now), nil
	}
	return true, 0, nil
}

func failureKey(key string) string { return prefix + "failures:" + hashID(key) }

// Failures lê o número de falhas registradas para a chave na janela
// corrente (zero quando não há registro).
func (s *Store) Failures(ctx context.Context, key string) (int, error) {
	n, err := s.rdb.Get(ctx, failureKey(key)).Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}

// RecordFailure registra uma falha. A janela é fixa e começa na primeira
// falha: o contador expira window depois dela, independentemente das
// falhas seguintes. A chave guarda apenas o hash da origem.
func (s *Store) RecordFailure(ctx context.Context, key string, window time.Duration) error {
	k := failureKey(key)
	pipe := s.rdb.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.ExpireNX(ctx, k, window)
	_, err := pipe.Exec(ctx)
	return err
}

// Ping verifica o Redis (readiness).
func (s *Store) Ping(ctx context.Context) error { return s.rdb.Ping(ctx).Err() }

// PortalSessions implementa portal.SessionStore com o mesmo mecanismo das
// sessões do painel (conteúdo cifrado, chave pelo hash do identificador),
// em namespace próprio. Cada sessão é indexada pelo link que a originou,
// para que a revogação do link remova as sessões abertas com ele.
type PortalSessions struct{ s *Store }

// NewPortalSessions cria o armazenamento de sessões do portal. O Store
// informado deve usar um namespace exclusivo do portal.
func NewPortalSessions(s *Store) *PortalSessions { return &PortalSessions{s: s} }

const portalSessionKind = "portal_session"

// linkKey é o índice das sessões abertas a partir de um link: um conjunto
// com os hashes dos identificadores de sessão (nunca os identificadores).
func (p *PortalSessions) linkKey(linkID uuid.UUID) string {
	return prefix + p.s.ns + ":link_sessions:" + linkID.String()
}

// Save grava a sessão com expiração e a indexa pelo link de origem. O
// índice expira junto com a sessão de maior validade: a expiração só é
// estendida, nunca encurtada, por uma gravação posterior.
func (p *PortalSessions) Save(ctx context.Context, sess portal.Session, ttl time.Duration) error {
	if ttl <= 0 {
		return portal.ErrSessionNotFound
	}
	b, err := p.s.seal(sess, portalSessionKind)
	if err != nil {
		return err
	}
	lk := p.linkKey(sess.LinkID)
	pipe := p.s.rdb.TxPipeline()
	pipe.Set(ctx, p.s.key(portalSessionKind, sess.ID), b, ttl)
	pipe.SAdd(ctx, lk, hashID(sess.ID))
	pipe.ExpireNX(ctx, lk, ttl)
	pipe.ExpireGT(ctx, lk, ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// Get lê a sessão; ausente, expirada ou ilegível equivale a inexistente.
func (p *PortalSessions) Get(ctx context.Context, id string) (portal.Session, error) {
	var sess portal.Session
	ok, err := p.s.GetJSON(ctx, portalSessionKind, id, &sess)
	if err != nil {
		return portal.Session{}, err
	}
	if !ok || sess.ID != id {
		return portal.Session{}, portal.ErrSessionNotFound
	}
	return sess, nil
}

// Delete remove a sessão. A entrada no índice do link fica até expirar;
// ela aponta para uma chave que não existe mais.
func (p *PortalSessions) Delete(ctx context.Context, id string) error {
	return p.s.DeleteKey(ctx, portalSessionKind, id)
}

// DeleteByLink remove as sessões abertas a partir do link e o índice. É
// idempotente: sem sessões indexadas, nada é removido.
func (p *PortalSessions) DeleteByLink(ctx context.Context, linkID uuid.UUID) error {
	lk := p.linkKey(linkID)
	hashes, err := p.s.rdb.SMembers(ctx, lk).Result()
	if err != nil {
		return err
	}
	pipe := p.s.rdb.TxPipeline()
	for _, h := range hashes {
		pipe.Del(ctx, prefix+p.s.ns+":"+portalSessionKind+":"+h)
	}
	pipe.Del(ctx, lk)
	_, err = pipe.Exec(ctx)
	return err
}
