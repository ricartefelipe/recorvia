package secrets

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func mustKeyring(t *testing.T, keys map[string][]byte, active string) *Keyring {
	t.Helper()
	k, err := NewKeyring(keys, active)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func seal(t *testing.T, k *Keyring, aad string) ([]byte, string) {
	t.Helper()
	ct, kid, err := k.Encrypt([]byte("segredo"), []byte(aad))
	if err != nil {
		t.Fatal(err)
	}
	return ct, kid
}

func TestKeyIDsSorted(t *testing.T) {
	k := mustKeyring(t, map[string][]byte{"k2": key(2), "k1": key(1), "k3": key(3)}, "k2")
	if got := k.KeyIDs(); !slices.Equal(got, []string{"k1", "k2", "k3"}) {
		t.Fatalf("KeyIDs = %v", got)
	}
}

func TestAuditReport(t *testing.T) {
	origin := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k1")
	rotated := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	acct1, kid1 := seal(t, origin, "tenant-a/simulator/api_key")
	acct2, _ := seal(t, origin, "tenant-b/simulator/api_key")
	hook, kid2 := seal(t, rotated, "tenant-a/webhook/1")
	expired, _ := seal(t, origin, "tenant-a/webhook/0")

	type item struct {
		purpose, keyID, aad string
		ct                  []byte
		inUse               bool
	}
	items := []item{
		{"segredo de webhook", kid2, "tenant-a/webhook/1", hook, true},
		{"credencial de provedor", kid1, "tenant-a/simulator/api_key", acct1, true},
		{"credencial de provedor", kid1, "tenant-b/simulator/api_key", acct2, true},
		{"segredo de webhook", kid1, "tenant-a/webhook/0", expired, false},
	}
	cases := []struct {
		name         string
		keyring      *Keyring
		tamper       bool
		wantErr      bool
		wantProblems []string // trechos esperados, na ordem do relatório
		wantAll      int      // problemas incluindo os que não bloqueiam
		// wantCorrupted: registros em uso ilegíveis com chave comprovada, que
		// não bloqueiam.
		wantCorrupted []string
	}{
		{name: "keyring completo", keyring: rotated},
		{name: "chave ausente", keyring: mustKeyring(t, map[string][]byte{"k2": key(2)}, "k2"), wantErr: true,
			wantProblems: []string{`chave "k1" ausente do keyring: 2 registro(s) de credencial de provedor não podem ser decifrados (2 em uso)`},
			wantAll:      2},
		{name: "chave com outro valor", keyring: mustKeyring(t, map[string][]byte{"k1": key(9), "k2": key(2)}, "k2"), wantErr: true,
			wantProblems: []string{`chave "k1" com valor diferente do usado na cifragem: nenhum dos 2 registro(s) de credencial de provedor decifra (2 em uso)`},
			wantAll:      2},
		{name: "um registro corrompido não é problema do keyring", keyring: rotated, tamper: true, wantAll: 1,
			wantCorrupted: []string{`1 de 2 registro(s) de credencial de provedor não decifram com a chave "k1", que decifra outros registros ` +
				`(dado corrompido ou copiado de outro contexto; 1 em uso)`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAudit(tc.keyring)
			for i, it := range items {
				ct := it.ct
				if tc.tamper && i == 1 {
					ct = slices.Clone(ct)
					ct[len(ct)-1] ^= 0xff
				}
				a.Check(it.purpose, it.keyID, ct, []byte(it.aad), it.inUse)
			}
			r := a.Report()
			if r.ActiveKey != tc.keyring.ActiveKeyID() || !slices.Equal(r.ConfiguredKeys, tc.keyring.KeyIDs()) {
				t.Fatalf("relatório descreve outro keyring: %+v", r)
			}
			if len(r.Usages) != 3 || r.Usages[0].Purpose != "credencial de provedor" || r.Usages[1].KeyID != "k1" || r.Usages[2].KeyID != "k2" {
				t.Fatalf("linhas fora da ordem por finalidade e chave: %+v", r.Usages)
			}
			if r.Usages[0].Total != 2 || r.Usages[0].InUse != 2 || r.Usages[1].InUse != 0 {
				t.Fatalf("contagens incorretas: %+v", r.Usages)
			}
			err := r.Err()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Err() = %v, erro esperado: %v", err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, ErrKeyringMismatch) {
				t.Fatalf("Err() = %v, esperado ErrKeyringMismatch", err)
			}
			blocking := r.Problems(true)
			if len(blocking) != len(tc.wantProblems) {
				t.Fatalf("problemas que bloqueiam = %q, esperado %d", blocking, len(tc.wantProblems))
			}
			for i, want := range tc.wantProblems {
				if blocking[i] != want {
					t.Fatalf("problema %d = %q, esperado %q", i, blocking[i], want)
				}
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("erro %q não traz o problema %q", err, want)
				}
			}
			if all := r.Problems(false); len(all) != tc.wantAll {
				t.Fatalf("problemas (com os fora de uso) = %q, esperado %d", all, tc.wantAll)
			}
			if got := r.CorruptedProblems(); !slices.Equal(got, tc.wantCorrupted) {
				t.Fatalf("registros ilegíveis com chave comprovada = %q, esperado %q", got, tc.wantCorrupted)
			}
		})
	}
}

