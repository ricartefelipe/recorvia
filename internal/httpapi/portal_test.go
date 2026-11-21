package httpapi

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/portal"
)

// A representação do link para a organização tem um conjunto fechado de
// campos e nunca inclui o hash do token, mesmo que ele chegue preenchido.
func TestPortalLinkViewNeverExposesTokenHash(t *testing.T) {
	created := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	used, revoked := created.Add(time.Hour), created.Add(2*time.Hour)
	userID, credentialID := uuid.New(), uuid.New()
	hash := []byte("0123456789abcdef0123456789abcdef")
	cases := []struct {
		name        string
		link        portal.LinkSummary
		status      string
		creatorType string
		creatorID   uuid.UUID
		creatorName any
		reason      any
	}{
		{"pendente gerado por membro", portal.LinkSummary{Link: portal.Link{ID: uuid.New(), TokenHash: hash, CreatedByUser: &userID, CreatedAt: created,
			ExpiresAt: created.Add(72 * time.Hour)}, CreatorName: "Ana", Current: portal.LinkPending}, "pending", "user", userID, "Ana", nil},
		{"usado e revogado, gerado por credencial", portal.LinkSummary{Link: portal.Link{ID: uuid.New(), TokenHash: hash, CreatedByCredential: &credentialID,
			CreatedAt: created, ExpiresAt: created.Add(72 * time.Hour), UsedAt: &used, RevokedAt: &revoked, RevokeReason: portal.RevokeRequested},
			CreatorName: "ERP interno", Current: portal.LinkRevoked}, "revoked", "api_credential", credentialID, "ERP interno", "revoked"},
		{"autor sem nome disponível", portal.LinkSummary{Link: portal.Link{ID: uuid.New(), CreatedByUser: &userID, CreatedAt: created,
			ExpiresAt: created.Add(time.Hour)}, Current: portal.LinkExpired}, "expired", "user", userID, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(portalLinkView(tc.link))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{string(hash), hex.EncodeToString(hash), base64.StdEncoding.EncodeToString(hash),
				base64.RawURLEncoding.EncodeToString(hash)} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("a representação do link contém o hash do token: %s", raw)
				}
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			want := []string{"created_at", "created_by", "expires_at", "id", "revoke_reason", "revoked_at", "status", "used_at"}
			if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
				t.Fatalf("campos do link: %v, esperado %v", keys, want)
			}
			by, _ := got["created_by"].(map[string]any)
			if got["id"] != tc.link.ID.String() || got["status"] != tc.status || got["revoke_reason"] != tc.reason || by["type"] != tc.creatorType ||
				by["id"] != tc.creatorID.String() || by["name"] != tc.creatorName || len(by) != 3 {
				t.Fatalf("link: %s", raw)
			}
			if (got["used_at"] == nil) != (tc.link.UsedAt == nil) || (got["revoked_at"] == nil) != (tc.link.RevokedAt == nil) {
				t.Fatalf("datas de uso e de revogação: %s", raw)
			}
		})
	}
}
