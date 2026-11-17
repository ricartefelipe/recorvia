// Package gateways contém os adaptadores de provedores de pagamento e o
// cliente HTTP compartilhado que classifica falhas em permanentes,
// repetíveis ou ambíguas (resultado desconhecido).
package gateways

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/ricartefelipe/recorvia/internal/payments"
)

// MaxResponseBody limita o corpo lido das respostas do provedor.
const MaxResponseBody = 1 << 20

// Client executa requisições JSON com timeout e classificação de erros.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Headers func(*http.Request)
}

// NewHTTPClient cria um cliente com timeouts conservadores e sem seguir
// redirecionamentos (a API do provedor não deveria redirecionar).
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// classifyTransport distingue falhas antes do envio (conexão recusada,
// resolução de nome) das que podem ter ocorrido após o provedor processar.
func classifyTransport(err error) *payments.GatewayError {
	var opErr *net.OpError
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr), errors.Is(err, syscall.ECONNREFUSED):
		return &payments.GatewayError{Kind: payments.ErrKindRetryable, Message: "provedor inacessível", Err: err}
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return &payments.GatewayError{Kind: payments.ErrKindRetryable, Message: "conexão não estabelecida", Err: err}
	}
	return &payments.GatewayError{Kind: payments.ErrKindAmbiguous, Message: "resposta não recebida", Err: err}
}

// ClassifyStatus classifica respostas HTTP sem sucesso.
func ClassifyStatus(status int, body []byte) *payments.GatewayError {
	ge := &payments.GatewayError{StatusCode: status, Message: string(truncate(body, 300))}
	switch {
	case status == http.StatusNotFound:
		ge.Kind = payments.ErrKindNotFound
	case status == http.StatusTooManyRequests, status == http.StatusServiceUnavailable:
		ge.Kind = payments.ErrKindRetryable
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusConflict:
		ge.Kind = payments.ErrKindPermanent
	default:
		ge.Kind = payments.ErrKindAmbiguous
	}
	return ge
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// Do envia a requisição e decodifica a resposta JSON em out (se não nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (err error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return &payments.GatewayError{Kind: payments.ErrKindPermanent, Message: "requisição inválida", Err: err}
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return &payments.GatewayError{Kind: payments.ErrKindPermanent, Message: "requisição inválida", Err: err}
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Headers != nil {
		c.Headers(req)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return classifyTransport(err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody))
	if cerr := resp.Body.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return &payments.GatewayError{Kind: payments.ErrKindAmbiguous, StatusCode: resp.StatusCode, Message: "resposta incompleta", Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ClassifyStatus(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return &payments.GatewayError{Kind: payments.ErrKindAmbiguous, StatusCode: resp.StatusCode, Message: "resposta ilegível", Err: err}
	}
	return nil
}

// IsNotFound informa se o erro é recurso inexistente no provedor.
func IsNotFound(err error) bool {
	var ge *payments.GatewayError
	return errors.As(err, &ge) && ge.Kind == payments.ErrKindNotFound
}

// Wrapf acrescenta contexto preservando a classificação.
func Wrapf(err error, format string, args ...any) error {
	return fmt.Errorf(format+": %w", append(args, err)...)
}
