// Comando migrate: bootstrap de papéis e aplicação das migrations.
//
//	migrate bootstrap   cria papéis e banco (exige RECORVIA_BOOTSTRAP_DATABASE_URL de superusuário)
//	migrate up          aplica migrations como recorvia_owner
//	migrate status      lista migrations e estado
//
// No ambiente local, com RECORVIA_DEMO_STAFF_SUBJECT definido, o up também
// inclui o usuário de demonstração da equipe da plataforma em
// platform_staff (como dono das tabelas; recusado em produção).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

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
		if err != nil {
			return err
		}
		staff, err := demoStaffFromEnv(os.Getenv, cfg.Env, cfg.OIDCIssuer)
		if err != nil || staff == nil {
			return err
		}
		if err := migrate.EnsureDemoStaff(ctx, cfg.MigrationsDBURL, *staff, time.Now().UTC()); err != nil {
			return err
		}
		log.Info("equipe da plataforma de demonstração incluída", slog.String("email", staff.Email))
		return nil
	}
	return fmt.Errorf("comando desconhecido %q", args[0])
}

// demoStaffFromEnv lê o usuário de demonstração da equipe da plataforma.
// Sem RECORVIA_DEMO_STAFF_SUBJECT, não há inclusão; em produção, a variável
// é recusada (a equipe real é mantida pelo runbook, com registro).
func demoStaffFromEnv(getenv func(string) string, env config.Environment, issuer string) (*migrate.DemoStaff, error) {
	raw := getenv("RECORVIA_DEMO_STAFF_SUBJECT")
	if raw == "" {
		return nil, nil
	}
	if env == config.Production {
		return nil, fmt.Errorf("RECORVIA_DEMO_STAFF_SUBJECT não é aceito em produção")
	}
	subject, err := uuid.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("RECORVIA_DEMO_STAFF_SUBJECT: %w", err)
	}
	if issuer == "" {
		return nil, fmt.Errorf("RECORVIA_OIDC_ISSUER é obrigatório com RECORVIA_DEMO_STAFF_SUBJECT")
	}
	name := getenv("RECORVIA_DEMO_STAFF_NAME")
	if name == "" {
		name = "Equipe Recorvia (demonstração)"
	}
	return &migrate.DemoStaff{Issuer: issuer, Subject: subject, Email: getenv("RECORVIA_DEMO_STAFF_EMAIL"), Name: name}, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
