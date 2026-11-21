package secrets

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrKeyringMismatch indica que o keyring configurado não decifra dados em
// uso já gravados por engano de configuração: chave ausente (e não declarada
// perdida) ou chave com outro valor.
var ErrKeyringMismatch = errors.New("secrets: keyring incompatível com os dados cifrados")

// KeyIDs retorna os identificadores das chaves configuradas, em ordem.
func (k *Keyring) KeyIDs() []string {
	ids := make([]string, 0, len(k.aeads))
	for id := range k.aeads {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// LostKeyIDs retorna os identificadores das chaves declaradas perdidas, em
// ordem.
func (k *Keyring) LostKeyIDs() []string {
	ids := make([]string, 0, len(k.lost))
	for id := range k.lost {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// KeyUsage resume os ciphertexts de uma finalidade cifrados com uma chave.
type KeyUsage struct {
	// Purpose descreve a finalidade (por exemplo, "credencial de provedor").
	Purpose string
	// KeyID é o identificador de chave registrado nos ciphertexts.
	KeyID string
	// Total e InUse contam os ciphertexts conferidos e, deles, os que a
	// aplicação ainda precisa ler.
	Total, InUse int
	// Missing indica chave ausente do keyring.
	Missing bool
	// Lost indica chave ausente e declarada perdida: os dados não são
	// recuperáveis e não apontam engano de configuração.
	Lost bool
	// KeyProven indica que a chave decifrou ao menos um ciphertext, de
	// qualquer finalidade: o valor configurado é o usado na cifragem, e o
	// que não decifra com ela é dado corrompido ou copiado de outro contexto.
	KeyProven bool
	// Failed e FailedInUse contam os ciphertexts que não decifram (chave
	// ausente, chave com outro valor, AAD divergente ou dado corrompido).
	Failed, FailedInUse int
}

// Blocking informa se há dado em uso ilegível por engano do keyring: chave
// ausente que não foi declarada perdida ou chave com outro valor (nenhum
// ciphertext dela decifra, em nenhuma finalidade).
func (u KeyUsage) Blocking() bool { return u.FailedInUse > 0 && !u.Lost && !u.KeyProven }

// Corrupted informa se há dado em uso que não decifra com uma chave
// comprovada por outros registros: o problema está no dado, não no keyring.
func (u KeyUsage) Corrupted() bool { return u.FailedInUse > 0 && u.KeyProven }

// LostInUse informa se há dado em uso cifrado com chave declarada perdida.
func (u KeyUsage) LostInUse() bool { return u.FailedInUse > 0 && u.Lost }

// Result classifica a linha em poucas palavras, para o relatório: "ok",
// "chave ausente", "chave declarada perdida", "chave com outro valor" ou
// "registros ilegíveis", com "(fora de uso)" quando nada do que falha ainda
// é lido pela aplicação.
func (u KeyUsage) Result() string {
	var result string
	switch {
	case u.Failed == 0:
		return "ok"
	case u.Lost:
		result = "chave declarada perdida"
	case u.Missing:
		result = "chave ausente"
	case !u.KeyProven:
		result = "chave com outro valor"
	default:
		result = "registros ilegíveis"
	}
	if u.FailedInUse == 0 {
		result += " (fora de uso)"
	}
	return result
}

// Problem descreve o que impede a leitura, ou devolve vazio quando todos os
// ciphertexts decifram.
func (u KeyUsage) Problem() string {
	switch {
	case u.Failed == 0:
		return ""
	case u.Lost:
		return fmt.Sprintf("chave %q declarada perdida: %d registro(s) de %s não podem ser decifrados (%d em uso)",
			u.KeyID, u.Failed, u.Purpose, u.FailedInUse)
	case u.Missing:
		return fmt.Sprintf("chave %q ausente do keyring: %d registro(s) de %s não podem ser decifrados (%d em uso)",
			u.KeyID, u.Failed, u.Purpose, u.FailedInUse)
	case !u.KeyProven:
		return fmt.Sprintf("chave %q com valor diferente do usado na cifragem: nenhum dos %d registro(s) de %s decifra (%d em uso)",
			u.KeyID, u.Total, u.Purpose, u.FailedInUse)
	default:
		return fmt.Sprintf("%d de %d registro(s) de %s não decifram com a chave %q, que decifra outros registros "+
			"(dado corrompido ou copiado de outro contexto; %d em uso)", u.Failed, u.Total, u.Purpose, u.KeyID, u.FailedInUse)
	}
}

// AuditReport é o resultado da conferência do keyring contra os dados
// cifrados já gravados.
type AuditReport struct {
	// ConfiguredKeys e ActiveKey descrevem o keyring conferido.
	ConfiguredKeys []string
	ActiveKey      string
	// LostKeys traz as chaves declaradas perdidas.
	LostKeys []string
	// Usages traz uma linha por finalidade e chave, em ordem estável.
	Usages []KeyUsage
}

func (r AuditReport) problems(keep func(KeyUsage) bool) []string {
	var out []string
	for _, u := range r.Usages {
		if p := u.Problem(); p != "" && keep(u) {
			out = append(out, p)
		}
	}
	return out
}

// Problems lista as descrições dos ciphertexts que não decifram; com
// blockingOnly, somente as que impedem a inicialização (engano do keyring
// sobre dados em uso).
func (r AuditReport) Problems(blockingOnly bool) []string {
	return r.problems(func(u KeyUsage) bool { return !blockingOnly || u.Blocking() })
}

// CorruptedProblems lista os dados em uso que não decifram com uma chave
// comprovada (dado corrompido): não bloqueiam, mas pedem investigação.
func (r AuditReport) CorruptedProblems() []string { return r.problems(KeyUsage.Corrupted) }

// LostProblems lista os dados em uso cifrados com chaves declaradas
// perdidas: não bloqueiam e ficam ilegíveis até serem substituídos.
func (r AuditReport) LostProblems() []string { return r.problems(KeyUsage.LostInUse) }

// Err devolve ErrKeyringMismatch, com a descrição de cada problema, quando
// algum dado em uso não decifra por engano do keyring; nil caso contrário.
// Não bloqueiam: dados fora de uso (segredos expirados, contas desativadas),
// dados de chaves declaradas perdidas e registros ilegíveis de uma chave
// comprovada por outros registros.
func (r AuditReport) Err() error {
	problems := r.Problems(true)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrKeyringMismatch, strings.Join(problems, "; "))
}

// Audit acumula a conferência de ciphertexts contra um keyring. Não guarda
// nem devolve o conteúdo decifrado.
type Audit struct {
	keyring *Keyring
	usages  map[[2]string]*KeyUsage
	// proven guarda as chaves que decifraram ao menos um ciphertext.
	proven map[string]bool
}

// NewAudit inicia uma conferência com o keyring informado.
func NewAudit(k *Keyring) *Audit {
	return &Audit{keyring: k, usages: map[[2]string]*KeyUsage{}, proven: map[string]bool{}}
}

// Check tenta decifrar o ciphertext e registra o resultado na linha da
// finalidade e da chave. inUse indica dado que a aplicação ainda precisa ler.
func (a *Audit) Check(purpose, keyID string, ciphertext, aad []byte, inUse bool) {
	u := a.usages[[2]string{purpose, keyID}]
	if u == nil {
		u = &KeyUsage{Purpose: purpose, KeyID: keyID}
		a.usages[[2]string{purpose, keyID}] = u
	}
	u.Total++
	if inUse {
		u.InUse++
	}
	_, err := a.keyring.Decrypt(ciphertext, keyID, aad)
	if err == nil {
		a.proven[keyID] = true
		return
	}
	u.Failed++
	if inUse {
		u.FailedInUse++
	}
	if errors.Is(err, ErrUnknownKey) {
		u.Missing = true
		u.Lost = a.keyring.lost[keyID]
	}
}

// Report devolve o resultado acumulado, ordenado por finalidade e chave. A
// comprovação de cada chave considera todas as finalidades: basta um
// ciphertext decifrado para o valor da chave estar certo.
func (a *Audit) Report() AuditReport {
	r := AuditReport{ConfiguredKeys: a.keyring.KeyIDs(), ActiveKey: a.keyring.ActiveKeyID(), LostKeys: a.keyring.LostKeyIDs(),
		Usages: make([]KeyUsage, 0, len(a.usages))}
	for _, u := range a.usages {
		row := *u
		row.KeyProven = a.proven[u.KeyID]
		r.Usages = append(r.Usages, row)
	}
	slices.SortFunc(r.Usages, func(x, y KeyUsage) int {
		if c := strings.Compare(x.Purpose, y.Purpose); c != 0 {
			return c
		}
		return strings.Compare(x.KeyID, y.KeyID)
	})
	return r
}
