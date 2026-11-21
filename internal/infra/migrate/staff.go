package migrate

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DemoStaff descreve o integrante da equipe da plataforma do ambiente de
// demonstração: o usuário do provedor de identidade (emissor e sujeito) que
// passa a integrar platform_staff.
type DemoStaff struct {
	Issuer  string
	Subject uuid.UUID
	Email   string
	Name    string
}

func (s DemoStaff) validate() error {
	var errs []error
	if s.Issuer == "" {
		errs = append(errs, errors.New("emissor OIDC obrigatório"))
	}
	if s.Subject == uuid.Nil {
		errs = append(errs, errors.New("sujeito obrigatório"))
	}
	if a, err := mail.ParseAddress(s.Email); err != nil || a.Address != s.Email {
		errs = append(errs, fmt.Errorf("e-mail inválido %q", s.Email))
	}
	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, errors.New("nome obrigatório"))
	}
	return errors.Join(errs...)
}

// EnsureDemoStaff inclui o usuário de demonstração na equipe da plataforma,
// como dono das tabelas (o papel da aplicação só lê platform_staff). Cria o
// usuário com o ID igual ao sujeito, como o seed, se ele ainda não entrou;
// é idempotente. Uso exclusivo do ambiente local: o comando recusa
// produção.
func EnsureDemoStaff(ctx context.Context, ownerURL string, s DemoStaff, now time.Time) (err error) {
	if err := s.validate(); err != nil {
		return fmt.Errorf("migrate: equipe de demonstração: %w", err)
	}
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close(ctx)) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // sem efeito depois do commit
	if _, err := tx.Exec(ctx, `INSERT INTO users (id, oidc_issuer, oidc_subject, email, display_name, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'active', $6, $6) ON CONFLICT (oidc_issuer, oidc_subject) DO NOTHING`,
		s.Subject, s.Issuer, s.Subject.String(), s.Email, s.Name, now); err != nil {
		return fmt.Errorf("migrate: usuário da equipe: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_staff (user_id, role)
		SELECT id, 'operations' FROM users WHERE oidc_issuer = $1 AND oidc_subject = $2
		ON CONFLICT (user_id) DO NOTHING`, s.Issuer, s.Subject.String()); err != nil {
		return fmt.Errorf("migrate: equipe da plataforma: %w", err)
	}
	return tx.Commit(ctx)
}
