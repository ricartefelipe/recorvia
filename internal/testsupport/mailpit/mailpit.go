//go:build integration

// Package mailpit inicia um servidor SMTP real (Mailpit) com Testcontainers
// e consulta a API HTTP dele para conferir as mensagens recebidas nos testes
// de integração do envio de e-mails.
package mailpit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image é a imagem do Mailpit, igual à do Docker Compose.
const Image = "axllent/mailpit:v1.33.1"

// Server descreve o Mailpit iniciado.
type Server struct {
	SMTPAddr string
	APIURL   string
}

var (
	once    sync.Once
	shared  Server
	initErr error
)

// Start inicia (uma vez por processo de teste) o container do Mailpit.
func Start(t testing.TB) Server {
	t.Helper()
	once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ctr, err := testcontainers.Run(ctx, Image,
			testcontainers.WithExposedPorts("1025/tcp", "8025/tcp"),
			testcontainers.WithWaitStrategy(wait.ForListeningPort("1025/tcp"), wait.ForHTTP("/api/v1/info").WithPort("8025/tcp")),
		)
		if err != nil {
			initErr = fmt.Errorf("mailpit: container: %w", err)
			return
		}
		smtpAddr, err := ctr.PortEndpoint(ctx, "1025/tcp", "")
		if err != nil {
			initErr = err
			return
		}
		api, err := ctr.PortEndpoint(ctx, "8025/tcp", "http")
		if err != nil {
			initErr = err
			return
		}
		shared = Server{SMTPAddr: smtpAddr, APIURL: api}
	})
	if initErr != nil {
		t.Fatalf("mailpit: %v", initErr)
	}
	return shared
}

// Address é um endereço de e-mail na API do Mailpit.
type Address struct {
	Name    string `json:"Name"`
	Address string `json:"Address"`
}

// Summary é o resumo de uma mensagem na listagem.
type Summary struct {
	ID        string    `json:"ID"`
	MessageID string    `json:"MessageID"`
	From      Address   `json:"From"`
	To        []Address `json:"To"`
	Subject   string    `json:"Subject"`
}

// Message é uma mensagem completa.
type Message struct {
	Summary
	Text string `json:"Text"`
	HTML string `json:"HTML"`
}

func (s Server) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.APIURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mailpit: %s respondeu %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// MessagesTo lista as mensagens enviadas ao destinatário.
func (s Server) MessagesTo(t testing.TB, recipient string) []Message {
	t.Helper()
	ctx := context.Background()
	var list struct {
		Messages []Summary `json:"messages"`
	}
	if err := s.get(ctx, "/api/v1/search?query="+url.QueryEscape("to:"+recipient), &list); err != nil {
		t.Fatal(err)
	}
	out := make([]Message, 0, len(list.Messages))
	for _, m := range list.Messages {
		var full Message
		if err := s.get(ctx, "/api/v1/message/"+url.PathEscape(m.ID), &full); err != nil {
			t.Fatal(err)
		}
		out = append(out, full)
	}
	return out
}