func TestAuditIgnoresUnreadableDataOutOfUse(t *testing.T) {
	origin := mustKeyring(t, map[string][]byte{"k1": key(1)}, "k1")
	ct, kid := seal(t, origin, "tenant-a/webhook/0")
	a := NewAudit(mustKeyring(t, map[string][]byte{"k2": key(2)}, "k2"))
	a.Check("segredo de webhook", kid, ct, []byte("tenant-a/webhook/0"), false)
	r := a.Report()
	if err := r.Err(); err != nil {
		t.Fatalf("dado fora de uso não deveria bloquear: %v", err)
	}
	u := r.Usages[0]
	if !u.Missing || u.Failed != 1 || u.Blocking() || u.Problem() == "" {
		t.Fatalf("uso = %+v, esperado chave ausente registrada sem bloquear", u)
	}
}

func TestAuditOfEmptyDatabase(t *testing.T) {
	r := NewAudit(mustKeyring(t, map[string][]byte{"k1": key(1)}, "k1")).Report()
	if r.Err() != nil || len(r.Usages) != 0 || len(r.Problems(false)) != 0 {
		t.Fatalf("banco sem dados cifrados deveria ser compatível: %+v", r)
	}
}

// O diagnóstico é por chave, não por finalidade e chave: um registro isolado
// que não decifra não vira "chave com outro valor" quando a mesma chave
// decifra registros de outra finalidade, e não impede a inicialização. Sem
// nenhum registro decifrado em nenhuma finalidade, a chave está errada.
func TestAuditProvesKeyAcrossPurposes(t *testing.T) {
	origin := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	acct1, kid := seal(t, origin, "tenant-a/simulator/api_key")
	acct2, _ := seal(t, origin, "tenant-b/simulator/api_key")
	hook, _ := seal(t, origin, "tenant-a/webhook/1")
	tampered := slices.Clone(hook)
	tampered[len(tampered)-1] ^= 0xff

	t.Run("registro único adulterado com a chave comprovada por outra finalidade", func(t *testing.T) {
		a := NewAudit(origin)
		a.Check("credencial de provedor", kid, acct1, []byte("tenant-a/simulator/api_key"), true)
		a.Check("credencial de provedor", kid, acct2, []byte("tenant-b/simulator/api_key"), true)
		a.Check("segredo de webhook", kid, tampered, []byte("tenant-a/webhook/1"), true)
		r := a.Report()
		if err := r.Err(); err != nil {
			t.Fatalf("dado corrompido com a chave comprovada não é engano do keyring: %v", err)
		}
		acct, hookUsage := r.Usages[0], r.Usages[1]
		if acct.Result() != "ok" || !acct.KeyProven {
			t.Fatalf("credenciais: %+v (%s)", acct, acct.Result())
		}
		if hookUsage.Result() != "registros ilegíveis" || hookUsage.Blocking() || !hookUsage.Corrupted() || !hookUsage.KeyProven {
			t.Fatalf("segredo adulterado: %+v (%s)", hookUsage, hookUsage.Result())
		}
		want := `1 de 1 registro(s) de segredo de webhook não decifram com a chave "k2", que decifra outros registros ` +
			`(dado corrompido ou copiado de outro contexto; 1 em uso)`
		if got := r.CorruptedProblems(); !slices.Equal(got, []string{want}) {
			t.Fatalf("problemas = %q, esperado %q", got, want)
		}
		if strings.Contains(hookUsage.Problem(), "valor diferente") {
			t.Fatalf("a mesma chave não pode aparecer como certa e errada: %q", hookUsage.Problem())
		}
	})
	t.Run("nenhum registro da chave decifra em nenhuma finalidade", func(t *testing.T) {
		a := NewAudit(mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(9)}, "k2"))
		a.Check("credencial de provedor", kid, acct1, []byte("tenant-a/simulator/api_key"), true)
		a.Check("segredo de webhook", kid, hook, []byte("tenant-a/webhook/1"), true)
		r := a.Report()
		if !errors.Is(r.Err(), ErrKeyringMismatch) || len(r.Problems(true)) != 2 || len(r.CorruptedProblems()) != 0 {
			t.Fatalf("Err() = %v; problemas = %q", r.Err(), r.Problems(true))
		}
		for _, u := range r.Usages {
			if u.Result() != "chave com outro valor" || u.KeyProven || !u.Blocking() {
				t.Fatalf("uso = %+v (%s)", u, u.Result())
			}
		}
	})
	t.Run("registro ilegível fora de uso", func(t *testing.T) {
		a := NewAudit(origin)
		a.Check("credencial de provedor", kid, acct1, []byte("tenant-a/simulator/api_key"), true)
		a.Check("segredo de webhook", kid, tampered, []byte("tenant-a/webhook/1"), false)
		r := a.Report()
		if u := r.Usages[1]; u.Result() != "registros ilegíveis (fora de uso)" || u.Corrupted() || u.Blocking() {
			t.Fatalf("uso = %+v (%s)", u, u.Result())
		}
		if r.Err() != nil || len(r.CorruptedProblems()) != 0 || len(r.Problems(false)) != 1 {
			t.Fatalf("relatório = %+v", r)
		}
	})
}

