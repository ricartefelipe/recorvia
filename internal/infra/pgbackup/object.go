package pgbackup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// objectName aceita nomes de objetos extras (por exemplo,
// keycloak/realm-recorvia-20261122T101500Z.json): sem caminhos absolutos nem
// "..", sempre sob extra/, longe das pastas do WAL-G.
var objectName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// ValidObjectName informa se o nome é aceito para objetos extras.
func ValidObjectName(name string) bool {
	return objectName.MatchString(name) && !strings.Contains(name, "..")
}

// InputRunner executa o WAL-G com uma entrada padrão.
type InputRunner interface {
	Runner
	RunInput(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
}

// RunInput implementa InputRunner.
func (r ExecRunner) RunInput(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	c := r.command(ctx, args)
	c.Stdin, c.Stdout = stdin, stdout
	if err := c.Run(); err != nil {
		return fmt.Errorf("wal-g %s: %w", strings.Join(args[:min(2, len(args))], " "), err)
	}
	return nil
}

// compressionExts são as extensões que o WAL-G acrescenta ao nome do objeto
// conforme a compactação do envio.
var compressionExts = []string{"", ".lz4", ".zst", ".br", ".lzma"}

// PutObject envia o conteúdo, compactado e cifrado com a chave do backup, para
// extra/<nome> no destino. Conteúdo vazio não é enviado.
func PutObject(ctx context.Context, r InputRunner, name string, data []byte) error {
	if !ValidObjectName(name) {
		return fmt.Errorf("nome de objeto inválido %q (letras, dígitos, ., _, - e /)", name)
	}
	if len(data) == 0 {
		return errors.New("entrada vazia: nada foi enviado")
	}
	return r.RunInput(ctx, bytes.NewReader(data), io.Discard, "st", "put", "--read-stdin", "extra/"+name)
}

// storedName procura, na listagem do diretório (wal-g st ls), o objeto com o
// nome pedido e a extensão de compactação acrescentada pelo WAL-G.
func storedName(listing []byte, base string) (string, bool) {
	names := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(listing))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "obj" {
			names[fields[len(fields)-1]] = true
		}
	}
	for _, ext := range compressionExts {
		if names[base+ext] {
			return base + ext, true
		}
	}
	return "", false
}

// GetObject escreve em w o objeto extra/<nome>, decifrado e descompactado. O
// nome armazenado (com a extensão da compactação) vem da listagem do
// destino, e o WAL-G grava num arquivo novo de um diretório temporário
// (0700), removido em seguida: um erro de leitura ou de decifragem aparece
// como é, em vez de ser escondido por outras tentativas.
func GetObject(ctx context.Context, r Runner, name string, w io.Writer) error {
	if !ValidObjectName(name) {
		return fmt.Errorf("nome de objeto inválido %q (letras, dígitos, ., _, - e /)", name)
	}
	full := path.Join("extra", name)
	dir, base := path.Split(full)
	var listing bytes.Buffer
	if err := r.Run(ctx, &listing, "st", "ls", dir); err != nil {
		return fmt.Errorf("listagem de %s: %w", dir, err)
	}
	stored, ok := storedName(listing.Bytes(), base)
	if !ok {
		return fmt.Errorf("objeto %s não encontrado no destino", full)
	}
	tmp, err := os.MkdirTemp("", "recorvia-pgbackup-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	local := filepath.Join(tmp, "objeto")
	if err := r.Run(ctx, io.Discard, "st", "get", dir+stored, local); err != nil {
		return fmt.Errorf("leitura de %s (chave de cifragem do backup diferente?): %w", dir+stored, err)
	}
	f, err := os.Open(local) //nolint:gosec // arquivo do diretório temporário criado acima
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}
