// Package redact remove credenciais de URLs de conexão e de mensagens de
// erro antes que elas cheguem aos logs (RNF-12).
//
// A redação dos logs por nome de chave (internal/infra/logging) não alcança
// o texto de um erro: bibliotecas como net/url, go-redis e amqp091 repetem a
// URL recebida inteira, com a senha, quando ela é malformada. As funções
// daqui saneiam as mensagens na origem, nos adaptadores que abrem conexões
// (PostgreSQL, Redis, RabbitMQ e SMTP).
package redact

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Mask é o texto que substitui senhas e segredos.
const Mask = "xxxxx"

// URL devolve raw com a senha substituída por Mask. Vale também para URLs
// malformadas, que net/url não interpreta (ver credentialSpan). Sem
// credenciais, devolve raw; sem esquema, trata raw como string de conexão
// chave=valor.
func URL(raw string) string {
	if !strings.Contains(raw, "://") {
		return redactKeyValue(raw)
	}
	return maskURLs(raw)
}

// minLiteralSecret é o tamanho mínimo de um segredo para que ele seja
// substituído onde aparecer no texto: segredos curtos ("p") corromperiam
// trechos comuns ("amqps://"). Senhas curtas continuam mascaradas na posição
// da URL.
const minLiteralSecret = 8

// maxLookahead limita a busca pelo "@" que encerra uma senha com caracteres
// reservados sem codificação.
const maxLookahead = 256

// authorityEnd são os caracteres que encerram a autoridade de uma URL num
// texto: início do caminho, consulta, fragmento, espaço ou aspas.
const authorityEnd = "/?#\"'`<> \t\r\n"

// credentialSpan localiza as credenciais logo após "://" (rest). userEnd é a
// posição do ":" que separa usuário e senha e at a do "@" que encerra as
// credenciais; ok é falso sem senha.
//
// Só a autoridade (o trecho antes do primeiro "/", "?", "#", espaço ou
// aspas) é tratada como credenciais: um "@" no caminho ou na consulta
// (?email=a@b) não é senha. Se a autoridade não tem "@" mas tem um ":"
// seguido de algo que não é porta numérica (como em
// "usuario:Ab3/Cd@host"), a senha contém caracteres reservados sem
// codificação: as credenciais vão até o primeiro "@" seguinte na mesma
// linha. Na dúvida, mascara-se mais do que o necessário.
func credentialSpan(rest string) (userEnd, at int, ok bool) {
	end := len(rest)
	if i := strings.IndexAny(rest, authorityEnd); i >= 0 {
		end = i
	}
	authority := rest[:end]
	if at = strings.LastIndex(authority, "@"); at >= 0 {
		userEnd = strings.Index(authority[:at], ":")
		return userEnd, at, userEnd >= 0
	}
	colon := strings.Index(authority, ":")
	if colon < 0 || isPort(hostPort(authority)) {
		return 0, 0, false
	}
	tail := rest[end:min(len(rest), end+maxLookahead)]
	if i := strings.IndexAny(tail, "\r\n"); i >= 0 {
		tail = tail[:i]
	}
	if i := strings.Index(tail, "://"); i >= 0 {
		tail = tail[:i]
	}
	i := strings.Index(tail, "@")
	if i < 0 {
		return 0, 0, false
	}
	return colon, end + i, true
}

// hostPort devolve o trecho depois do último ":" da autoridade (a porta,
// se a autoridade for host:porta), respeitando endereços IPv6 entre
// colchetes.
func hostPort(authority string) string {
	if strings.HasPrefix(authority, "[") {
		if i := strings.Index(authority, "]"); i >= 0 {
			authority = authority[i+1:]
			if !strings.HasPrefix(authority, ":") {
				return ""
			}
		}
	}
	return authority[strings.LastIndex(authority, ":")+1:]
}

// isPort informa se p é vazio ou uma porta numérica (a autoridade é
// host:porta, sem senha).
func isPort(p string) bool {
	for _, r := range p {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// schemeStart encontra o início de URLs num texto.
var schemeStart = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://`)

// maskURLs mascara a senha de cada URL contida em s.
func maskURLs(s string) string {
	var b strings.Builder
	for {
		loc := schemeStart.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:loc[1]])
		rest := s[loc[1]:]
		userEnd, at, ok := credentialSpan(rest)
		if !ok {
			s = rest
			continue
		}
		b.WriteString(rest[:userEnd] + ":" + Mask)
		s = rest[at:]
	}
}