// Uma chave declarada perdida não bloqueia: os dados cifrados com ela não
// são recuperáveis e a declaração é explícita, por chave. As demais chaves
// continuam conferidas.
func TestAuditWithLostKeys(t *testing.T) {
	origin := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k1")
	old, kid1 := seal(t, origin, "tenant-a/simulator/api_key")
	rotated := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	current, kid2 := seal(t, rotated, "tenant-b/simulator/api_key")

	onlyNew := mustKeyring(t, map[string][]byte{"k2": key(2)}, "k2")
	withLost, err := onlyNew.WithLostKeys([]string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := withLost.LostKeyIDs(); !slices.Equal(got, []string{"k1"}) || len(onlyNew.LostKeyIDs()) != 0 {
		t.Fatalf("chaves perdidas = %v (o keyring original não pode mudar: %v)", got, onlyNew.LostKeyIDs())
	}
	check := func(k *Keyring) AuditReport {
		a := NewAudit(k)
		a.Check("credencial de provedor", kid1, old, []byte("tenant-a/simulator/api_key"), true)
		a.Check("credencial de provedor", kid2, current, []byte("tenant-b/simulator/api_key"), true)
		return a.Report()
	}

	if err := check(onlyNew).Err(); !errors.Is(err, ErrKeyringMismatch) {
		t.Fatalf("sem a declaração, a chave ausente bloqueia: %v", err)
	}
	r := check(withLost)
	if err := r.Err(); err != nil {
		t.Fatalf("chave declarada perdida não deveria bloquear: %v", err)
	}
	if !slices.Equal(r.LostKeys, []string{"k1"}) {
		t.Fatalf("relatório sem as chaves perdidas: %+v", r)
	}
	u := r.Usages[0]
	if !u.Lost || !u.Missing || u.Blocking() || !u.LostInUse() || u.Result() != "chave declarada perdida" {
		t.Fatalf("uso = %+v (%s)", u, u.Result())
	}
	want := `chave "k1" declarada perdida: 1 registro(s) de credencial de provedor não podem ser decifrados (1 em uso)`
	if got := r.LostProblems(); !slices.Equal(got, []string{want}) {
		t.Fatalf("problemas de chave perdida = %q, esperado %q", got, want)
	}
	if r.Usages[1].Result() != "ok" {
		t.Fatalf("a chave presente continua conferida: %+v", r.Usages[1])
	}

	// A declaração vale só para a chave citada: outra chave ausente bloqueia.
	other, err := mustKeyring(t, map[string][]byte{"k3": key(3)}, "k3").WithLostKeys([]string{"k1"})
	if err != nil {
		t.Fatal(err)
	}
	r = check(other)
	if err := r.Err(); !errors.Is(err, ErrKeyringMismatch) || !strings.Contains(err.Error(), `chave "k2" ausente do keyring`) ||
		strings.Contains(err.Error(), `"k1"`) {
		t.Fatalf("Err() = %v, esperado bloquear só pela chave k2", err)
	}
}

func TestWithLostKeysRejectsConfiguredOrEmptyKey(t *testing.T) {
	k := mustKeyring(t, map[string][]byte{"k1": key(1)}, "k1")
	if _, err := k.WithLostKeys([]string{"k1"}); err == nil || !strings.Contains(err.Error(), `"k1" está configurada`) {
		t.Fatalf("erro = %v, esperado recusar chave configurada", err)
	}
	if _, err := k.WithLostKeys([]string{""}); err == nil {
		t.Fatal("esperava recusar identificador vazio")
	}
	same, err := k.WithLostKeys(nil)
	if err != nil || len(same.LostKeyIDs()) != 0 || same.ActiveKeyID() != "k1" {
		t.Fatalf("sem chaves perdidas: %v %v", same, err)
	}
	// A declaração não muda a cifragem nem a decifragem.
	ct, kid := seal(t, same, "aad")
	if pt, err := k.Decrypt(ct, kid, []byte("aad")); err != nil || string(pt) != "segredo" {
		t.Fatalf("decifragem = %q, %v", pt, err)
	}
}

// A rotação em duas fases mantém compatíveis os processos que convivem
// durante um reinício: na fase 1 todos recebem a chave nova, ainda sem
// ativá-la; na fase 2, quem já ativou a chave nova cifra dados que os demais
// (ainda na fase 1) decifram, e vice-versa. Ativar a chave nova num único
// passo deixa os processos ainda não reiniciados sem como ler o dado novo.
func TestKeyRotationInTwoPhasesKeepsProcessesCompatible(t *testing.T) {
	before := mustKeyring(t, map[string][]byte{"k1": key(1)}, "k1")
	phase1 := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k1")
	phase2 := mustKeyring(t, map[string][]byte{"k1": key(1), "k2": key(2)}, "k2")
	aad := []byte("tenant-a/webhook/1")

	// Fase 1 em andamento: processos antigos e processos com a chave nova
	// distribuída cifram com k1; todos leem.
	for _, writer := range []*Keyring{before, phase1} {
		ct, kid := seal(t, writer, string(aad))
		for _, reader := range []*Keyring{before, phase1} {
			if _, err := reader.Decrypt(ct, kid, aad); err != nil {
				t.Fatalf("fase 1: dado cifrado com %s ilegível: %v", kid, err)
			}
		}
	}
	// Fase 2 em andamento: processos da fase 1 e da fase 2 convivem.
	for _, writer := range []*Keyring{phase1, phase2} {
		ct, kid := seal(t, writer, string(aad))
		for _, reader := range []*Keyring{phase1, phase2} {
			if _, err := reader.Decrypt(ct, kid, aad); err != nil {
				t.Fatalf("fase 2: dado cifrado com %s ilegível: %v", kid, err)
			}
		}
	}
	// Passo único (o que o procedimento evita): o processo ainda não
	// reiniciado não conhece a chave do dado novo.
	ct, kid := seal(t, phase2, string(aad))
	if _, err := before.Decrypt(ct, kid, aad); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("erro = %v, esperado chave desconhecida no processo sem a chave nova", err)
	}
}
