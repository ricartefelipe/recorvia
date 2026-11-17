// Comando migrate: bootstrap de papéis e aplicação das migrations.
//
//	migrate bootstrap   cria papéis e banco (exige RECORVIA_BOOTSTRAP_DATABASE_URL de superusuário)
//	migrate up          aplica migrations como recorvia_owner
//	migrate status      lista migrations e estado
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
	"github.com/ricartefelipe/recorvia/internal/infra/logging"
	"github.com/ricartefelipe/recorvia/internal/infra/migrate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := logging.New(os.Stdout, os.Getenv("RECORVIA_LOG_LEVEL"), "migrate")
	if err := run(ctx, os.Args[1:], log); err != nil {
		log.Error("falha na migração", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log *slog.Logger) error {
	if len(args) != 1 {
		return fmt.Errorf("uso: migrate bootstrap|up|status")
	}
	switch args[0] {
	case "bootstrap":
		super := os.Getenv("RECORVIA_BOOTSTRAP_DATABASE_URL")
		if super == "" {
			return fmt.Errorf("RECORVIA_BOOTSTRAP_DATABASE_URL é obrigatório")
		}
		opts := migrate.BootstrapOptions{
			Database:      envOr("RECORVIA_DATABASE_NAME", "recorvia"),
			OwnerPassword: os.Getenv("RECORVIA_OWNER_PASSWORD"),
			AppPassword:   os.Getenv("RECORVIA_APP_PASSWORD"),
		}
		if err := migrate.Bootstrap(ctx, super, opts); err != nil {
			return err
		}
		log.Info("bootstrap concluído", slog.String("database", opts.Database))
		return nil
	case "up", "status":
		cfg, err := config.Load(os.Getenv)
		if err != nil {
			return err
		}
		if err := cfg.ValidateMigrations(); err != nil {
			return err
		}
		if args[0] == "status" {
			lines, err := migrate.Status(ctx, cfg.MigrationsDBURL)
			for _, l := range lines {
				fmt.Println(l)
			}
			return err
		}
		applied, err := migrate.Up(ctx, cfg.MigrationsDBURL)
		log.Info("migrations aplicadas", slog.Int("count", len(applied)), slog.Any("files", applied))
		return err
	}
	return fmt.Errorf("comando desconhecido %q", args[0])
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
