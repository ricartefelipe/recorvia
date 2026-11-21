// Package portal implementa o acesso do cliente do tenant ao portal.
//
// A organização gera um link temporário de uso único (somente o hash do
// token é guardado). O cliente troca o link por uma sessão restrita, mantida
// no servidor e identificada por um cookie próprio, que dá acesso apenas aos
// dados daquele cliente naquela organização. A cada requisição a sessão é
// validada contra o link que a originou, o cliente e a organização: revogar
// o link, arquivar o cliente ou desativar a organização encerra o acesso.
//
// A organização lista os links de um cliente e revoga um link de forma
// explícita; a revogação também remove do armazenamento as sessões abertas
// com ele.
package portal

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
)

// ErrSessionNotFound indica sessão inexistente ou expirada no armazenamento.
var ErrSessionNotFound = errors.New("portal: sessão não encontrada")

// LinkState é a situação derivada de um link.
type LinkState string

const (
	LinkPending LinkState = "pending"
	LinkUsed    LinkState = "used"
	LinkRevoked LinkState = "revoked"
	LinkExpired LinkState = "expired"
)

// Motivos de revogação de links.
const (
	// RevokeSuperseded indica que um link mais novo do mesmo cliente o substituiu.
	RevokeSuperseded = "superseded"
	// RevokeRequested indica revogação explícita pela organização.
	RevokeRequested = "revoked"
)

// Link é um link temporário de acesso ao portal.
type Link struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	CustomerID uuid.UUID
	TokenHash  []byte
	// Autor: um membro (CreatedByUser) ou uma credencial de API
	// (CreatedByCredential) da organização.
	CreatedByUser       *uuid.UUID
	CreatedByCredential *uuid.UUID
	CreatedAt           time.Time
	ExpiresAt           time.Time
	UsedAt              *time.Time
	RevokedAt           *time.Time
	RevokeReason        string
}

// State calcula a situação no instante now. A revogação prevalece sobre o
// uso: um link usado e depois revogado não sustenta mais a sessão.
func (l Link) State(now time.Time) LinkState {
	switch {
	case l.RevokedAt != nil:
		return LinkRevoked
	case l.UsedAt != nil:
		return LinkUsed
	case !now.Before(l.ExpiresAt):
		return LinkExpired
	}
	return LinkPending
}

// LinkSummary é o link como a organização o lista: sem o token (que só
// existe na resposta da geração) e sem o hash (TokenHash fica vazio, a
// listagem não o lê).
type LinkSummary struct {
	Link
	// CreatorName é o nome do membro ou da credencial de API que gerou o
	// link.
	CreatorName string
	// Current é a situação no instante da consulta.
	Current LinkState
}

// Creator identifica quem gera o link: um membro ou uma credencial de API.
type Creator struct {
	Actor        audit.Actor
	UserID       *uuid.UUID
	CredentialID *uuid.UUID
}

// Session é a sessão restrita do portal, mantida no servidor.
type Session struct {
	ID         string
	TenantID   uuid.UUID
	CustomerID uuid.UUID
	LinkID     uuid.UUID
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// ValidCSRF compara o token recebido com o da sessão em tempo constante.
func (s Session) ValidCSRF(token string) bool {
	if token == "" || s.CSRFToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRFToken)) == 1
}

// Actor descreve o cliente na auditoria.
func (s Session) Actor() audit.Actor { return audit.CustomerPortal(s.CustomerID) }

// randomToken gera n bytes aleatórios em base64url.
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	return base64.RawURLEncoding.EncodeToString(b)
}

// validLinkToken aceita somente o formato gerado (32 bytes em base64url sem
// preenchimento), antes de qualquer consulta.
func validLinkToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	return err == nil && len(b) == 32
}
