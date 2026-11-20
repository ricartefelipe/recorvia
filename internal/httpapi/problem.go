package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
)

// problem é a resposta de erro RFC 9457.
type problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	Code      string              `json:"code"`
	RequestID string              `json:"request_id,omitempty"`
	Errors    []apperr.FieldError `json:"errors,omitempty"`
}

var statusByKind = map[apperr.Kind]int{
	apperr.KindInvalid:            http.StatusUnprocessableEntity,
	apperr.KindNotFound:           http.StatusNotFound,
	apperr.KindConflict:           http.StatusConflict,
	apperr.KindForbidden:          http.StatusForbidden,
	apperr.KindUnauthenticated:    http.StatusUnauthorized,
	apperr.KindPreconditionFailed: http.StatusPreconditionFailed,
	apperr.KindUnavailable:        http.StatusServiceUnavailable,
	apperr.KindRateLimited:        http.StatusTooManyRequests,
	apperr.KindLimitExceeded:      http.StatusForbidden,
}

// transientConflicts são conflitos transitórios: a resposta informa
// Retry-After (segundos) e não fica registrada na Idempotency-Key, para que a
// repetição com a mesma chave execute o pedido depois que a condição passar.
var transientConflicts = map[string]int{
	// Outra execução da conciliação em andamento (lease de até 15 minutos).
	"reconciliation_running": 60,
	// Reembolso anterior da cobrança com resultado desconhecido, até a nova
	// consulta ao provedor.
	"refund_outcome_unknown": 60,
}

var titles = map[int]string{
	400: "Requisição inválida", 401: "Não autenticado", 403: "Acesso negado", 404: "Não encontrado", 409: "Conflito",
	412: "Versão desatualizada", 413: "Conteúdo muito grande", 415: "Tipo de conteúdo não suportado", 422: "Dados inválidos",
	428: "Pré-condição obrigatória", 429: "Muitas requisições", 500: "Erro interno", 503: "Serviço indisponível",
}

// writeError traduz o erro em resposta estruturada sem expor detalhes
// internos; a causa completa vai para o log com o request_id.
func (a *API) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	p := problem{Code: "internal_error", Detail: "Ocorreu um erro inesperado. Informe o identificador da requisição ao suporte."}
	var maxErr *http.MaxBytesError
	if e, ok := apperr.As(err); ok {
		if s, known := statusByKind[e.Kind]; known {
			status = s
			p.Code, p.Detail, p.Errors = e.Code, e.Message, e.Fields
		}
	} else if errors.As(err, &maxErr) {
		status, p.Code, p.Detail = http.StatusRequestEntityTooLarge, "payload_too_large", "O corpo da requisição excede o limite permitido."
	}
	if p.Code == "if_match_required" {
		status = http.StatusPreconditionRequired
	}
	p.Status, p.Title = status, titles[status]
	p.Type = "https://recorvia.dev/problems/" + strings.ReplaceAll(p.Code, "_", "-")
	p.RequestID = logging.AttrValue(r.Context(), "request_id")
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	a.log.Log(r.Context(), level, "requisição com erro", slog.Int("status", status), slog.String("code", p.Code), slog.String("error", err.Error()))
	w.Header().Set("Content-Type", "application/problem+json")
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	} else if retry, ok := transientConflicts[p.Code]; ok && status == http.StatusConflict {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	w.WriteHeader(status)
	if encErr := json.NewEncoder(w).Encode(p); encErr != nil {
		a.log.Warn("resposta de erro não enviada", slog.String("error", encErr.Error()))
	}
}

func (a *API) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.log.WarnContext(r.Context(), "resposta não enviada", slog.String("error", err.Error()))
	}
}

func (a *API) writeVersioned(w http.ResponseWriter, r *http.Request, status, version int, v any) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(version)))
	a.writeJSON(w, r, status, v)
}

// decodeJSON lê o corpo com limite, rejeita campos desconhecidos e exige
// um único objeto JSON.
func decodeJSON(r *http.Request, v any) error {
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return apperr.Invalid("unsupported_media_type", "Envie o corpo como application/json.")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return apperr.Invalid("empty_body", "O corpo da requisição é obrigatório.")
		}
		return apperr.Invalid("invalid_json", "JSON inválido: "+sanitizeJSONError(err))
	}
	if dec.More() {
		return apperr.Invalid("invalid_json", "Envie um único objeto JSON.")
	}
	return nil
}

func sanitizeJSONError(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return "tipo inválido no campo " + typeErr.Field
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field") {
		return "campo desconhecido " + strings.TrimPrefix(msg, "json: unknown field ")
	}
	return "estrutura inválida"
}

// ifMatch lê a versão esperada do header If-Match ("3").
func ifMatch(r *http.Request) (int, error) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" {
		return 0, &apperr.Error{Kind: apperr.KindPreconditionFailed, Code: "if_match_required",
			Message: "Informe a versão do recurso no header If-Match."}
	}
	n, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(v, "W/"), `"`))
	if err != nil || n < 1 {
		return 0, apperr.Invalid("invalid_if_match", "Header If-Match inválido.")
	}
	return n, nil
}
