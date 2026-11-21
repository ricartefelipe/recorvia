package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// wait aguarda o canal com um prazo de segurança (o teste não depende do
// prazo para sincronizar; ele só evita que uma regressão trave a suíte).
func wait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s não ocorreu", what)
		var zero T
		return zero
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// RNF-11: com SIGTERM, a API para de aceitar conexões e a requisição em
// andamento termina com a resposta completa antes de Serve retornar (o
// processo só sai depois dela); o servidor de métricas é encerrado junto.
func TestServeFinishesInFlightRequestAfterSIGTERM(t *testing.T) {
	ctx, stop := SignalContext(t.Context())
	defer stop()
	started, release := make(chan struct{}), make(chan struct{})
	api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "concluída")
	}), ReadHeaderTimeout: 5 * time.Second}
	shutdownBegan := make(chan struct{})
	api.RegisterOnShutdown(func() { close(shutdownBegan) })
	metrics := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 5 * time.Second}
	apiLn, metricsLn := listen(t), listen(t)

	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, slog.New(slog.DiscardHandler), "api", 5*time.Second,
			HTTPServer{Server: api, Listener: apiLn}, HTTPServer{Server: metrics, Listener: metricsLn})
	}()
	type result struct {
		status int
		body   string
		err    error
	}
	answered := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + apiLn.Addr().String() + "/lento")
		if err != nil {
			answered <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		answered <- result{status: resp.StatusCode, body: string(b), err: err}
	}()
	wait(t, started, "início da requisição")

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(t, ctx.Done(), "cancelamento do contexto pelo SIGTERM")
	wait(t, shutdownBegan, "início do encerramento")
	// O encerramento fecha o listener antes de esperar as requisições.
	if c, err := net.Dial("tcp", apiLn.Addr().String()); err == nil {
		_ = c.Close()
		t.Fatal("a API ainda aceita conexões durante o encerramento")
	}
	// Enquanto a API drena, o servidor de métricas segue atendendo: ele é
	// encerrado depois.
	if resp, err := http.Get("http://" + metricsLn.Addr().String() + "/metrics"); err != nil {
		t.Fatalf("métricas indisponíveis durante a drenagem da API: %v", err)
	} else {
		_ = resp.Body.Close()
	}
	select {
	case err := <-served:
		t.Fatalf("Serve retornou com a requisição em andamento: %v", err)
	case res := <-answered:
		t.Fatalf("a requisição terminou antes de ser liberada: %+v", res)
	default:
	}

	close(release)
	res := wait(t, answered, "resposta da requisição em andamento")
	if res.err != nil || res.status != http.StatusOK || res.body != "concluída" {
		t.Fatalf("resposta %+v", res)
	}
	if err := wait(t, served, "retorno de Serve"); err != nil {
		t.Fatalf("encerramento dentro do prazo deve terminar sem erro: %v", err)
	}
	// O servidor de métricas é encerrado depois da API (as métricas seguem
	// disponíveis durante a drenagem).
	if c, err := net.Dial("tcp", metricsLn.Addr().String()); err == nil {
		_ = c.Close()
		t.Fatal("o servidor de métricas deve estar encerrado")
	}
}

// RNF-11: o encerramento é limitado pelo prazo configurado
// (RECORVIA_SHUTDOWN_TIMEOUT): uma requisição que não termina não segura o
// processo além dele.
func TestServeGivesUpAtShutdownTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-release
	}), ReadHeaderTimeout: 5 * time.Second}
	ln := listen(t)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, slog.New(slog.DiscardHandler), "api", 50*time.Millisecond, HTTPServer{Server: srv, Listener: ln})
	}()
	go func() {
		if resp, err := http.Get("http://" + ln.Addr().String() + "/sem-fim"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	wait(t, started, "início da requisição")
	cancel()
	if err := wait(t, served, "retorno de Serve no prazo"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("esperado o fim do prazo de encerramento: %v", err)
	}
}

// A falha de um servidor encerra os demais e é devolvida; endereço inválido
// falha antes de atender.
func TestServeReportsServerFailures(t *testing.T) {
	broken := listen(t)
	_ = broken.Close()
	other := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: 5 * time.Second}
	otherLn := listen(t)
	err := Serve(t.Context(), slog.New(slog.DiscardHandler), "api", time.Second,
		HTTPServer{Server: &http.Server{ReadHeaderTimeout: time.Second}, Listener: broken}, HTTPServer{Server: other, Listener: otherLn})
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("falha do listener deve ser devolvida: %v", err)
	}
	if c, derr := net.Dial("tcp", otherLn.Addr().String()); derr == nil {
		_ = c.Close()
		t.Fatal("o outro servidor deve ser encerrado")
	}
	err = Serve(t.Context(), slog.New(slog.DiscardHandler), "api", time.Second,
		HTTPServer{Server: &http.Server{Addr: "127.0.0.1:0", ReadHeaderTimeout: time.Second}},
		HTTPServer{Server: &http.Server{Addr: "endereço inválido", ReadHeaderTimeout: time.Second}})
	if err == nil {
		t.Fatal("endereço inválido deve falhar")
	}
}

// RNF-11: o primeiro sinal cancela o contexto (encerramento gracioso); um
// segundo sinal durante a drenagem força a saída.
func TestSecondSignalForcesExit(t *testing.T) {
	forced := make(chan struct{})
	ctx, stop := signalContext(t.Context(), func() { close(forced) })
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	wait(t, ctx.Done(), "cancelamento pelo primeiro sinal")
	select {
	case <-forced:
		t.Fatal("o primeiro sinal não força a saída")
	default:
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	wait(t, forced, "saída forçada pelo segundo sinal")
	stop() // idempotente
}

// RNF-11: durante a drenagem do worker, o servidor de saúde continua no ar,
// /livez responde 200 e /readyz 503 (worker=shutting_down); ele fecha depois
// que o worker termina.
func TestRunWorkerKeepsHealthServerUntilDrained(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	draining, release := make(chan struct{}), make(chan struct{})
	run := func(ctx context.Context) error {
		<-ctx.Done()
		close(draining)
		<-release
		return nil
	}
	mux := WorkerHealthMux(http.NotFoundHandler(), WorkerProbes{Database: okDB, Broker: func() bool { return true },
		ShuttingDown: func() bool { return ctx.Err() != nil }})
	ln := listen(t)
	base := "http://" + ln.Addr().String()
	done := make(chan error, 1)
	go func() {
		done <- RunWorker(ctx, slog.New(slog.DiscardHandler), run,
			HTTPServer{Server: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, Listener: ln})
	}()
	// Sem conexões persistentes: uma conexão aberta pelo cliente e nunca
	// usada só seria fechada pelo Shutdown depois de 5 s.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	status := func(path string) int {
		t.Helper()
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := status("/readyz"); got != http.StatusOK {
		t.Fatalf("readyz antes do encerramento: %d", got)
	}
	cancel()
	wait(t, draining, "início da drenagem")
	if got := status("/readyz"); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz durante a drenagem: %d", got)
	}
	if got := status("/livez"); got != http.StatusOK {
		t.Fatalf("livez durante a drenagem: %d", got)
	}
	close(release)
	if err := wait(t, done, "retorno do worker"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		_ = c.Close()
		t.Fatal("o servidor de saúde deve fechar depois da drenagem")
	}
}
