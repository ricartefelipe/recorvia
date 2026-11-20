package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Cabeçalhos das entregas (Standard Webhooks).
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
	HeaderEventType = "x-recorvia-event-type"
)

// MaxSignatures limita as assinaturas enviadas numa entrega (segredo
// corrente e anteriores ainda na janela de rotação).
const MaxSignatures = 5

func signedContent(msgID string, ts time.Time, body []byte) []byte {
	prefix := msgID + "." + strconv.FormatInt(ts.Unix(), 10) + "."
	out := make([]byte, 0, len(prefix)+len(body))
	return append(append(out, prefix...), body...)
}

// Sign calcula a assinatura "v1,<base64>" de uma entrega com um segredo.
func Sign(secret, msgID string, ts time.Time, body []byte) (string, error) {
	key, err := secretKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(signedContent(msgID, ts, body))
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// SignatureHeader assina com cada segredo válido (no máximo MaxSignatures,
// na ordem recebida) e junta as assinaturas separadas por espaço.
func SignatureHeader(secrets []string, msgID string, ts time.Time, body []byte) (string, int, error) {
	if len(secrets) == 0 {
		return "", 0, errors.New("webhooks: endpoint sem segredo válido")
	}
	secrets = secrets[:min(len(secrets), MaxSignatures)]
	sigs := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Sign(s, msgID, ts, body)
		if err != nil {
			return "", 0, err
		}
		sigs = append(sigs, sig)
	}
	return strings.Join(sigs, " "), len(sigs), nil
}

// Erros de verificação.
var (
	ErrSignatureMismatch = errors.New("webhooks: assinatura não confere")
	ErrTimestampTooOld   = errors.New("webhooks: timestamp fora da tolerância")
)

// Verify confere uma entrega recebida como o destinatário deve fazer: o
// timestamp dentro da tolerância e ao menos uma assinatura v1 válida para o
// segredo (comparação em tempo constante). Usada em testes e documentação.
func Verify(secret, msgID, timestamp, signatureHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return ErrTimestampTooOld
	}
	ts := time.Unix(sec, 0)
	if d := now.Sub(ts); d > tolerance || d < -tolerance {
		return ErrTimestampTooOld
	}
	expected, err := Sign(secret, msgID, ts, body)
	if err != nil {
		return err
	}
	for _, sig := range strings.Fields(signatureHeader) {
		if hmac.Equal([]byte(sig), []byte(expected)) {
			return nil
		}
	}
	return ErrSignatureMismatch
}
