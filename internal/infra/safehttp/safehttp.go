// Package safehttp é o cliente HTTP dos webhooks enviados, com proteção
// contra SSRF:
//
//   - resolve o DNS do destino e recusa a requisição se qualquer endereço
//     resolvido for interno (loopback, redes privadas, link-local, CGNAT,
//     multicast, reservados, documentação) ou de metadados de nuvem
//     (169.254.169.254, fd00:ec2::254 e equivalentes);
//   - conecta no próprio IP validado e confere de novo o endereço efetivo no
//     Dialer.Control, de modo que uma resposta de DNS diferente entre a
//     validação e a conexão (DNS rebinding) não alcança destinos internos;
//   - não usa proxy do ambiente, não segue redirecionamentos e limita tempo
//     de conexão, de handshake TLS, de resposta e o tamanho lido da resposta;
//   - exige https quando configurado (produção).
//
// Destinos privados só são aceitos pelos CIDRs explícitos da configuração
// (ambiente local); endereços de metadados de nuvem e endereços que não são
// unicast nunca são aceitos.
package safehttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

// ErrBlocked indica destino não permitido (endereço interno, metadados de
// nuvem, esquema ou URL inválidos). A falha é permanente para a URL.
var ErrBlocked = errors.New("safehttp: destino não permitido")

func blocked(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBlocked, fmt.Sprintf(format, args...))
}

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// blockedRanges são destinos internos ou não roteáveis na internet.
var blockedRanges = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	// ::/96 inclui ::, ::1 e os endereços compatíveis com IPv4 (::a.b.c.d,
	// obsoletos); ::ffff:0:0:0/96 são os traduzidos (SIIT, RFC 2765). Ambos
	// embutem um IPv4 que um tradutor na rede poderia alcançar.
	"::/96", "::ffff:0:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
	"3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8",
)

// metadataAddrs nunca são permitidos, nem pela lista de exceções.
var metadataAddrs = prefixes(
	"169.254.169.254/32", "169.254.170.2/32", "169.254.169.123/32", "100.100.100.200/32", "192.0.0.192/32",
	"fd00:ec2::254/128", "fd00:ec2::23/128",
)

// nat64 incorpora um IPv4 nos últimos 32 bits (RFC 6052).
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// Policy decide se um endereço pode ser destino.
type Policy struct {
	// AllowedPrivate são exceções explícitas (somente ambiente local).
	AllowedPrivate []netip.Prefix
}

// Check devolve ErrBlocked para endereços não permitidos.
func (p Policy) Check(ip netip.Addr) error {
	if !ip.IsValid() {
		return blocked("endereço inválido")
	}
	ip = ip.Unmap()
	if ip.Is6() && nat64.Contains(ip) {
		b := ip.As16()
		ip = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	}
	ip = ip.WithZone("")
	for _, m := range metadataAddrs {
		if m.Contains(ip) {
			return blocked("endereço de metadados de nuvem %s", ip)
		}
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return blocked("endereço não unicast %s", ip)
	}
	for _, a := range p.AllowedPrivate {
		if a.Contains(ip) {
			return nil
		}
	}
	for _, b := range blockedRanges {
		if b.Contains(ip) {
			return blocked("endereço interno %s", ip)
		}
	}
	return nil
}

// Resolver resolve nomes em endereços (net.DefaultResolver satisfaz).
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Options configura o cliente.
type Options struct {
	Policy       Policy
	Resolver     Resolver
	RequireHTTPS bool
	// Timeout total da requisição (inclui conexão e leitura da resposta).
	Timeout               time.Duration
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	// MaxResponseBytes limita o corpo lido (o restante é descartado com o
	// fechamento da conexão).
	MaxResponseBytes int64
	// TLSConfig permite raízes próprias em testes.
	TLSConfig *tls.Config
}

// Client envia requisições com as proteções descritas no pacote.
type Client struct {
	opts Options
	http *http.Client
}

// New cria o cliente com valores padrão seguros.
func New(o Options) *Client {
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.TLSHandshakeTimeout <= 0 {
		o.TLSHandshakeTimeout = 5 * time.Second
	}
	if o.ResponseHeaderTimeout <= 0 {
		o.ResponseHeaderTimeout = o.Timeout
	}
	if o.MaxResponseBytes <= 0 {
		o.MaxResponseBytes = 64 << 10
	}
	c := &Client{opts: o}
	tr := &http.Transport{
		Proxy:                  nil, // nunca usar proxy do ambiente: a validação vale para o destino real
		DialContext:            c.dialContext,
		TLSClientConfig:        o.TLSConfig,
		TLSHandshakeTimeout:    o.TLSHandshakeTimeout,
		ResponseHeaderTimeout:  o.ResponseHeaderTimeout,
		MaxResponseHeaderBytes: 32 << 10,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ForceAttemptHTTP2:      true,
	}
	c.http = &http.Client{
		Transport: tr,
		Timeout:   o.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c
}

// checkURL valida esquema e forma da URL.
func (c *Client) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Hostname() == "" || u.User != nil {
		return nil, blocked("URL inválida")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if c.opts.RequireHTTPS {
			return nil, blocked("somente https é permitido")
		}
	default:
		return nil, blocked("esquema %q não permitido", u.Scheme)
	}
	return u, nil
}

// resolve devolve os endereços do host, todos validados pela política.
func (c *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := c.opts.Policy.Check(ip); err != nil {
			return nil, err
		}
		return []netip.Addr{ip}, nil
	}
	ips, err := c.opts.Resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("safehttp: resolução de %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("safehttp: %s não resolve para nenhum endereço", host)
	}
	for _, ip := range ips {
		if err := c.opts.Policy.Check(ip); err != nil {
			return nil, err
		}
	}
	return ips, nil
}

// Validate resolve o destino de uma URL e confere todos os endereços.
func (c *Client) Validate(ctx context.Context, rawURL string) error {
	u, err := c.checkURL(rawURL)
	if err != nil {
		return err
	}
	_, err = c.resolve(ctx, u.Hostname())
	return err
}

// dialContext resolve e valida o host e conecta no IP validado; o Control
// confere o endereço efetivo da conexão.
func (c *Client) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, blocked("endereço inválido")
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, blocked("porta inválida")
	}
	ips, err := c.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: c.opts.DialTimeout, Control: c.control}
	var lastErr error
	for _, ip := range ips {
		conn, err := d.DialContext(ctx, network, netip.AddrPortFrom(ip, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Client) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return blocked("endereço de conexão inválido")
	}
	return c.opts.Policy.Check(ap.Addr())
}

// Post envia o corpo e devolve o status HTTP. Redirecionamentos não são
// seguidos (o status 3xx é devolvido); o corpo da resposta é lido até o
// limite e descartado.
func (c *Client) Post(ctx context.Context, rawURL string, header map[string]string, body []byte) (int, error) {
	u, err := c.checkURL(rawURL)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return 0, blocked("requisição inválida")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			return 0, err
		}
		return 0, fmt.Errorf("safehttp: requisição: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, c.opts.MaxResponseBytes)); err != nil {
		return resp.StatusCode, fmt.Errorf("safehttp: leitura da resposta: %w", err)
	}
	return resp.StatusCode, nil
}
