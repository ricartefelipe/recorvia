package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestEncryptDecryptAndRotation(t *testing.T) {
	old, err := NewKeyring(map[string][]byte{"k1": key(1)}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	ct, kid, err := old.Encrypt([]byte("segredo"), []byte("tenant-a/asaas"))
	if err != nil || kid != "k1" {
		t.Fatalf("encrypt: %v %s", err, kid)
	}
	rotated, err := NewKeyring(map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.ActiveKeyID() != "k2" {
		t.Fatal("chave ativa incorreta")
	}
	pt, err := rotated.Decrypt(ct, kid, []byte("tenant-a/asaas"))
	if err != nil || string(pt) != "segredo" {
		t.Fatalf("decrypt após rotação: %q %v", pt, err)
	}
	if _, err := rotated.Decrypt(ct, kid, []byte("tenant-b/asaas")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("AAD divergente deve falhar")
	}
	if _, err := rotated.Decrypt(ct, "k9", nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatal("chave desconhecida deve falhar")
	}
	if _, err := rotated.Decrypt([]byte{1, 2}, "k1", nil); !errors.Is(err, ErrDecrypt) {
		t.Fatal("ciphertext curto deve falhar")
	}
	tampered := bytes.Clone(ct)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := rotated.Decrypt(tampered, "k1", []byte("tenant-a/asaas")); !errors.Is(err, ErrDecrypt) {
		t.Fatal("ciphertext adulterado deve falhar")
	}
	ct2, _, _ := old.Encrypt([]byte("segredo"), nil)
	if bytes.Equal(ct, ct2) {
		t.Fatal("nonce deve variar a cada cifragem")
	}
}

func TestNewKeyringValidation(t *testing.T) {
	if _, err := NewKeyring(map[string][]byte{"k1": []byte("curta")}, "k1"); err == nil {
		t.Fatal("chave curta deve falhar")
	}
	if _, err := NewKeyring(map[string][]byte{"k1": key(1)}, "k2"); err == nil {
		t.Fatal("chave ativa ausente deve falhar")
	}
}
