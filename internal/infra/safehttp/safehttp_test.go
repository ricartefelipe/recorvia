package safehttp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResolver devolve respostas programadas por host, em sequência (a
// última se repete), simulando DNS controlado e rebinding.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
	err     error
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	seq := r.answers[host]
	if len(seq) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := seq[0]
	if len(seq) > 1 {
		r.answers[host] = seq[1:]
	}
	return out, nil
}

func addrs(ss ...string) []netip.Addr {
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestPolicyCheck(t *testing.T) {
	strict := Policy{}
	local := Policy{AllowedPrivate: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("fd00::/8")}}
	cases := []struct {
		ip            string
		strict, local bool
	}{
		{"93.184.216.34", true, true},
		{"8.8.8.8", true, true},
		{"2001:4860:4860::8888", true, true},
		{"64:ff9b::808:808", true, true}, // NAT64 de 8.8.8.8
		{"10.1.2.3", false, false},
		{"172.16.0.1", false, false},
		{"192.168.1.1", false, false},
		{"100.64.0.1", false, false},
		{"127.0.0.1", false, true},
		{"::ffff:127.0.0.1", false, true}, // IPv4 mapeado
		{"64:ff9b::7f00:1", false, true},  // NAT64 de 127.0.0.1
		{"::1", false, false},
		{"169.254.10.1", false, true},
		{"169.254.169.254", false, false}, // metadados: nunca permitido
		{"fd00:ec2::254", false, false},
		{"fd12::1", false, true},
		{"100.100.100.200", false, false},
		{"192.0.0.192", false, false},
		{"fe80::1", false, false},
		{"0.0.0.0", false, false},
		{"::", false, false},
		{"255.255.255.255", false, false},
		{"224.0.0.1", false, false},
		{"ff02::1", false, false},
		{"198.18.0.1", false, false},
		{"203.0.113.9", false, false},
		{"2001:db8::1", false, false},
		{"2002:7f00:1::1", false, false},
		// IPv4 embutido em formas que não são o mapeamento ::ffff:a.b.c.d:
		// compatível (obsoleto) e traduzido (SIIT); a exceção local de
		// 127.0.0.0/8 não vale para elas.
		{"::127.0.0.1", false, false},
		{"::8.8.8.8", false, false},
		{"::ffff:0:127.0.0.1", false, false},
		{"::ffff:0:a9fe:a9fe", false, false},
		{"3fff::1", false, false},    // documentação (RFC 9637)
		{"5f00::1", false, false},    // SRv6 SIDs (RFC 9602)
		{"2001:4860::1", true, true}, // unicast global fora de 2001::/23
	}
	for _, c := range cases {
		ip := netip.MustParseAddr(c.ip)
		if got := strict.Check(ip) == nil; got != c.strict {
			t.Errorf("estrita %s: permitido=%v, esperado %v", c.ip, got, c.strict)
		}
		if got := local.Check(ip) == nil; got != c.local {
			t.Errorf("local %s: permitido=%v, esperado %v", c.ip, got, c.local)
		}
		if err := strict.Check(ip); err != nil && !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: erro deve ser ErrBlocked: %v", c.ip, err)
		}
	}
	if err := strict.Check(netip.Addr{}); !errors.Is(err, ErrBlocked) {
		t.Fatal("endereço inválido deve ser bloqueado")
	}
	if err := strict.Check(netip.MustParseAddr("fe80::1%eth0")); !errors.Is(err, ErrBlocked) {
		t.Fatal("endereço com zona deve ser bloqueado")
	}
}

