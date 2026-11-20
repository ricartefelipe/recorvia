// Package secrets cifra credenciais em repouso com AES-256-GCM. Cada
// ciphertext registra o identificador da chave, permitindo rotação: novas
// cifragens usam a chave ativa e chaves antigas continuam decifrando até a
// recifragem. O AAD vincula o ciphertext ao seu contexto (tenant e
// finalidade), impedindo que seja copiado para outro registro.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// ErrUnknownKey indica ciphertext cifrado com chave não configurada.
var ErrUnknownKey = errors.New("secrets: chave desconhecida")

// ErrDecrypt indica ciphertext adulterado ou AAD divergente.
var ErrDecrypt = errors.New("secrets: falha ao decifrar")

// Keyring guarda as chaves configuradas.
type Keyring struct {
	aeads  map[string]cipher.AEAD
	active string
}

// NewKeyring cria o keyring a partir de chaves de 32 bytes.
func NewKeyring(keys map[string][]byte, active string) (*Keyring, error) {
	k := &Keyring{aeads: map[string]cipher.AEAD{}, active: active}
	for id, key := range keys {
		if len(key) != 32 {
			return nil, fmt.Errorf("secrets: chave %q deve ter 32 bytes", id)
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		k.aeads[id] = aead
	}
	if _, ok := k.aeads[active]; !ok {
		return nil, fmt.Errorf("secrets: chave ativa %q não configurada", active)
	}
	return k, nil
}

// ActiveKeyID retorna a chave usada nas novas cifragens.
func (k *Keyring) ActiveKeyID() string { return k.active }

// Encrypt cifra com a chave ativa. Formato: nonce || ciphertext || tag.
func (k *Keyring) Encrypt(plaintext, aad []byte) ([]byte, string, error) {
	aead := k.aeads[k.active]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), k.active, nil
}

// Decrypt decifra com a chave indicada.
func (k *Keyring) Decrypt(ciphertext []byte, keyID string, aad []byte) ([]byte, error) {
	aead, ok := k.aeads[keyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrDecrypt
	}
	nonce, body := ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():]
	out, err := aead.Open(nil, nonce, body, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}
