package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
)

// O cookie de vínculo do login tem nome derivado do state (um por fluxo) e
// herda o prefixo __Host- do cookie de sessão; com o prefixo, o caminho é /
// (exigência do navegador para __Host-), sem ele fica restrito a /auth.
func TestLoginCookiePerFlow(t *testing.T) {
	cases := []struct {
		name, cookie, prefix, path string
		secure                     bool
	}{
		{"produção", "__Host-recorvia_session", "__Host-recorvia_session_login_", "/", true},
		{"local", "recorvia_session", "recorvia_session_login_", "/auth", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &API{Deps: Deps{Config: &config.Config{CookieName: tc.cookie, CookieSecure: tc.secure}}}
			if a.loginCookieName("state-a") == a.loginCookieName("state-b") {
				t.Fatal("fluxos diferentes devem ter cookies diferentes")
			}
			rec := httptest.NewRecorder()
			a.setLoginCookie(rec, "state-a", "vinculo", 10*time.Minute)
			ck := (&http.Response{Header: rec.Header()}).Cookies()[0]
			if !strings.HasPrefix(ck.Name, tc.prefix) || ck.Name != a.loginCookieName("state-a") || ck.Path != tc.path || ck.Secure != tc.secure ||
				!ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != 600 || strings.Contains(ck.Name, "state-a") {
				t.Fatalf("cookie de vínculo: %+v", ck)
			}
		})
	}
}
