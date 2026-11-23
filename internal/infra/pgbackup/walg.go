package pgbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner executa o WAL-G com os argumentos informados e grava a saída padrão
// em stdout. A saída de erro vai para o log do processo.
type Runner interface {
	Run(ctx context.Context, stdout io.Writer, args ...string) error
}

// ExecRunner executa o binário do WAL-G com o ambiente traduzido da
// configuração.
type ExecRunner struct {
	Bin    string
	Env    []string
	Stderr io.Writer
}

// Run implementa Runner.
func (r ExecRunner) Run(ctx context.Context, stdout io.Writer, args ...string) error {
	cmd := r.command(ctx, args)
	cmd.Stdout = stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wal-g %s: %w", args[0], err)
	}
	return nil
}

func (r ExecRunner) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Bin, args...) //nolint:gosec // executável fixo da imagem; argumentos montados pelo agente
	cmd.Env = r.Env
	cmd.Stderr = r.Stderr
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// Backup é um backup base no armazenamento (wal-g backup-list --json --detail).
type Backup struct {
	Name           string          `json:"backup_name"`
	WALFile        string          `json:"wal_file_name"`
	StartTime      time.Time       `json:"start_time"`
	FinishTime     time.Time       `json:"finish_time"`
	Hostname       string          `json:"hostname"`
	PGVersion      int             `json:"pg_version"`
	CompressedSize int64           `json:"compressed_size"`
	Permanent      bool            `json:"is_permanent"`
	StartLSN       uint64          `json:"start_lsn"`
	FinishLSN      uint64          `json:"finish_lsn"`
	UserData       json.RawMessage `json:"user_data,omitempty"`
}

// UserData são os dados que o agente grava em cada backup base.
type UserData struct {
	// KeyFingerprint identifica a chave de cifragem usada (Fingerprint).
	KeyFingerprint string `json:"recorvia_key_fingerprint"`
}

// Fingerprint devolve a impressão digital da chave gravada no backup (vazia
// se o backup não foi feito pelo agente).
func (b Backup) Fingerprint() string {
	var u UserData
	if len(b.UserData) == 0 || json.Unmarshal(b.UserData, &u) != nil {
		return ""
	}
	return u.KeyFingerprint
}

// Timeline é a linha do tempo do servidor em que o backup começou, lida do
// nome (base_TTTTTTTT...); zero se o nome não segue o padrão.
func (b Backup) Timeline() uint32 {
	rest, ok := strings.CutPrefix(b.Name, "base_")
	if !ok || len(rest) < 8 {
		return 0
	}
	tl, err := strconv.ParseUint(rest[:8], 16, 32)
	if err != nil {
		return 0
	}
	return uint32(tl)
}

// ListBackups lista os backups base do armazenamento, do mais antigo para o
// mais recente (pelo fim). Sem backups, a lista é vazia.
func ListBackups(ctx context.Context, r Runner) ([]Backup, error) {
	var out bytes.Buffer
	if err := r.Run(ctx, &out, "backup-list", "--json", "--detail"); err != nil {
		return nil, err
	}
	return parseBackupList(out.Bytes())
}

func parseBackupList(b []byte) ([]Backup, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	var list []Backup
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("listagem de backups ilegível: %w", err)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].FinishTime.Before(list[j].FinishTime) })
	return list, nil
}

// Latest devolve o backup mais recente (nil se a lista é vazia).
func Latest(list []Backup) *Backup {
	if len(list) == 0 {
		return nil
	}
	return &list[len(list)-1]
}

// ErrNoBackupForTarget indica que nenhum backup base do histórico da linha do
// tempo alvo terminou antes do instante pedido.
var ErrNoBackupForTarget = errors.New("nenhum backup base terminado antes do instante pedido")

// ForTarget escolhe o backup base para recuperar até o instante na linha do
// tempo alvo: o mais recente que terminou antes dele (a recuperação só
// alcança um instante posterior ao fim do backup) entre os do histórico da
// linha (Ancestry.Contains). Com target zero, o mais recente do histórico.
func ForTarget(list []Backup, target time.Time, history Ancestry) (Backup, error) {
	var chosen *Backup
	for i := range list {
		if !history.Contains(list[i]) {
			continue
		}
		if target.IsZero() || list[i].FinishTime.Before(target) {
			chosen = &list[i]
		}
	}
	if chosen == nil {
		return Backup{}, ErrNoBackupForTarget
	}
	return *chosen, nil
}

// PushBackup faz um backup base do diretório de dados com a impressão digital
// da chave nos dados do backup.
func PushBackup(ctx context.Context, r Runner, pgdata, fingerprint string) error {
	data, err := json.Marshal(UserData{KeyFingerprint: fingerprint})
	if err != nil {
		return err
	}
	return r.Run(ctx, io.Discard, "backup-push", pgdata, "--add-user-data", string(data))
}

// ApplyRetention remove os backups base e o WAL fora da retenção: ficam os
// retainFull mais recentes e, com retainDays maior que zero, também todos os
// terminados nos últimos retainDays dias. O WAL anterior ao backup mais
// antigo mantido é removido.
func ApplyRetention(ctx context.Context, r Runner, now time.Time, retainFull, retainDays int) error {
	args := []string{"delete", "retain", "FULL", strconv.Itoa(retainFull)}
	if retainDays > 0 {
		args = append(args, "--after", now.AddDate(0, 0, -retainDays).UTC().Format(time.RFC3339))
	}
	return r.Run(ctx, io.Discard, append(args, "--confirm")...)
}

// FetchBackup extrai o backup base no diretório de dados.
func FetchBackup(ctx context.Context, r Runner, pgdata, name string) error {
	return r.Run(ctx, io.Discard, "backup-fetch", pgdata, name)
}
