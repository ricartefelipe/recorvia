package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/pgstore"
)

const (
	idempotencyLease = time.Minute
	idempotencyTTL   = 24 * time.Hour
)

type idemKey struct{}

// idempotencyKeyFrom retorna a chave de negócio derivada do Idempotency-Key
// (ver businessKey), usada pelos casos de uso como proteção persistente
// contra duplicidade.
func idempotencyKeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(idemKey{}).(string)
	return k
}

// businessKey deriva a chave de negócio gravada pelos casos de uso
// (payments.request_key, customer_credit_entries.request_key e
// credit_notes.request_key): hex(SHA-256(cliente|operação|chave)). O tamanho
// é fixo (64 caracteres) para qualquer Idempotency-Key aceito, o que mantém a
// chave dentro do limite das colunas, e o escopo por cliente e operação
// impede que a mesma chave colida entre usuários ou rotas diferentes.
func businessKey(clientID, operation, key string) string {
	sum := sha256.Sum256([]byte(clientID + "|" + operation + "|" + key))
	return hex.EncodeToString(sum[:])
}

func validIdempotencyKey(k string) bool {
	if len(k) < 8 || len(k) > 255 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

type bufferedWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header         { return b.header }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

// idempotency implementa Idempotency-Key com escopo de tenant, cliente da
// API e operação:
//   - repetição equivalente (mesma impressão do pedido) devolve o resultado
//     registrado: status, corpo, Content-Type e o header Location, quando a
//     resposta original o trazia;
//   - mesma chave com conteúdo diferente gera 409;
//   - pedido concorrente com a mesma chave gera 409 enquanto o primeiro
//     estiver em processamento;
//   - processamento interrompido (lease expirado) pode ser assumido por nova
//     tentativa; os casos de uso usam a chave de negócio para não duplicar
//     efeitos já confirmados. Cada execução tem um token próprio, e só a que
//     detém a chave registra o resultado ou a libera.
func (a *API) idempotency(info RouteInfo, next http.Handler) http.Handler {
	operation := info.Method + " " + info.Pattern
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := mustPrincipal(r)
		key := r.Header.Get("Idempotency-Key")
		if !validIdempotencyKey(key) {
			a.writeError(w, r, apperr.Invalid("idempotency_key_required", "Informe o header Idempotency-Key (8 a 255 caracteres visíveis)."))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h := sha256.New()
		h.Write([]byte(r.Method + "\n" + r.URL.Path + "\n"))
		h.Write(body)
		fingerprint := h.Sum(nil)
		// Token próprio desta execução: só ela registra o resultado ou libera
		// a chave, mesmo que outra assuma o processamento depois do lease.
		k := pgstore.IdempotencyKey{TenantID: p.TenantID, ClientID: p.ClientID(), Operation: operation, Key: key, Lease: ids.New()}

		rec, won, err := a.Store.IdempotencyBegin(r.Context(), k, fingerprint, idempotencyLease, idempotencyTTL)
		if err != nil {
			a.writeError(w, r, err)
			return
		}
		if !won {
			switch {
			case !bytes.Equal(rec.RequestHash, fingerprint):
				a.writeError(w, r, apperr.Conflict("idempotency_key_reused", "Esta Idempotency-Key já foi usada com outro conteúdo."))
				return
			case rec.Completed:
				w.Header().Set("Content-Type", rec.ResponseCT)
				// O endereço do recurso faz parte do resultado: a repetição
				// o devolve como a resposta original.
				if rec.ResponseLocation != "" {
					w.Header().Set("Location", rec.ResponseLocation)
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(rec.ResponseStatus)
				if _, err := w.Write(rec.ResponseBody); err != nil {
					a.log.WarnContext(r.Context(), "resposta repetida não enviada", slog.String("error", err.Error()))
				}
				return
			case rec.LockedUntil != nil && rec.LockedUntil.After(a.Clock.Now()):
				w.Header().Set("Retry-After", strconv.Itoa(int(time.Until(*rec.LockedUntil).Seconds())+1))
				a.writeError(w, r, apperr.Conflict("idempotency_in_progress", "Um pedido com esta Idempotency-Key ainda está em processamento."))
				return
			default:
				ok, err := a.Store.IdempotencyTakeOver(r.Context(), k, idempotencyLease)
				if err != nil {
					a.writeError(w, r, err)
					return
				}
				if !ok {
					a.writeError(w, r, apperr.Conflict("idempotency_in_progress", "Um pedido com esta Idempotency-Key ainda está em processamento."))
					return
				}
			}
		}
		bw := &bufferedWriter{header: http.Header{}}
		ctx := context.WithValue(r.Context(), idemKey{}, businessKey(k.ClientID, operation, key))
		next.ServeHTTP(bw, r.WithContext(ctx))
		if bw.status == 0 {
			bw.status = http.StatusOK
		}
		// Falhas transitórias (5xx, 429 e respostas com Retry-After, como o
		// conflito com outra conciliação em andamento) ou de autorização
		// liberam a chave para nova tentativa; resultados definitivos ficam
		// registrados. O pedido aceito e ainda em processamento (202, como a
		// conciliação executada pelo worker) também libera a chave: o retrato
		// "em andamento" não é o resultado, e a repetição com a mesma chave
		// volta ao caso de uso, que devolve o mesmo recurso pela chave de
		// negócio, na situação atual.
		persistCtx := context.WithoutCancel(r.Context())
		if bw.status >= 500 || bw.status == http.StatusUnauthorized || bw.status == http.StatusForbidden || bw.status == http.StatusTooManyRequests ||
			bw.status == http.StatusAccepted || bw.header.Get("Retry-After") != "" {
			if err := a.Store.IdempotencyRelease(persistCtx, k); err != nil {
				a.log.WarnContext(r.Context(), "liberação de chave de idempotência falhou", slog.String("error", err.Error()))
			}
		} else if err := a.Store.IdempotencyComplete(persistCtx, k, bw.status, bw.buf.Bytes(), bw.header.Get("Content-Type"),
			bw.header.Get("Location")); errors.Is(err, pgstore.ErrIdempotencyLeaseLost) {
			// Outra execução assumiu a chave depois do lease: o resultado dela
			// é o registrado; esta resposta vai só para o próprio cliente.
			a.log.WarnContext(r.Context(), "resultado idempotente não registrado: a chave foi assumida por outra execução")
		} else if err != nil {
			a.log.ErrorContext(r.Context(), "registro do resultado idempotente falhou", slog.String("error", err.Error()))
		}
		for name, vals := range bw.header {
			for _, v := range vals {
				w.Header().Add(name, v)
			}
		}
		w.WriteHeader(bw.status)
		if _, err := w.Write(bw.buf.Bytes()); err != nil {
			a.log.WarnContext(r.Context(), "resposta não enviada", slog.String("error", err.Error()))
		}
	})
}
