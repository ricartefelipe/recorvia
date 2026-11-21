package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSinkRecordsHeadersAndExactBody(t *testing.T) {
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(NewSink(func() time.Time { return at }).Handler())
	defer srv.Close()

	body := `{"type":"invoice.issued","data":{"valor":"R$ 10,00"}}` + "\n"
	post := func(name, payload string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/hooks/"+name, strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("webhook-id", "evt_1")
		req.Header.Set("Webhook-Signature", "v1,abc")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }() // corpo já descartado; erro de fechamento não afeta o teste
		return resp.StatusCode
	}
	if code := post("acme", body); code != http.StatusNoContent {
		t.Fatalf("status = %d, esperado 204", code)
	}
	if code := post("beta", "{}"); code != http.StatusNoContent {
		t.Fatalf("status = %d, esperado 204", code)
	}

	list := func(query string) []Received {
		t.Helper()
		resp, err := http.Get(srv.URL + "/requests" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }() // erro de fechamento não afeta o teste
		var out []Received
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	all := list("")
	if len(all) != 2 || all[0].Seq != 1 || all[1].Seq != 2 {
		t.Fatalf("requisições = %+v, esperado duas em ordem de chegada", all)
	}
	acme := list("?name=acme")
	if len(acme) != 1 {
		t.Fatalf("filtro por nome devolveu %d requisições, esperado 1", len(acme))
	}
	got := acme[0]
	raw, err := base64.StdEncoding.DecodeString(got.BodyBase64)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Fatalf("corpo = %q, esperado os bytes exatos %q", raw, body)
	}
	if got.Headers["webhook-id"] != "evt_1" || got.Headers["webhook-signature"] != "v1,abc" {
		t.Fatalf("cabeçalhos = %v, esperado nomes em minúsculas com os valores recebidos", got.Headers)
	}
	if !got.ReceivedAt.Equal(at) || got.Name != "acme" {
		t.Fatalf("registro = %+v, esperado nome acme e o instante do relógio", got)
	}
	if empty := list("?name=outro"); len(empty) != 0 {
		t.Fatalf("nome sem requisições devolveu %d", len(empty))
	}
}

func TestSinkRejectsOversizedBodyAndUnknownRoutes(t *testing.T) {
	sink := NewSink(time.Now)
	srv := httptest.NewServer(sink.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/hooks/acme", "application/json", strings.NewReader(strings.Repeat("x", maxBody+1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, esperado 413", resp.StatusCode)
	}
	if len(sink.reqs) != 0 {
		t.Fatalf("corpo acima do limite foi gravado")
	}
	resp, err = http.Get(srv.URL + "/hooks/acme")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET em /hooks = %d, esperado 405", resp.StatusCode)
	}
}

func TestSinkKeepsOnlyTheMostRecentRequests(t *testing.T) {
	sink := NewSink(time.Now)
	h := sink.Handler()
	for range maxRequests + 3 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/acme", strings.NewReader("{}")))
	}
	if len(sink.reqs) != maxRequests {
		t.Fatalf("guardadas = %d, esperado %d", len(sink.reqs), maxRequests)
	}
	if first, last := sink.reqs[0].Seq, sink.reqs[len(sink.reqs)-1].Seq; first != 4 || last != maxRequests+3 {
		t.Fatalf("sequência guardada = %d..%d, esperado 4..%d", first, last, maxRequests+3)
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(NewSink(time.Now).Handler())
	defer srv.Close()
	if err := healthcheck(strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("healthcheck = %v, esperado sucesso", err)
	}
	down := httptest.NewServer(http.NotFoundHandler())
	defer down.Close()
	if err := healthcheck(strings.TrimPrefix(down.URL, "http://")); err == nil {
		t.Fatal("healthcheck de servidor sem /healthz deveria falhar")
	}
}
