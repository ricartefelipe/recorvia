// Package mailer envia e-mails por SMTP (Mailpit no ambiente local) com
// tempo limite por envio e TLS conforme a configuração: none (somente
// ambiente local), starttls (obrigatório: falha se o servidor não oferecer)
// ou tls (TLS implícito). A autenticação PLAIN só é usada sobre conexão
// cifrada (ou com servidor local, conforme net/smtp).
//
// As mensagens são multipart/alternative (texto e HTML) em UTF-8 com
// quoted-printable, Message-ID estável por notificação e cabeçalho
// Auto-Submitted. Assunto e nomes passam por codificação RFC 2047 e não
// aceitam quebras de linha (sem injeção de cabeçalhos).
//
// Destinatários recusados pelo servidor no RCPT não impedem a entrega aos
// demais: a mensagem segue para os aceitos e as recusas voltam no
// resultado; o envio só falha se nenhum destinatário for aceito (de forma
// permanente quando todas as recusas forem 5xx). Depois que o servidor
// aceita a mensagem (resposta ao DATA), uma falha no QUIT não desfaz o envio.
package mailer

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
)

// Modos de TLS aceitos.
const (
	TLSNone     = "none"
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
)

// Config descreve o servidor SMTP.
type Config struct {
	Addr     string
	From     string
	Username string
	Password string
	TLS      string
	Timeout  time.Duration
	// TLSConfig permite raízes próprias em testes; ServerName é preenchido.
	TLSConfig *tls.Config
	// LocalName é o nome anunciado no EHLO (padrão: localhost).
	LocalName string
}

// Message é um e-mail a enviar.
type Message struct {
	// ID gera o Message-ID estável (<ID@domínio do remetente>).
	ID       string
	FromName string
	To       []string
	Subject  string
	Text     string
	HTML     string
	Date     time.Time
}

// Result é o resultado de um envio aceito pelo servidor.
type Result struct {
	MessageID string
	// Refused descreve os destinatários recusados no RCPT, no formato
	// "endereço (código mensagem)"; os demais receberam a mensagem.
	Refused []string
}

// Mailer envia mensagens pelo servidor configurado.
type Mailer struct {
	cfg  Config
	from mail.Address
	host string
}

// New valida a configuração.
func New(cfg Config) (*Mailer, error) {
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("mailer: endereço SMTP inválido %q", cfg.Addr)
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("mailer: remetente inválido: %w", err)
	}
	switch cfg.TLS {
	case TLSNone, TLSStartTLS, TLSImplicit:
	case "":
		cfg.TLS = TLSStartTLS
	default:
		return nil, fmt.Errorf("mailer: modo TLS desconhecido %q", cfg.TLS)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.LocalName == "" {
		cfg.LocalName = "localhost"
	}
	return &Mailer{cfg: cfg, from: mail.Address{Address: from.Address}, host: host}, nil
}

// SendError classifica falhas: Permanent para respostas 5xx do servidor.
type SendError struct {
	Code      int
	Permanent bool
	Err       error
}

func (e *SendError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("smtp %d: %v", e.Code, e.Err)
	}
	return "smtp: " + e.Err.Error()
}

func (e *SendError) Unwrap() error { return e.Err }

// IsPermanent informa se a falha não deve ser repetida.
func IsPermanent(err error) bool {
	var se *SendError
	return errors.As(err, &se) && se.Permanent
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var se *SendError
	if errors.As(err, &se) {
		return err
	}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		return &SendError{Code: tp.Code, Permanent: tp.Code >= 500, Err: errors.New(tp.Msg)}
	}
	return &SendError{Err: err}
}

func (m *Mailer) tlsConfig() *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12}
	if m.cfg.TLSConfig != nil {
		c = m.cfg.TLSConfig.Clone()
	}
	if c.ServerName == "" {
		c.ServerName = m.host
	}
	return c
}

// MessageID devolve o identificador estável da mensagem.
func (m *Mailer) MessageID(id string) string {
	domain := m.from.Address[strings.LastIndex(m.from.Address, "@")+1:]
	return "<" + id + "@" + domain + ">"
}

