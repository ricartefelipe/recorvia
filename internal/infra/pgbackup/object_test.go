package pgbackup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

const stListing = `type size last modified                     name
dir  0     0001-01-01 00:00:00 +0000 UTC     antigos/
obj  24281 2026-11-23 02:41:42.588 +0000 UTC realm-recorvia-20261123T024310Z.json.lz4
obj  10    2026-11-23 02:41:42.588 +0000 UTC outro.json.zst
`

func TestStoredName(t *testing.T) {
	cases := map[string]string{
		"realm-recorvia-20261123T024310Z.json": "realm-recorvia-20261123T024310Z.json.lz4",
		"outro.json":                           "outro.json.zst",
		"realm-recorvia":                       "",
		"antigos":                              "",
	}
	for base, want := range cases {
		got, ok := storedName([]byte(stListing), base)
		if got != want || ok != (want != "") {
			t.Errorf("storedName(%q) = %q, %v; esperado %q", base, got, ok, want)
		}
	}
}

func TestGetObjectUsesStoredNameAndShowsRealError(t *testing.T) {
	var dests []string
	r := &fakeRunner{onCall: func(args []string) {
		if args[0] == "st" && args[1] == "get" {
			dests = append(dests, args[3])
			if _, err := os.Stat(args[3]); err == nil {
				t.Errorf("destino %s já existe (o WAL-G grava só em arquivo novo)", args[3])
			}
			_ = os.WriteFile(args[3], []byte(`{"realm":"recorvia"}`), 0o600)
		}
	}}
	r.fail = map[string]error{}
	lsOut := func(args []string) bool { return args[0] == "st" && args[1] == "ls" }
	runner := &listingRunner{fakeRunner: r, listing: stListing, isList: lsOut}
	var out bytes.Buffer
	if err := GetObject(context.Background(), runner, "keycloak/realm-recorvia-20261123T024310Z.json", &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != `{"realm":"recorvia"}` {
		t.Errorf("conteúdo %q", out.String())
	}
	want := []string{"st ls extra/keycloak/", "st get extra/keycloak/realm-recorvia-20261123T024310Z.json.lz4 " + dests[0]}
	if got := r.commands(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("chamadas %q; esperado %q", got, want)
	}
	if _, err := os.Stat(dests[0]); !errors.Is(err, os.ErrNotExist) {
		t.Error("o arquivo temporário decifrado deveria ter sido removido")
	}

	// Leitura falha (chave errada): o erro do WAL-G aparece, numa única tentativa.
	r.calls, r.onCall = nil, nil
	r.fail["st"] = errors.New("corrupted chunk")
	err := GetObject(context.Background(), runner, "keycloak/realm-recorvia-20261123T024310Z.json", &out)
	if err == nil || !strings.Contains(err.Error(), "corrupted chunk") || len(r.calls) != 2 {
		t.Fatalf("erro de leitura: %v, chamadas %q", err, r.commands())
	}

	// Objeto inexistente: mensagem própria, sem tentativa de leitura.
	r.calls = nil
	delete(r.fail, "st")
	if err := GetObject(context.Background(), runner, "keycloak/nao-existe.json", &out); err == nil ||
		!strings.Contains(err.Error(), "não encontrado") || len(r.calls) != 1 {
		t.Fatalf("objeto inexistente: %v, %q", err, r.commands())
	}
	if err := GetObject(context.Background(), runner, "../wal_005/x", &out); err == nil {
		t.Error("nome fora de extra/ aceito")
	}
}

func TestPutObject(t *testing.T) {
	r := &fakeRunner{}
	if err := PutObject(context.Background(), r, "keycloak/realm.json", nil); err == nil || len(r.calls) != 0 {
		t.Errorf("entrada vazia enviada: %v", err)
	}
	if err := PutObject(context.Background(), r, "keycloak/realm.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if got := r.commands(); len(got) != 1 || got[0] != "st put --read-stdin extra/keycloak/realm.json" || string(r.stdin) != "{}" {
		t.Errorf("envio: %q, entrada %q", got, r.stdin)
	}
}

// listingRunner responde à listagem do destino (st ls); as falhas
// configuradas no fakeRunner valem só para as demais chamadas (st get).
type listingRunner struct {
	*fakeRunner
	listing string
	isList  func([]string) bool
}

func (l *listingRunner) Run(ctx context.Context, stdout io.Writer, args ...string) error {
	if l.isList(args) {
		l.calls = append(l.calls, args)
		_, _ = stdout.Write([]byte(l.listing))
		return nil
	}
	return l.fakeRunner.Run(ctx, stdout, args...)
}
