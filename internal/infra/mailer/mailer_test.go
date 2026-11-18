package mailer

import (
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 10, 15, 0, 0, 0, time.UTC)

func TestNewValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]Config{
		"endereço":  {Addr: "sem-porta", From: "cobranca@recorvia.test"},
		"remetente": {Addr: "localhost:25", From: "invalido"},
		"tls":       {Addr: "localhost:25", From: "cobranca@recorvia.test", TLS: "ssl"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: esperava erro", name)
		}
	}
	m, err := New(Config{Addr: "localhost:25", From: "Cobrança <cobranca@recorvia.test>"})
	if err != nil {
		t.Fatal(err)
	}
	if m.cfg.TLS != TLSStartTLS || m.cfg.Timeout != 15*time.Second || m.cfg.LocalName != "localhost" {
		t.Fatalf("padrões: %+v", m.cfg)
	}
	if got := m.MessageID("abc"); got != "<abc@recorvia.test>" {
		t.Fatalf("Message-ID: %s", got)
	}
	if c := m.tlsConfig(); c.ServerName != "localhost" || c.MinVersion == 0 {
		t.Fatalf("configuração TLS: %+v", c)
	}
}

func TestBuildMessage(t *testing.T) {
	from := mail.Address{Address: "cobranca@recorvia.test"}
	raw, err := Build(from, Message{ID: "n1", FromName: "Acme Serviços", To: []string{"a@cliente.test", "b@cliente.test"},
		Subject: "Fatura ACME-1 vencida em 05/03/2026", Text: "Olá, Ana.\nLinha com acentuação: ção.", HTML: "<p>Olá, Ana.</p>", Date: t0},
		"<n1@recorvia.test>")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	dec := new(mime.WordDecoder)
	subject, _ := dec.DecodeHeader(h.Get("Subject"))
	fromHdr, _ := mail.ParseAddress(h.Get("From"))
	if subject != "Fatura ACME-1 vencida em 05/03/2026" || fromHdr.Name != "Acme Serviços via Recorvia" || fromHdr.Address != from.Address ||
		h.Get("Message-ID") != "<n1@recorvia.test>" || h.Get("Auto-Submitted") != "auto-generated" || h.Get("MIME-Version") != "1.0" {
		t.Fatalf("cabeçalhos: %v", h)
	}
	if tos, err := h.AddressList("To"); err != nil || len(tos) != 2 {
		t.Fatalf("destinatários: %v %v", tos, err)
	}
	if d, err := h.Date(); err != nil || !d.Equal(t0) {
		t.Fatalf("data: %v %v", d, err)
	}
	_, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(quotedprintable.NewReader(p))
		parts = append(parts, p.Header.Get("Content-Type")+"|"+string(body))
	}
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "text/plain") || !strings.Contains(parts[0], "acentuação: ção.") ||
		!strings.Contains(parts[0], "Olá, Ana.\r\nLinha") || !strings.HasPrefix(parts[1], "text/html") {
		t.Fatalf("partes: %q", parts)
	}
	for _, bad := range []Message{{Subject: "a\r\nBcc: x@y.test"}, {FromName: "x\ny"}} {
		if _, err := Build(from, bad, "<id@x>"); err == nil {
			t.Errorf("quebra de linha em cabeçalho deve falhar: %+v", bad)
		}
	}
	noDate, err := Build(from, Message{To: []string{"a@x.test"}}, "<id@x>")
	if err != nil || !strings.Contains(string(noDate), "Date: ") {
		t.Fatalf("data padrão: %v", err)
	}
}

func TestSendRejectsInvalidRecipientsWithoutConnecting(t *testing.T) {
	m, _ := New(Config{Addr: "127.0.0.1:1", From: "cobranca@recorvia.test", TLS: TLSNone})
	for _, to := range [][]string{nil, {"sem-arroba"}, {"Nome <a@x.test>"}} {
		_, err := m.Send(context.Background(), Message{ID: "x", To: to})
		if !IsPermanent(err) {
			t.Errorf("destinatários %v: esperava erro permanente, obtive %v", to, err)
		}
	}
}

// smtpServer é um servidor SMTP mínimo com respostas configuráveis.
type smtpServer struct {
	ln       net.Listener
	starttls bool
	rcptCode string
	// rcptByAddr sobrepõe a resposta ao RCPT de endereços específicos.
	rcptByAddr map[string]string
	// quit define a resposta ao QUIT: vazio responde 221; "close" encerra a
	// conexão sem responder; outro valor é enviado como resposta.
	quit  string
	mu    sync.Mutex
	data  string
	rcpts []string
}

