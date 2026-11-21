package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// ShutdownSignals iniciam o encerramento gracioso da API e do worker (o
// orquestrador envia SIGTERM; SIGINT vem do terminal).
var ShutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

// SignalContext devolve um contexto cancelado ao receber um dos
// ShutdownSignals. A partir daí o cancelamento chega a todas as etapas que
// usam o contexto: servidores, agendador, relay e consumidores. Um segundo
// sinal durante a drenagem força a saída imediata (código 1), para o
// operador que não quer esperar as mensagens e requisições em andamento.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signalContext(parent, func() {
		fmt.Fprintln(os.Stderr, "segundo sinal de encerramento: saída imediata sem aguardar os processamentos em andamento")
		os.Exit(1)
	})
}

// signalContext é SignalContext com a ação do segundo sinal injetável.
func signalContext(parent context.Context, force func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, ShutdownSignals...)
	done := make(chan struct{})
	go func() {
		select {
		case <-signals:
			cancel()
		case <-done:
			return
		}
		select {
		case <-signals:
			force()
		case <-done:
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			signal.Stop(signals)
			close(done)
			cancel()
		})
	}
}

// HTTPServer é um servidor atendido por Serve.
type HTTPServer struct {
	Server *http.Server
	// Listener é opcional: sem ele, Serve escuta em Server.Addr (TCP).
	Listener net.Listener
}

// Serve atende os servidores até ctx terminar (sinal de encerramento) ou um
// deles falhar. Em seguida encerra todos com http.Server.Shutdown: novas
// conexões deixam de ser aceitas e as requisições em andamento terminam,
// limitadas por shutdownTimeout. Devolve a falha do servidor, se houver, e as
// falhas do encerramento (context.DeadlineExceeded quando o prazo acaba com
// requisições ainda em andamento).
func Serve(ctx context.Context, log *slog.Logger, name string, shutdownTimeout time.Duration, servers ...HTTPServer) error {
	listeners := make([]net.Listener, len(servers))
	for i, s := range servers {
		ln := s.Listener
		if ln == nil {
			var err error
			if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", s.Server.Addr); err != nil {
				for _, open := range listeners[:i] {
					_ = open.Close() // nenhum servidor começou a atender
				}
				return err
			}
		}
		listeners[i] = ln
	}
	errc := make(chan error, len(servers))
	for i, s := range servers {
		go func() { errc <- s.Server.Serve(listeners[i]) }()
	}
	var serveErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case <-ctx.Done():
	}
	log.Info("encerrando " + name + ": aguardando as requisições em andamento")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	errs := []error{serveErr}
	// Em ordem: os servidores seguintes (métricas) continuam atendendo
	// enquanto o primeiro drena as requisições em andamento.
	for i, s := range servers {
		errs = append(errs, s.Server.Shutdown(sctx))
		// Um Serve que ainda não tinha começado quando o encerramento chegou
		// devolve ErrServerClosed sem fechar o listener.
		if err := listeners[i].Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunWorker executa o worker (run) e o servidor de saúde e métricas. No
// encerramento, o servidor continua atendendo enquanto o worker drena (a
// readiness responde 503, ver WorkerProbes.ShuttingDown, e as métricas
// seguem disponíveis) e é encerrado com Serve depois que run retorna.
func RunWorker(ctx context.Context, log *slog.Logger, run func(context.Context) error, health HTTPServer) error {
	serveCtx, stopServing := context.WithCancel(context.WithoutCancel(ctx))
	defer stopServing()
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, log, "servidor de saúde do worker", 5*time.Second, health) }()
	runErr := run(ctx)
	stopServing()
	return errors.Join(runErr, <-served)
}
