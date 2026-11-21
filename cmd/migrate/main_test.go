package main

import (
	"testing"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/infra/config"
)

func TestDemoStaffFromEnv(t *testing.T) {
	subject := uuid.New()
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }
	const issuer = "http://localhost:8180/realms/recorvia"

	if s, err := demoStaffFromEnv(env(nil), config.Development, issuer); s != nil || err != nil {
		t.Fatalf("sem a variável, nada é incluído: %+v %v", s, err)
	}
	s, err := demoStaffFromEnv(env(map[string]string{"RECORVIA_DEMO_STAFF_SUBJECT": subject.String(),
		"RECORVIA_DEMO_STAFF_EMAIL": "equipe@recorvia.test"}), config.Development, issuer)
	if err != nil || s == nil || s.Subject != subject || s.Issuer != issuer || s.Email != "equipe@recorvia.test" || s.Name == "" {
		t.Fatalf("equipe de demonstração: %+v %v", s, err)
	}
	for name, tc := range map[string]struct {
		vars   map[string]string
		env    config.Environment
		issuer string
	}{
		"produção":         {map[string]string{"RECORVIA_DEMO_STAFF_SUBJECT": subject.String()}, config.Production, issuer},
		"sujeito inválido": {map[string]string{"RECORVIA_DEMO_STAFF_SUBJECT": "equipe"}, config.Development, issuer},
		"sem emissor":      {map[string]string{"RECORVIA_DEMO_STAFF_SUBJECT": subject.String()}, config.Development, ""},
	} {
		if s, err := demoStaffFromEnv(env(tc.vars), tc.env, tc.issuer); err == nil || s != nil {
			t.Errorf("%s: deveria ser recusado: %+v", name, s)
		}
	}
}
