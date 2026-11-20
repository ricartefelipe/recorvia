package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// Healthcheck consulta /livez do próprio processo (usado pelo healthcheck do
// container, que não tem shell nem curl).
func Healthcheck(addr, def string) error {
	if addr == "" {
		addr = def
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/livez", nil)
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
		return errors.New("processo não saudável")
	}
	return nil
}

// WorkerProbes são as verificações de prontidão do worker.
type WorkerProbes struct {
	// Database verifica a conexão com o PostgreSQL.
	Database func(context.Context) error
	// Broker informa se há conexão ativa com o RabbitMQ.
	Broker func() bool
}

// WorkerHealthMux expõe as rotas operacionais do worker na porta de
// métricas. As semânticas são distintas:
//
//   - /livez indica apenas que o processo responde. Não verifica
//     dependências, para que uma indisponibilidade do banco ou do broker não
//     provoque reinícios em cascata (o worker reconecta sozinho).
//   - /readyz responde 200 somente com PostgreSQL e RabbitMQ acessíveis e 503
//     caso contrário, com o estado de cada verificação no corpo.
//   - /metrics publica as métricas Prometheus.
func WorkerHealthMux(metrics http.Handler, p WorkerProbes) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		checks := map[string]string{"postgres": "ok", "rabbitmq": "ok"}
		if p.Database == nil || p.Database(ctx) != nil {
			checks["postgres"] = "unavailable"
		}
		if p.Broker == nil || !p.Broker() {
			checks["rabbitmq"] = "unavailable"
		}
		status, code := "ok", http.StatusOK
		for _, v := range checks {
			if v != "ok" {
				status, code = "unavailable", http.StatusServiceUnavailable
			}
		}
		writeHealth(w, code, map[string]any{"status": status, "checks": checks})
	})
	return mux
}

func writeHealth(w http.ResponseWriter, code int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	// Falha de escrita na resposta não tem tratamento útil: o status já foi enviado.
	_ = json.NewEncoder(w).Encode(body)
}
