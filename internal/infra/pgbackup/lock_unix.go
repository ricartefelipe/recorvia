//go:build unix

package pgbackup

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// FileLock obtém, sem esperar, o bloqueio exclusivo do arquivo (criado se
// não existe). O bloqueio é liberado pela função devolvida ou pelo fim do
// processo.
func FileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // caminho fixo do agente
	if err != nil {
		return nil, fmt.Errorf("arquivo de bloqueio %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("outro backup base está em andamento neste container")
		}
		return nil, fmt.Errorf("bloqueio de %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
