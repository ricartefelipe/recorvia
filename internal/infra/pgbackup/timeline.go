package pgbackup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Ancestry é o histórico de uma linha do tempo: as linhas do tempo pelas
// quais o servidor passou até chegar a ela e, para cada uma, o LSN em que foi
// deixada (lido do arquivo .history arquivado). A própria linha alvo não tem
// fim.
type Ancestry struct {
	Target uint32
	leave  map[uint32]uint64
}

// NewAncestry monta o histórico da linha alvo a partir dos pares (linha do
// tempo anterior, LSN da troca) do arquivo .history.
func NewAncestry(target uint32, parents map[uint32]uint64) Ancestry {
	leave := map[uint32]uint64{target: math.MaxUint64}
	for tl, lsn := range parents {
		leave[tl] = lsn
	}
	return Ancestry{Target: target, leave: leave}
}

// Contains informa se o backup base serve à recuperação na linha alvo: ele
// está numa linha do histórico e terminou antes do ponto em que essa linha foi
// deixada. Um backup de uma linha abandonada, feito depois da ramificação,
// não está no histórico (a recuperação terminaria com "requested timeline is
// not a child of this server's history").
func (a Ancestry) Contains(b Backup) bool {
	end, ok := a.leave[b.Timeline()]
	if !ok {
		return false
	}
	if b.Timeline() == a.Target {
		return true
	}
	return b.FinishLSN > 0 && b.FinishLSN <= end
}

// ParseLSN lê um LSN no formato do PostgreSQL (X/Y, hexadecimal).
func ParseLSN(s string) (uint64, error) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok {
		return 0, fmt.Errorf("LSN inválido %q", s)
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, fmt.Errorf("LSN inválido %q", s)
	}
	return h<<32 | l, nil
}

// ParseHistory lê o conteúdo de um arquivo .history (linhas "linha do tempo
// anterior, LSN da troca, motivo", com comentários iniciados por #).
func ParseHistory(target uint32, content []byte) (Ancestry, error) {
	parents := map[uint32]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return Ancestry{}, fmt.Errorf("linha inválida no histórico da linha do tempo %d: %q", target, line)
		}
		tl, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil || uint32(tl) >= target {
			return Ancestry{}, fmt.Errorf("linha inválida no histórico da linha do tempo %d: %q", target, line)
		}
		lsn, err := ParseLSN(fields[1])
		if err != nil {
			return Ancestry{}, err
		}
		parents[uint32(tl)] = lsn
	}
	if err := sc.Err(); err != nil {
		return Ancestry{}, err
	}
	return NewAncestry(target, parents), nil
}

// historyName é o nome do arquivo de histórico de uma linha do tempo.
func historyName(tl uint32) string { return fmt.Sprintf("%08X.history", tl) }

// fetchFile busca um arquivo do WAL arquivado (wal-fetch) para a memória;
// found é falso quando o arquivo não existe no armazenamento.
func fetchFile(ctx context.Context, r Runner, name string) (data []byte, found bool, err error) {
	dir, err := os.MkdirTemp("", "recorvia-pgbackup-")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "arquivo")
	switch err := r.Run(ctx, io.Discard, "wal-fetch", name, path); FetchExitCode(err) {
	case FetchOK:
	case FetchNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("leitura de %s no armazenamento (chave de cifragem do backup diferente?): %w", name, err)
	}
	data, err = os.ReadFile(path) //nolint:gosec // arquivo do diretório temporário criado acima
	return data, true, err
}

// LatestTimeline devolve a linha do tempo mais recente do WAL arquivado. O
// PostgreSQL numera as linhas novas em sequência, a partir da mais recente
// com histórico no arquivo; a 1 não tem histórico.
func LatestTimeline(ctx context.Context, r Runner) (uint32, error) {
	latest := uint32(1)
	for tl := uint32(2); tl < math.MaxUint32; tl++ {
		_, found, err := fetchFile(ctx, r, historyName(tl))
		if err != nil {
			return 0, err
		}
		if !found {
			return latest, nil
		}
		latest = tl
	}
	return latest, nil
}

// ReadAncestry lê do armazenamento o histórico da linha do tempo.
func ReadAncestry(ctx context.Context, r Runner, tl uint32) (Ancestry, error) {
	if tl <= 1 {
		return NewAncestry(1, nil), nil
	}
	data, found, err := fetchFile(ctx, r, historyName(tl))
	if err != nil {
		return Ancestry{}, err
	}
	if !found {
		return Ancestry{}, fmt.Errorf("linha do tempo %d sem o arquivo %s no armazenamento", tl, historyName(tl))
	}
	return ParseHistory(tl, data)
}