func TestValidate(t *testing.T) {
	res := &fakeResolver{answers: map[string][][]netip.Addr{
		"publico.test": {addrs("93.184.216.34")},
		"interno.test": {addrs("10.0.0.5")},
		"misto.test":   {addrs("93.184.216.34", "127.0.0.1")},
		"vazio.test":   {{}},
	}}
	c := New(Options{Resolver: res, RequireHTTPS: true})
	ctx := context.Background()
	cases := []struct {
		url     string
		ok      bool
		blocked bool
	}{
		{"https://publico.test/hook", true, false},
		{"https://93.184.216.34/hook", true, false},
		{"https://[2001:4860:4860::8888]/hook", true, false},
		{"http://publico.test/hook", false, true},
		{"ftp://publico.test/hook", false, true},
		{"https://u:p@publico.test/hook", false, true},
		{"https://interno.test/hook", false, true},
		{"https://misto.test/hook", false, true},
		{"https://127.0.0.1/hook", false, true},
		{"https://169.254.169.254/latest/meta-data", false, true},
		{"https://[::1]/hook", false, true},
		{"https://inexistente.test/hook", false, false},
		{"https://vazio.test/hook", false, false},
		{"::sem-esquema", false, true},
	}
	for _, tc := range cases {
		err := c.Validate(ctx, tc.url)
		if (err == nil) != tc.ok || (err != nil && errors.Is(err, ErrBlocked) != tc.blocked) {
			t.Errorf("Validate(%s) = %v (ok=%v, bloqueado=%v)", tc.url, err, tc.ok, tc.blocked)
		}
	}
	res.err = errors.New("dns indisponível")
	if err := c.Validate(ctx, "https://publico.test/x"); err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("falha de DNS não é bloqueio: %v", err)
	}
}

type target struct {
	srv  *httptest.Server
	hits atomic.Int32
	last atomic.Value
}

func newTarget(t *testing.T, h http.HandlerFunc) *target {
	t.Helper()
	tg := &target{}
	tg.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tg.hits.Add(1)
		tg.last.Store(r.Clone(context.Background()))
		h(w, r)
	}))
	t.Cleanup(tg.srv.Close)
	return tg
}

func (tg *target) port() string {
	u, _ := url.Parse(tg.srv.URL)
	return u.Port()
}

var loopback = Policy{AllowedPrivate: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}

func ok(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestPostBlocksLoopbackByDefault(t *testing.T) {
	tg := newTarget(t, ok)
	c := New(Options{})
	_, err := c.Post(context.Background(), tg.srv.URL, nil, []byte("{}"))
	if !errors.Is(err, ErrBlocked) || tg.hits.Load() != 0 {
		t.Fatalf("loopback deve ser bloqueado antes da conexão: %v (requisições: %d)", err, tg.hits.Load())
	}
}

func TestPostWithExplicitPrivateException(t *testing.T) {
	tg := newTarget(t, ok)
	res := &fakeResolver{answers: map[string][][]netip.Addr{"hooks.local.test": {addrs("127.0.0.1")}}}
	c := New(Options{Policy: loopback, Resolver: res})
	code, err := c.Post(context.Background(), "http://hooks.local.test:"+tg.port()+"/hook", map[string]string{"webhook-id": "evt_1",
		"Content-Type": "application/json"}, []byte(`{"a":1}`))
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("exceção explícita deve permitir: %d %v", code, err)
	}
	r := tg.last.Load().(*http.Request)
	if r.Header.Get("webhook-id") != "evt_1" || r.Host != "hooks.local.test:"+tg.port() || r.Method != http.MethodPost {
		t.Fatalf("requisição recebida: %+v", r)
	}
}

// DNS rebinding: a validação vê um IP público, mas a conexão resolveria para
// loopback; o IP é validado de novo na conexão e a requisição não sai.
func TestPostRevalidatesAtConnectionTime(t *testing.T) {
	tg := newTarget(t, ok)
	res := &fakeResolver{answers: map[string][][]netip.Addr{"rebind.test": {addrs("93.184.216.34"), addrs("127.0.0.1")}}}
	c := New(Options{Resolver: res})
	u := "http://rebind.test:" + tg.port() + "/hook"
	if err := c.Validate(context.Background(), u); err != nil {
		t.Fatalf("validação inicial com IP público: %v", err)
	}
	if _, err := c.Post(context.Background(), u, nil, nil); !errors.Is(err, ErrBlocked) || tg.hits.Load() != 0 {
		t.Fatalf("rebinding deve ser bloqueado na conexão: %v", err)
	}
}