func newSMTPServer(t *testing.T, rcptCode string, starttls bool) *smtpServer {
	t.Helper()
	return startSMTPServer(t, &smtpServer{rcptCode: rcptCode, starttls: starttls})
}

func startSMTPServer(t *testing.T, s *smtpServer) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *smtpServer) received() (string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, append([]string(nil), s.rcpts...)
}

func (s *smtpServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *smtpServer) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 teste ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch cmd {
		case "EHLO", "HELO":
			if s.starttls {
				_ = tp.PrintfLine("250-teste")
				_ = tp.PrintfLine("250 STARTTLS")
			} else {
				_ = tp.PrintfLine("250 teste")
			}
		case "MAIL":
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			_, arg, _ := strings.Cut(line, ":") // RCPT TO:<endereço>
			addr := strings.Trim(arg, "<> ")
			code := s.rcptCode
			if c, ok := s.rcptByAddr[addr]; ok {
				code = c
			}
			if strings.HasPrefix(code, "250") {
				s.mu.Lock()
				s.rcpts = append(s.rcpts, addr)
				s.mu.Unlock()
			}
			_ = tp.PrintfLine("%s", code)
		case "DATA":
			_ = tp.PrintfLine("354 envie")
			b, _ := tp.ReadDotBytes()
			s.mu.Lock()
			s.data = string(b)
			s.mu.Unlock()
			_ = tp.PrintfLine("250 aceito")
		case "QUIT":
			switch s.quit {
			case "":
				_ = tp.PrintfLine("221 até logo")
			case "close":
			default:
				_ = tp.PrintfLine("%s", s.quit)
			}
			return
		default:
			_ = tp.PrintfLine("502 não implementado")
		}
	}
}

func TestSendClassifiesServerResponses(t *testing.T) {
	cases := []struct {
		name      string
		rcpt      string
		permanent bool
		ok        bool
	}{
		{"aceito", "250 ok", false, true},
		{"caixa inexistente", "550 5.1.1 caixa inexistente", true, false},
		{"temporário", "451 4.7.1 tente mais tarde", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newSMTPServer(t, c.rcpt, false)
			m, err := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSNone, Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			res, err := m.Send(context.Background(), Message{ID: "n1", To: []string{"a@cliente.test"}, Subject: "Teste", Text: "corpo", HTML: "<p>corpo</p>"})
			if c.ok {
				data, _ := srv.received()
				if err != nil || res.MessageID != "<n1@recorvia.test>" || len(res.Refused) != 0 || !strings.Contains(data, "Subject: Teste") {
					t.Fatalf("envio: %+v %v", res, err)
				}
				return
			}
			var se *SendError
			if !errors.As(err, &se) || IsPermanent(err) != c.permanent || se.Code == 0 || !strings.Contains(se.Error(), "smtp") {
				t.Fatalf("classificação: %v", err)
			}
		})
	}
}

func TestSendRequiresOfferedStartTLS(t *testing.T) {
	srv := newSMTPServer(t, "250 ok", false)
	m, _ := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSStartTLS, Timeout: 5 * time.Second})
	_, err := m.Send(context.Background(), Message{ID: "n1", To: []string{"a@cliente.test"}})
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("sem STARTTLS o envio não pode prosseguir em claro: %v", err)
	}
}

func TestSendRefusesPlainAuthWithoutTLS(t *testing.T) {
	srv := newSMTPServer(t, "250 ok", false)
	// net/smtp recusa AUTH PLAIN sem TLS para servidores que não sejam locais;
	// "localhost" é aceito, por isso o teste usa o IP com nome diferente.
	m, _ := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSNone, Username: "u", Password: "p", Timeout: 5 * time.Second})
	m.host = "smtp.externo.test"
	if _, err := m.Send(context.Background(), Message{ID: "n1", To: []string{"a@cliente.test"}}); err == nil {
		t.Fatal("autenticação sem TLS deve falhar")
	}
}

func TestSendHonorsContextDeadline(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c // não responde a saudação: o cliente deve desistir no prazo
		}
	}()
	m, _ := New(Config{Addr: ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSNone, Timeout: time.Minute})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = m.Send(ctx, Message{ID: "n1", To: []string{"a@cliente.test"}})
	if err == nil || IsPermanent(err) {
		t.Fatalf("prazo esgotado deve ser falha transitória: %v", err)
	}
	select {
	case c := <-accepted:
		_ = c.Close()
	default:
	}
}

