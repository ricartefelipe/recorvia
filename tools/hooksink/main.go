// Comando hooksink é um receptor de webhooks para exercícios operacionais
// (deploy/backup/dr-check.sh): faz o papel do sistema do tenant que recebe as
// entregas do Recorvia. Grava em memória os cabeçalhos e o corpo de cada
// requisição recebida em POST /hooks/{name} e os devolve em GET /requests,
// para que o exercício confira a assinatura com o segredo do endpoint. Não
// faz parte do produto e não deve ser exposto fora de ambientes de teste.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Limites do receptor: corpo por requisição e requisições guardadas.
const (
	maxBody     = 1 << 20
	maxRequests = 5000
)

// Received é uma requisição recebida. O corpo vem em base64 para que a
// assinatura seja conferida sobre os bytes exatos.
type Received struct {
	Seq        int               `json:"seq"`
	Name       string            `json:"name"`
	ReceivedAt time.Time         `json:"received_at"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"body_base64"`
}

// Sink guarda as requisições recebidas, na ordem de chegada.
type Sink struct {
	now func() time.Time

	mu   sync.Mutex
	seq  int
	reqs []Received
}

// NewSink cria o receptor com o relógio informado.
func NewSink(now func() time.Time) *Sink { return &Sink{now: now} }

// Handler devolve as rotas do receptor.
func (s *Sink) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /hooks/{name}", s.receive)
	mux.HandleFunc("GET /requests", s.list)
	return mux
}

func (s *Sink) receive(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		http.Error(w, "corpo ilegível ou acima do limite", http.StatusRequestEntityTooLarge)
		return
	}
	headers := make(map[string]string, len(r.Header))
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	s.mu.Lock()
	s.seq++
	s.reqs = append(s.reqs, Received{Seq: s.seq, Name: r.PathValue("name"), ReceivedAt: s.now().UTC(), Headers: headers,
		BodyBase64: base64.StdEncoding.EncodeToString(body)})
	if len(s.reqs) > maxRequests {
		s.reqs = s.reqs[len(s.reqs)-maxRequests:]
	}
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// list devolve as requisições recebidas; com ?name=, só as desse caminho.
func (s *Sink) list(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	s.mu.Lock()
	out := make([]Received, 0, len(s.reqs))
	for _, req := range s.reqs {
		if name == "" || req.Name == name {
			out = append(out, req)
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		slog.Default().Warn("resposta não enviada", slog.String("error", err.Error()))
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func healthcheck(addr string) error {
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	if err := resp.Body.Close(); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("receptor não saudável")
	}
	return nil
}

func run() error {
	addr := envOr("HOOKSINK_ADDR", ":8099")
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		return healthcheck(addr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: NewSink(time.Now).Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Default().Info("receptor de webhooks ouvindo", slog.String("addr", addr))
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func main() {
	if err := run(); err != nil {
		slog.Default().Error("receptor encerrado com erro", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
