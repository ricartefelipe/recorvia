// Comando carga prepara os dados sintéticos e executa os cenários do teste de
// carga do Recorvia (ADR-088, docs/operacao/carga.md) contra o ambiente
// isolado de deploy/carga. É chamado por deploy/carga/carga.sh (make
// load-test), que sobe o ambiente e passa os endereços e segredos pelo
// ambiente. Não faz parte do produto e não deve apontar para produção.
//
// Subcomandos:
//
//	preparar     organizações, plano da plataforma, catálogo e credenciais de
//	             API, pelos serviços da aplicação (como o seed)
//	popular      clientes, assinaturas retroativas (histórico de faturas e
//	             ledger) e pagamentos manuais, pela API
//	provedor     configura o simulador de pagamentos como provedor padrão
//	leitura      cenário (a): listas paginadas e painel
//	escrita      cenário (b): clientes, assinaturas e pagamentos manuais com
//	             Idempotency-Key
//	faturamento  cenário (c): fechamento em lote com o worker parado e
//	             retomado, cobranças no simulador e webhooks de volta
//	consumo      cenário (d): ingestão de consumo medido em lote
//	webhooks     cenário (e): pico de webhooks do provedor, com repetições e
//	             fora de ordem
//	motor        diagnóstico: o motor de recorrência isolado, neste
//	             processo, com o worker parado (perfil de CPU opcional)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// env reúne os endereços do ambiente de carga, lidos de variáveis
// RECORVIA_LOAD_*.
type env struct {
	API           string // URL base da API (http://127.0.0.1:porta)
	APIMetrics    string // /metrics da API
	WorkerMetrics string // /metrics do worker
	Paysim        string // URL base do simulador
	RabbitAPI     string // URL da API de administração do RabbitMQ
	RabbitUser    string
	RabbitPass    string
	ObserverDB    string // URL do PostgreSQL para observação (somente leitura)
	StateFile     string // estado gerado por preparar
	OutDir        string // diretório dos resultados
	Project       string // projeto do compose (containers medidos)
	Worker        string // container do worker
}

func loadEnv() env {
	get := func(k string) string { return os.Getenv("RECORVIA_LOAD_" + k) }
	return env{API: get("API"), APIMetrics: get("API_METRICS"), WorkerMetrics: get("WORKER_METRICS"), Paysim: get("PAYSIM"),
		RabbitAPI: get("RABBIT_API"), RabbitUser: get("RABBIT_USER"), RabbitPass: get("RABBIT_PASSWORD"), ObserverDB: get("OBSERVER_DB"),
		StateFile: get("STATE"), OutDir: get("OUT"), Project: get("PROJECT"), Worker: get("WORKER_CONTAINER")}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: carga <preparar|popular|provedor|leitura|escrita|faturamento|consumo|webhooks> [opções]")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	e := loadEnv()
	suspended := watchClock(ctx, log)
	// A porta do /metrics do worker muda sempre que ele é religado (pelo
	// faturamento ou pelo motor): a do ambiente pode ser de antes.
	if e.Worker != "" && cmd != "motor" {
		if u, err := workerMetricsURL(ctx, e.Worker); err != nil {
			log.Warn("porta do /metrics do worker não encontrada", slog.String("error", err.Error()))
		} else {
			if u != e.WorkerMetrics {
				log.Info("porta do /metrics do worker mudou desde o início do roteiro", slog.String("anterior", e.WorkerMetrics), slog.String("atual", u))
			}
			e.WorkerMetrics = u
		}
	}
	var err error
	switch cmd {
	case "preparar":
		err = runPrepare(ctx, log, e, args)
	case "popular":
		err = runPopulate(ctx, log, e, args)
	case "provedor":
		err = runProvider(ctx, log, e, args)
	case "motor":
		err = runEngine(ctx, log, e, args)
	case "leitura", "escrita", "faturamento", "consumo", "webhooks":
		err = runScenario(ctx, log, e, cmd, args)
	default:
		err = fmt.Errorf("subcomando desconhecido: %s", cmd)
	}
	// Um cenário medido com a máquina suspensa no meio não serve para
	// comparação: falha em vez de gravar números enganosos.
	if lost := suspended(); err == nil && lost > 0 && cmd != "preparar" && cmd != "popular" && cmd != "provedor" {
		err = fmt.Errorf("a máquina ficou suspensa ou parada por %s durante a medição: refaça o cenário", lost.Round(time.Second))
	}
	if err != nil {
		log.Error("carga falhou", slog.String("subcomando", cmd), slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// state é o resultado de preparar e popular: organizações, credenciais e
// identificadores usados pelos cenários. Fica no diretório temporário do
// roteiro (permissão 600) e some com ele.
type state struct {
	CreatedAt time.Time `json:"created_at"`
	Orgs      []org     `json:"orgs"`
}

type org struct {
	Slug          string   `json:"slug"`
	TenantID      string   `json:"tenant_id"`
	Token         string   `json:"token"`
	Customers     int      `json:"customers"`
	FlatPlans     []string `json:"flat_plans"`
	UsagePlan     string   `json:"usage_plan"`
	MetricCode    string   `json:"metric_code"`
	PaysimKey     string   `json:"paysim_key"`
	WebhookURL    string   `json:"webhook_url,omitempty"`
	WebhookSecret string   `json:"webhook_secret,omitempty"`
	CustomerIDs   []string `json:"customer_ids,omitempty"`
	UsageSubs     []string `json:"usage_subscriptions,omitempty"`
}

func readState(path string) (*state, error) {
	if path == "" {
		return nil, errors.New("RECORVIA_LOAD_STATE não definido")
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("estado %s: %w", path, err)
	}
	if len(s.Orgs) == 0 {
		return nil, errors.New("estado sem organizações: rode preparar")
	}
	return &s, nil
}

func writeState(path string, s *state) error {
	if path == "" {
		return errors.New("RECORVIA_LOAD_STATE não definido")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// intList interpreta listas como "8,16,32".
func intList(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("valor inválido na lista: %q", p)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("lista vazia")
	}
	return out, nil
}

func newFlags(name string) *flag.FlagSet { return flag.NewFlagSet(name, flag.ContinueOnError) }

// outPath devolve o caminho de um arquivo de resultado, criando o diretório.
func outPath(e env, name string) (string, error) {
	dir := e.OutDir
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}