// password devolve a senha de uma URL de conexão (vazia se não houver).
func password(rawURL string) string {
	_, rest, ok := strings.Cut(rawURL, "://")
	if !ok {
		return ""
	}
	userEnd, at, ok := credentialSpan(rest)
	if !ok {
		return ""
	}
	return rest[userEnd+1 : at]
}

// keyValuePassword encontra password=... em strings de conexão no formato
// chave=valor do PostgreSQL.
var keyValuePassword = regexp.MustCompile(`(?i)(password\s*=\s*)('(?:[^'\\]|\\.)*'|\S+)`)

func redactKeyValue(s string) string {
	return keyValuePassword.ReplaceAllString(s, "${1}"+Mask)
}

// String devolve s sem os segredos informados, sem senhas de URLs e sem
// password=... de strings de conexão. Cada segredo pode ser uma URL de
// conexão (são removidas a URL inteira e a senha nela contida, nas formas
// codificada e decodificada) ou um valor literal (uma senha). Segredos
// literais com menos de minLiteralSecret caracteres só são mascarados na
// posição da URL.
func String(s string, secrets ...string) string {
	for _, secret := range secrets {
		switch {
		case secret == "":
		case strings.Contains(secret, "://"):
			s = strings.ReplaceAll(s, secret, URL(secret))
			s = replaceSecret(s, password(secret))
		case strings.Contains(strings.ToLower(secret), "password"):
			s = strings.ReplaceAll(s, secret, redactKeyValue(secret))
		default:
			s = replaceSecret(s, secret)
		}
	}
	return redactKeyValue(maskURLs(s))
}

// replaceSecret substitui o segredo literal e, se for diferente, a sua
// forma decodificada (%40 -> @), quando ele tem o tamanho mínimo.
func replaceSecret(s, secret string) string {
	if len(secret) < minLiteralSecret {
		return s
	}
	s = strings.ReplaceAll(s, secret, Mask)
	if dec, err := url.PathUnescape(secret); err == nil && dec != secret && len(dec) >= minLiteralSecret {
		s = strings.ReplaceAll(s, dec, Mask)
	}
	return s
}

// sanitized é um erro com a mensagem saneada; a causa continua acessível
// para errors.Is e errors.As.
type sanitized struct {
	msg string
	err error
}

func (e *sanitized) Error() string { return e.msg }
func (e *sanitized) Unwrap() error { return e.err }

// Error devolve err com a mensagem saneada por String. errors.Is e
// errors.As continuam funcionando pela causa original. Devolve nil para
// err nil.
func Error(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	return &sanitized{msg: String(err.Error(), secrets...), err: err}
}

// ParseError descreve a falha ao interpretar uma URL de conexão sem repetir
// o texto da biblioteca. Com credenciais na URL, o motivo dado por net/url
// pode conter trechos da senha (escape inválido, ou a parte da senha depois
// de um "/" sem codificação lida como porta: `invalid port ":Ab3dEf"`): a
// mensagem traz só a URL mascarada e a orientação de codificar os
// caracteres reservados. Sem credenciais, o motivo é anexado. A causa não é
// encadeada, para que nenhum caminho de formatação chegue à mensagem
// original.
func ParseError(component, raw string, err error) error {
	msg := component + ": URL inválida (" + String(raw, raw) + ")"
	if strings.Contains(raw, "@") {
		return errors.New(msg + ": use esquema://usuário:senha@host:porta/caminho e codifique na senha os caracteres reservados " +
			"(%2F para /, %3F para ?, %23 para #, %40 para @, %3A para :, %25 para %, %20 para espaço)")
	}
	var urlErr *url.Error
	switch {
	case errors.As(err, &urlErr) && urlErr.Err != nil:
		msg += ": " + String(urlErr.Err.Error(), raw)
	case err != nil:
		msg += ": " + String(err.Error(), raw)
	}
	return errors.New(msg)
}