func TestDialerControlChecksEffectiveAddress(t *testing.T) {
	c := New(Options{})
	if err := c.control("tcp4", "127.0.0.1:443", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("Control deve recusar loopback")
	}
	if err := c.control("tcp6", "[fd00:ec2::254]:80", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("Control deve recusar metadados IPv6")
	}
	if err := c.control("tcp4", "93.184.216.34:443", nil); err != nil {
		t.Fatalf("Control deve aceitar endereço público: %v", err)
	}
	if err := c.control("tcp4", "lixo", nil); !errors.Is(err, ErrBlocked) {
		t.Fatal("endereço inválido")
	}
	if _, err := c.dialContext(context.Background(), "tcp", "semporta"); !errors.Is(err, ErrBlocked) {
		t.Fatal("endereço sem porta")
	}
	if _, err := c.dialContext(context.Background(), "tcp", "93.184.216.34:0"); !errors.Is(err, ErrBlocked) {
		t.Fatal("porta zero")
	}
}

func TestPostDoesNotFollowRedirects(t *testing.T) {
	final := newTarget(t, ok)
	redirect := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.srv.URL+"/destino", http.StatusFound)
	})
	c := New(Options{Policy: loopback})
	code, err := c.Post(context.Background(), redirect.srv.URL, nil, nil)
	if err != nil || code != http.StatusFound || final.hits.Load() != 0 {
		t.Fatalf("redirecionamento não pode ser seguido: %d %v (destino: %d)", code, err, final.hits.Load())
	}
}

func TestPostLimitsResponseSize(t *testing.T) {
	tg := newTarget(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range 64 { // 4 MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	c := New(Options{Policy: loopback, MaxResponseBytes: 1 << 10})
	code, err := c.Post(context.Background(), tg.srv.URL, nil, nil)
	if err != nil || code != http.StatusOK {
		t.Fatalf("resposta grande deve ser truncada sem erro: %d %v", code, err)
	}
}

func TestPostTimesOut(t *testing.T) {
	release := make(chan struct{})
	tg := newTarget(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	t.Cleanup(func() { close(release) })
	c := New(Options{Policy: loopback, Timeout: 100 * time.Millisecond})
	_, err := c.Post(context.Background(), tg.srv.URL, nil, nil)
	if err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("esperava tempo esgotado: %v", err)
	}
}

func TestPostIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	tg := newTarget(t, ok)
	c := New(Options{Policy: loopback})
	if code, err := c.Post(context.Background(), tg.srv.URL, nil, nil); err != nil || code != http.StatusNoContent || tg.hits.Load() != 1 {
		t.Fatalf("o proxy do ambiente não deve ser usado: %d %v", code, err)
	}
}

func TestPostOverTLSWithRequiredHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(ok))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{answers: map[string][][]netip.Addr{"example.com": {addrs("127.0.0.1")}}}
	c := New(Options{Policy: loopback, Resolver: res, RequireHTTPS: true, TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	code, err := c.Post(context.Background(), "https://example.com:"+u.Port()+"/hook", nil, nil)
	if err != nil || code != http.StatusNoContent {
		t.Fatalf("https com certificado válido: %d %v", code, err)
	}
	if _, err := c.Post(context.Background(), "http://example.com:"+u.Port()+"/hook", nil, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("http recusado com https obrigatório: %v", err)
	}
	if _, err := c.Post(context.Background(), "https://example.com:"+u.Port()+"/\x7f", nil, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("URL inválida: %v", err)
	}
}

func TestPostConnectionRefused(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	c := New(Options{Policy: loopback})
	if _, err := c.Post(context.Background(), "http://"+addr+"/x", nil, nil); err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("conexão recusada é falha transitória: %v", err)
	}
}
