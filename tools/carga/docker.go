package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// docker executa um comando do Docker com prazo próprio e devolve a saída
// padrão sem espaços nas pontas.
func docker(ctx context.Context, limit time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).Output() //nolint:gosec // argumentos montados pelo próprio teste (container do projeto de carga)
	if err != nil {
		var detail string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("docker %s: %w %s", strings.Join(args, " "), err, truncate(detail, 300))
	}
	return strings.TrimSpace(string(out)), nil
}

// stopWorker para o worker com a drenagem normal (SIGTERM e prazo do
// compose) e startWorker o religa, esperando a prontidão (healthcheck).
func stopWorker(ctx context.Context, e env) error {
	if e.Worker == "" {
		return fmt.Errorf("RECORVIA_LOAD_WORKER_CONTAINER não definido")
	}
	_, err := docker(ctx, 3*time.Minute, "stop", "-t", "150", e.Worker)
	return err
}

func startWorker(ctx context.Context, e *env) error {
	if _, err := docker(ctx, time.Minute, "start", e.Worker); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		h, err := docker(ctx, 20*time.Second, "inspect", "-f", "{{.State.Health.Status}}", e.Worker)
		if err == nil && h == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("worker não ficou pronto em 2 min (situação %q)", h)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	// A porta publicada no host (escolhida pelo Docker) muda quando o
	// container é religado.
	u, err := workerMetricsURL(ctx, e.Worker)
	if err != nil {
		return err
	}
	e.WorkerMetrics = u
	return nil
}

// workerMetricsURL descobre a porta publicada agora para o /metrics do
// worker. Cada subcomando chama ao começar: faturamento e motor religam o
// worker, e o Docker escolhe outra porta a cada início.
func workerMetricsURL(ctx context.Context, container string) (string, error) {
	out, err := docker(ctx, 20*time.Second, "port", container, "9091/tcp")
	if err != nil {
		return "", err
	}
	return metricsURLFromPort(out)
}

// metricsURLFromPort monta o endereço a partir da saída de "docker port"
// (uma linha por endereço, como 127.0.0.1:49153).
func metricsURLFromPort(out string) (string, error) {
	for _, addr := range strings.Fields(out) {
		i := strings.LastIndexByte(addr, ':')
		if i < 0 || i == len(addr)-1 {
			continue
		}
		if _, err := strconv.Atoi(addr[i+1:]); err != nil {
			continue
		}
		return "http://127.0.0.1:" + addr[i+1:] + "/metrics", nil
	}
	return "", fmt.Errorf("porta do /metrics do worker não publicada: %q", truncate(out, 100))
}