// Send entrega a mensagem e devolve o Message-ID e as recusas de
// destinatários. O prazo é o menor entre o tempo limite configurado e o do
// contexto.
func (m *Mailer) Send(ctx context.Context, msg Message) (Result, error) {
	if len(msg.To) == 0 {
		return Result{}, &SendError{Permanent: true, Err: errors.New("mensagem sem destinatários")}
	}
	for _, to := range msg.To {
		if a, err := mail.ParseAddress(to); err != nil || a.Address != to {
			return Result{}, &SendError{Permanent: true, Err: fmt.Errorf("destinatário inválido %q", to)}
		}
	}
	id := m.MessageID(msg.ID)
	data, err := Build(m.from, msg, id)
	if err != nil {
		return Result{}, &SendError{Permanent: true, Err: err}
	}
	deadline := time.Now().Add(m.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	refused, err := m.deliver(ctx, deadline, msg.To, data)
	if err != nil {
		return Result{}, classify(err)
	}
	return Result{MessageID: id, Refused: refused}, nil
}

func (m *Mailer) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{}
	if m.cfg.TLS == TLSImplicit {
		return (&tls.Dialer{NetDialer: d, Config: m.tlsConfig()}).DialContext(ctx, "tcp", m.cfg.Addr)
	}
	return d.DialContext(ctx, "tcp", m.cfg.Addr)
}

func (m *Mailer) deliver(ctx context.Context, deadline time.Time, to []string, data []byte) ([]string, error) {
	conn, err := m.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	// O cancelamento do contexto interrompe a conversa SMTP em andamento.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	c, err := smtp.NewClient(conn, m.host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello(m.cfg.LocalName); err != nil {
		return nil, err
	}
	if m.cfg.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return nil, &SendError{Permanent: false, Err: errors.New("o servidor não oferece STARTTLS")}
		}
		if err := c.StartTLS(m.tlsConfig()); err != nil {
			return nil, err
		}
	}
	if m.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.host)); err != nil {
			return nil, err
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return nil, err
	}
	refused, err := addRecipients(c, to)
	if err != nil {
		return nil, err
	}
	w, err := c.Data()
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	// O servidor já aceitou a mensagem (resposta ao DATA): uma falha no QUIT
	// (prazo, cancelamento ou conexão encerrada) não desfaz o envio, e tratá-la
	// como falha levaria a um reenvio.
	_ = c.Quit()
	return refused, nil
}

// addRecipients envia o RCPT de cada destinatário. Recusas do servidor não
// interrompem os demais e são devolvidas; sem nenhum destinatário aceito, o
// envio falha (permanente se todas as recusas forem 5xx). Erros de conexão
// interrompem o envio.
func addRecipients(c *smtp.Client, to []string) ([]string, error) {
	var refused []string
	accepted, code, permanent := 0, 0, true
	for _, r := range to {
		err := c.Rcpt(r)
		var tp *textproto.Error
		switch {
		case err == nil:
			accepted++
			continue
		case !errors.As(err, &tp):
			return nil, err
		}
		if code == 0 {
			code = tp.Code
		}
		permanent = permanent && tp.Code >= 500
		refused = append(refused, fmt.Sprintf("%s (%d %s)", r, tp.Code, tp.Msg))
	}
	if accepted == 0 {
		return nil, &SendError{Code: code, Permanent: permanent, Err: errors.New("destinatários recusados: " + strings.Join(refused, "; "))}
	}
	return refused, nil
}

// Build monta a mensagem MIME (cabeçalhos e partes texto e HTML).
func Build(from mail.Address, msg Message, messageID string) ([]byte, error) {
	for _, v := range []string{msg.Subject, msg.FromName, messageID} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, errors.New("mailer: quebra de linha em cabeçalho")
		}
	}
	if msg.Date.IsZero() {
		msg.Date = time.Now()
	}
	sender := from
	if msg.FromName != "" {
		sender.Name = msg.FromName + " via Recorvia"
	}
	to := make([]string, len(msg.To))
	for i, r := range msg.To {
		to[i] = (&mail.Address{Address: r}).String()
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct{ ctype, content string }{{"text/plain; charset=utf-8", msg.Text}, {"text/html; charset=utf-8", msg.HTML}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{"Content-Type": {part.ctype}, "Content-Transfer-Encoding": {"quoted-printable"}})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(part.content, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", sender.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", msg.Subject))
	header("Date", msg.Date.UTC().Format(time.RFC1123Z))
	header("Message-ID", messageID)
	header("MIME-Version", "1.0")
	header("Auto-Submitted", "auto-generated")
	header("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
	b.WriteString("\r\n")
	b.Write(body.Bytes())
	return b.Bytes(), nil
}
