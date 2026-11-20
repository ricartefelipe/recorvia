// Package apicredentials modela as credenciais de API (máquina a máquina)
// de uma organização.
//
// O token é exibido uma única vez, no formato rcv_<identificador>_<segredo>:
// o identificador (80 bits, público) localiza a credencial e o segredo (256
// bits aleatórios) a autentica. Somente o SHA-256 do segredo é persistido e
// a comparação é feita em tempo constante. Os escopos são um subconjunto das
// permissões de quem emite a credencial e valem apenas no tenant ao qual ela
// pertence.
package apicredentials

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

const (
	// TokenPrefix identifica tokens do Recorvia (inclusive em varreduras de
	// segredos vazados).
	TokenPrefix = "rcv_"
	// KeyIDLength é o tamanho do identificador público (base32, 80 bits).
	KeyIDLength = 16
	// SecretLength é o tamanho do segredo (base32 sem preenchimento, 256 bits).
	SecretLength = 52
	// TokenLength é o tamanho total do token.
	TokenLength = len(TokenPrefix) + KeyIDLength + 1 + SecretLength

	keyIDBytes  = 10
	secretBytes = 32

	// RotationGrace é o período em que o segredo anterior continua válido
	// após a rotação, para a troca sem indisponibilidade na integração.
	RotationGrace = 24 * time.Hour
	// MaxExpiresInDays é a validade máxima informada na criação.
	MaxExpiresInDays = 730
	// MaxScopes limita a quantidade de escopos por credencial.
	MaxScopes = 64
	// MaxActivePerTenant limita as credenciais ativas por organização.
	MaxActivePerTenant = 50
	// LastUsedResolution é o intervalo mínimo entre registros de último uso.
	LastUsedResolution = time.Minute
)

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

func randomBase32(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read não falha a partir do Go 1.24
	return strings.ToLower(encoding.EncodeToString(b))
}

// newKeyID gera o identificador público da credencial.
func newKeyID() string { return randomBase32(keyIDBytes) }

// newSecret gera um segredo aleatório de 256 bits.
func newSecret() string { return randomBase32(secretBytes) }

// FormatToken monta o token exibido uma única vez.
func FormatToken(keyID, secret string) string { return TokenPrefix + keyID + "_" + secret }

func base32Lower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// ParseToken separa identificador e segredo. Somente o formato exato é
// aceito (tamanho, prefixo, separador e alfabeto base32 em minúsculas), sem
// normalização: o segredo comparado é exatamente o recebido.
func ParseToken(token string) (keyID, secret string, ok bool) {
	if len(token) != TokenLength || !strings.HasPrefix(token, TokenPrefix) {
		return "", "", false
	}
	rest := token[len(TokenPrefix):]
	if rest[KeyIDLength] != '_' {
		return "", "", false
	}
	keyID, secret = rest[:KeyIDLength], rest[KeyIDLength+1:]
	if !base32Lower(keyID) || !base32Lower(secret) {
		return "", "", false
	}
	return keyID, secret, true
}

// HashSecret calcula o hash persistido do segredo. O segredo tem 256 bits
// aleatórios; um hash rápido basta, pois não há espaço de busca a explorar.
func HashSecret(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

// State é a situação derivada da credencial.
type State string

const (
	StateActive  State = "active"
	StateExpired State = "expired"
	StateRevoked State = "revoked"
)

// Credential é uma credencial de API de uma organização.
type Credential struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	Name               string
	KeyID              string
	SecretHash         []byte
	PreviousSecretHash []byte
	// PreviousSecretExpiresAt encerra a janela de transição da rotação.
	PreviousSecretExpiresAt *time.Time
	Scopes                  []tenancy.Permission
	CreatedBy               uuid.UUID
	CreatedAt               time.Time
	ExpiresAt               *time.Time
	RotatedBy               *uuid.UUID
	RotatedAt               *time.Time
	LastUsedAt              *time.Time
	RevokedBy               *uuid.UUID
	RevokedAt               *time.Time
	Version                 int
}

