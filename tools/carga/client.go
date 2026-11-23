package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// apiClient chama a API do Recorvia com uma credencial de API (Bearer) e
// registra a latência de cada chamada no recorder.
type apiClient struct {
	base  string
	token string
	http  *http.Client
	rec   *recorder
}

func newHTTPClient(conns int) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns, t.MaxIdleConnsPerHost, t.MaxConnsPerHost = conns, conns, conns
	return &http.Client{Transport: t, Timeout: 60 * time.Second}
}

// httpError é a resposta com situação 400 ou acima.
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("situação %d: %s", e.Status, e.Body) }

// call executa a requisição. op identifica a operação no recorder (vazio não
// registra). out, se informado, recebe o JSON da resposta 2xx.
func (c *apiClient) call(ctx context.Context, op, method, path string, body any, header http.Header, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if op != "" && ctx.Err() == nil {
			c.rec.observe(op, time.Since(start), 0)
		}
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	if op != "" {
		c.rec.observe(op, time.Since(start), resp.StatusCode)
	}
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, &httpError{Status: resp.StatusCode, Body: truncate(string(data), 300)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("resposta de %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// permanentError marca uma falha que retry não deve repetir.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return permanentError{err} }

// retry repete a chamada em falhas transitórias (429, 409, 5xx e transporte),
// com espera crescente; erros marcados com permanent não se repetem. Usado
// na população dos dados, não nos cenários medidos.
func retry(ctx context.Context, attempts int, fn func() (int, error)) error {
	var err error
	for i := range attempts {
		_, err = fn()
		if err == nil {
			return nil
		}
		var pe permanentError
		if errors.As(err, &pe) {
			return err
		}
		var he *httpError
		if errors.As(err, &he) && he.Status < 500 && he.Status != http.StatusTooManyRequests && he.Status != http.StatusConflict {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 500 * time.Millisecond):
		}
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// page é a resposta das listagens por cursor.
type page[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}