func TestSendImplicitTLSFailsAgainstPlainServer(t *testing.T) {
	srv := newSMTPServer(t, "250 ok", false)
	m, _ := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSImplicit, Timeout: 2 * time.Second})
	if _, err := m.Send(context.Background(), Message{ID: "n1", To: []string{"a@cliente.test"}}); err == nil {
		t.Fatal("handshake TLS com servidor sem TLS deve falhar")
	}
}

func TestClassify(t *testing.T) {
	if classify(nil) != nil {
		t.Fatal("nil")
	}
	se := &SendError{Permanent: true, Err: errors.New("x")}
	if !errors.Is(classify(se), se) || !IsPermanent(se) || se.Unwrap() == nil || se.Error() != "smtp: x" {
		t.Fatal("SendError preservado")
	}
	if IsPermanent(errors.New("rede")) || IsPermanent(classify(errors.New("rede"))) {
		t.Fatal("erro de rede é transitório")
	}
}

// Recusa de parte dos destinatários no RCPT: a mensagem segue para os
// aceitos e as recusas voltam no resultado; sem nenhum aceito, o envio
// falha (permanente somente se todas as recusas forem 5xx).
func TestSendDeliversToAcceptedRecipients(t *testing.T) {
	to := []string{"a@cliente.test", "velho@cliente.test", "b@cliente.test"}
	cases := []struct {
		name      string
		byAddr    map[string]string
		accepted  []string
		refused   int
		ok        bool
		permanent bool
	}{
		{"um recusado", map[string]string{"velho@cliente.test": "550 5.1.1 caixa inexistente"},
			[]string{"a@cliente.test", "b@cliente.test"}, 1, true, false},
		{"todos recusados (5xx)", map[string]string{"a@cliente.test": "550 5.1.1 x", "velho@cliente.test": "553 5.1.3 y", "b@cliente.test": "550 5.1.1 z"},
			nil, 3, false, true},
		{"todos recusados (com 4xx)", map[string]string{"a@cliente.test": "550 5.1.1 x", "velho@cliente.test": "451 4.3.0 y", "b@cliente.test": "550 5.1.1 z"},
			nil, 3, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := startSMTPServer(t, &smtpServer{rcptCode: "250 ok", rcptByAddr: c.byAddr})
			m, _ := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSNone, Timeout: 5 * time.Second})
			res, err := m.Send(context.Background(), Message{ID: "n1", To: to, Subject: "Fatura", Text: "corpo"})
			data, rcpts := srv.received()
			if !c.ok {
				var se *SendError
				if !errors.As(err, &se) || IsPermanent(err) != c.permanent || strings.Count(err.Error(), "@cliente.test") != c.refused || data != "" {
					t.Fatalf("falha esperada: %v (dados: %q)", err, data)
				}
				return
			}
			if err != nil || res.MessageID != "<n1@recorvia.test>" || len(res.Refused) != c.refused ||
				!strings.Contains(res.Refused[0], "velho@cliente.test (550 5.1.1 caixa inexistente)") {
				t.Fatalf("envio parcial: %+v %v", res, err)
			}
			if strings.Join(rcpts, ",") != strings.Join(c.accepted, ",") || !strings.Contains(data, "Subject: Fatura") {
				t.Fatalf("destinatários aceitos %v, dados %q", rcpts, data)
			}
		})
	}
}

// Depois do aceite da mensagem (250 ao DATA), uma falha no QUIT não é falha
// de envio: tratá-la como transitória levaria ao reenvio do e-mail.
func TestSendIgnoresQuitFailureAfterAcceptedData(t *testing.T) {
	for _, quit := range []string{"close", "421 4.4.2 encerrando"} {
		t.Run(quit, func(t *testing.T) {
			srv := startSMTPServer(t, &smtpServer{rcptCode: "250 ok", quit: quit})
			m, _ := New(Config{Addr: srv.ln.Addr().String(), From: "cobranca@recorvia.test", TLS: TLSNone, Timeout: 5 * time.Second})
			res, err := m.Send(context.Background(), Message{ID: "n1", To: []string{"a@cliente.test"}, Subject: "Fatura", Text: "corpo"})
			if data, _ := srv.received(); err != nil || res.MessageID != "<n1@recorvia.test>" || !strings.Contains(data, "Subject: Fatura") {
				t.Fatalf("mensagem aceita deve contar como enviada: %+v %v", res, err)
			}
		})
	}
}