// Prefix é a parte pública do token (exibida nas listagens).
func (c Credential) Prefix() string { return TokenPrefix + c.KeyID }

// Issuer é o membro responsável pelo segredo vigente: quem rotacionou por
// último ou, sem rotação, quem criou.
func (c Credential) Issuer() uuid.UUID {
	if c.RotatedBy != nil {
		return *c.RotatedBy
	}
	return c.CreatedBy
}

// State calcula a situação no instante now.
func (c Credential) State(now time.Time) State {
	switch {
	case c.RevokedAt != nil:
		return StateRevoked
	case c.ExpiresAt != nil && !now.Before(*c.ExpiresAt):
		return StateExpired
	}
	return StateActive
}

// constantTimeCompare é a comparação usada na verificação do segredo
// (variável apenas para que os testes confirmem o uso e a quantidade de
// comparações).
var constantTimeCompare = subtle.ConstantTimeCompare

// MatchSecret compara o segredo apresentado com o vigente e, durante a
// janela de rotação, com o anterior. As duas comparações são sempre feitas,
// sobre hashes de mesmo tamanho e em tempo constante, de modo que o tempo de
// resposta não indica qual hash foi comparado nem quantos bytes coincidiram.
func (c Credential) MatchSecret(secret string, now time.Time) bool {
	h := HashSecret(secret)
	currentHash := c.SecretHash
	if len(currentHash) != len(h) {
		currentHash = make([]byte, len(h))
	}
	previousHash := c.PreviousSecretHash
	previousValid := len(previousHash) == len(h) && c.PreviousSecretExpiresAt != nil && now.Before(*c.PreviousSecretExpiresAt)
	if !previousValid {
		previousHash = make([]byte, len(h))
	}
	current := constantTimeCompare(h, currentHash) == 1 && len(c.SecretHash) == len(h)
	previous := constantTimeCompare(h, previousHash) == 1 && previousValid
	return current || previous
}

// nonDelegable são permissões que não podem ser concedidas a credenciais:
// a gestão de membros e do plano da plataforma exige uma pessoa
// identificada, e uma credencial não emite nem administra outras
// credenciais (o que permitiria perpetuar o acesso sem um membro).
var nonDelegable = []tenancy.Permission{tenancy.PermMembersManage, tenancy.PermPlatformManage, tenancy.PermAPICredentials}

// NonDelegable lista as permissões que nunca viram escopo de credencial.
func NonDelegable() []tenancy.Permission { return slices.Clone(nonDelegable) }

// Delegable informa se a permissão pode ser escopo de uma credencial.
func Delegable(p tenancy.Permission) bool { return !slices.Contains(nonDelegable, p) }

// EffectiveScopes restringe os escopos às permissões atuais do papel de quem
// emitiu o segredo vigente: rebaixar esse membro reduz a credencial na
// mesma medida, sem regravá-la.
func EffectiveScopes(scopes []tenancy.Permission, issuer tenancy.Role) []tenancy.Permission {
	out := make([]tenancy.Permission, 0, len(scopes))
	for _, s := range scopes {
		if issuer.Can(s) && Delegable(s) {
			out = append(out, s)
		}
	}
	return out
}

// Principal é a identidade autenticada por uma credencial em uma requisição.
type Principal struct {
	CredentialID uuid.UUID
	TenantID     uuid.UUID
	Name         string
	KeyID        string
	// Scopes são os escopos efetivos (ver EffectiveScopes).
	Scopes []tenancy.Permission
}

// Can informa se os escopos efetivos concedem a permissão.
func (p Principal) Can(perm tenancy.Permission) bool { return slices.Contains(p.Scopes, perm) }

// Actor descreve a credencial na auditoria.
func (p Principal) Actor() audit.Actor { return audit.APICredential(p.CredentialID, p.Name) }

// ClientID identifica a credencial como cliente da API (escopo das chaves de
// idempotência). É estável na rotação, que preserva o identificador.
func (p Principal) ClientID() string { return "api_credential:" + p.CredentialID.String() }
