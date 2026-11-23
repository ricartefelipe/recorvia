// Package pgbackup implementa o agente de backup contínuo do PostgreSQL
// (ADR-087): arquivamento de WAL e backups base cifrados no cliente para
// armazenamento compatível com S3, com o WAL-G, retenção, restauração para um
// instante e métricas do arquivamento e dos backups.
//
// A configuração vem de variáveis RECORVIA_BACKUP_* (ou de arquivos, com o
// sufixo _FILE) e é traduzida aqui para as variáveis do WAL-G, de modo que o
// archive_command, o restore_command, o agente e os comandos avulsos usem a
// mesma tradução e as mesmas recusas: sem destino ou sem chave de cifragem,
// nada é enviado nem restaurado.
package pgbackup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Variáveis de configuração.
const (
	EnvS3Prefix          = "RECORVIA_BACKUP_S3_PREFIX"
	EnvS3Endpoint        = "RECORVIA_BACKUP_S3_ENDPOINT"
	EnvS3Region          = "RECORVIA_BACKUP_S3_REGION"
	EnvS3ForcePathStyle  = "RECORVIA_BACKUP_S3_FORCE_PATH_STYLE"
	EnvS3AccessKeyID     = "RECORVIA_BACKUP_S3_ACCESS_KEY_ID"
	EnvS3SecretAccessKey = "RECORVIA_BACKUP_S3_SECRET_ACCESS_KEY" //nolint:gosec // nome da variável, não é credencial
	EnvEncryptionKey     = "RECORVIA_BACKUP_ENCRYPTION_KEY"
	EnvBaseInterval      = "RECORVIA_BACKUP_BASE_INTERVAL"
	EnvRetryInterval     = "RECORVIA_BACKUP_RETRY_INTERVAL"
	EnvRetainFull        = "RECORVIA_BACKUP_RETAIN_FULL"
	EnvRetainDays        = "RECORVIA_BACKUP_RETAIN_DAYS"
	EnvMetricsAddr       = "RECORVIA_BACKUP_METRICS_ADDR"
	EnvPGPassword        = "RECORVIA_BACKUP_PG_PASSWORD" //nolint:gosec // nome da variável, não é credencial
	// fileSuffix indica que o valor está no arquivo cujo caminho é o valor
	// da variável (segredos montados pelo orquestrador).
	fileSuffix = "_FILE"
)

// KeySize é o tamanho da chave de cifragem (libsodium, secretstream).
const KeySize = 32

// Config é a configuração do backup contínuo.
type Config struct {
	S3Prefix        string
	S3Endpoint      string
	S3Region        string
	S3ForcePath     bool
	AccessKeyID     string
	SecretAccessKey string

	// key é a chave de cifragem decodificada; keyFile, o arquivo de onde ela
	// foi lida (repassado ao WAL-G, para que o valor não vá para o ambiente
	// dos processos filhos).
	key     []byte
	keyB64  string
	keyFile string

	PGData        string
	PGPassword    string
	BaseInterval  time.Duration
	RetryInterval time.Duration
	RetainFull    int
	RetainDays    int
	MetricsAddr   string
}

// ReadFileFunc lê um arquivo de segredo.
type ReadFileFunc func(path string) ([]byte, error)

// Load lê a configuração. Destino e chave de cifragem são obrigatórios; a
// chave é a codificação base64 de 32 bytes aleatórios (openssl rand -base64 32).
func Load(getenv func(string) string, readFile ReadFileFunc) (Config, error) {
	var errs []error
	secret := func(name string) string {
		v, err := secretValue(getenv, readFile, name)
		if err != nil {
			errs = append(errs, err)
		}
		return v
	}
	c := Config{
		S3Prefix:        strings.TrimSpace(getenv(EnvS3Prefix)),
		S3Endpoint:      strings.TrimSpace(getenv(EnvS3Endpoint)),
		S3Region:        strings.TrimSpace(getenv(EnvS3Region)),
		AccessKeyID:     secret(EnvS3AccessKeyID),
		SecretAccessKey: secret(EnvS3SecretAccessKey),
		PGData:          strings.TrimSpace(getenv("PGDATA")),
		PGPassword:      secret(EnvPGPassword),
		MetricsAddr:     strings.TrimSpace(getenv(EnvMetricsAddr)),
	}
	if c.S3Region == "" {
		c.S3Region = "us-east-1"
	}
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9187"
	}
	if c.S3Prefix == "" {
		errs = append(errs, fmt.Errorf("%s é obrigatório (destino do backup, por exemplo s3://bucket/recorvia)", EnvS3Prefix))
	} else if !strings.HasPrefix(c.S3Prefix, "s3://") || len(strings.TrimPrefix(c.S3Prefix, "s3://")) == 0 {
		errs = append(errs, fmt.Errorf("%s deve ter a forma s3://bucket[/caminho]", EnvS3Prefix))
	}
	if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
		errs = append(errs, fmt.Errorf("%s e %s devem ser informados juntos (ou nenhum dos dois, para a credencial do ambiente)",
			EnvS3AccessKeyID, EnvS3SecretAccessKey))
	}
	var err error
	if c.S3ForcePath, err = parseBool(getenv(EnvS3ForcePathStyle), false); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvS3ForcePathStyle, err))
	}
	if err := c.loadKey(getenv, readFile); err != nil {
		errs = append(errs, err)
	}
	if c.BaseInterval, err = parseDuration(getenv(EnvBaseInterval), 24*time.Hour, time.Minute); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvBaseInterval, err))
	}
	if c.RetryInterval, err = parseDuration(getenv(EnvRetryInterval), 15*time.Minute, time.Second); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvRetryInterval, err))
	}
	if c.RetainFull, err = parseInt(getenv(EnvRetainFull), 7, 1); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvRetainFull, err))
	}
	if c.RetainDays, err = parseInt(getenv(EnvRetainDays), 7, 0); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvRetainDays, err))
	}
	return c, errors.Join(errs...)
}

// loadKey lê a chave de cifragem do backup da variável ou do arquivo.
func (c *Config) loadKey(getenv func(string) string, readFile ReadFileFunc) error {
	value, file := getenv(EnvEncryptionKey), strings.TrimSpace(getenv(EnvEncryptionKey+fileSuffix))
	switch {
	case value != "" && file != "":
		return fmt.Errorf("informe %s ou %s, não os dois", EnvEncryptionKey, EnvEncryptionKey+fileSuffix)
	case value == "" && file == "":
		return fmt.Errorf("chave de cifragem do backup ausente: defina %s ou %s (32 bytes em base64; nada é enviado nem restaurado sem ela)",
			EnvEncryptionKey, EnvEncryptionKey+fileSuffix)
	case file != "":
		b, err := readFile(file)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvEncryptionKey+fileSuffix, err)
		}
		value, c.keyFile = string(b), file
	}
	value = strings.TrimSpace(value)
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != KeySize {
		return fmt.Errorf("chave de cifragem do backup inválida: esperado base64 de %d bytes (openssl rand -base64 32)", KeySize)
	}
	c.key, c.keyB64 = key, value
	return nil
}

// secretValue lê NOME ou, se definido, o arquivo de NOME_FILE.
func secretValue(getenv func(string) string, readFile ReadFileFunc, name string) (string, error) {
	value, file := getenv(name), strings.TrimSpace(getenv(name+fileSuffix))
	if file == "" {
		return value, nil
	}
	if value != "" {
		return "", fmt.Errorf("informe %s ou %s, não os dois", name, name+fileSuffix)
	}
	b, err := readFile(file)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name+fileSuffix, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// KeyFingerprint identifica a chave sem revelá-la: os 16 primeiros dígitos
// hexadecimais do HMAC-SHA256 de um texto fixo com a chave. Vai nos dados do
// backup base para que a restauração recuse uma chave diferente antes de
// baixar qualquer arquivo.
func (c Config) KeyFingerprint() string {
	return Fingerprint(c.key)
}

// Fingerprint calcula a impressão digital de uma chave.
func Fingerprint(key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("recorvia-pgbackup/key-fingerprint/v1"))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// WalgEnv devolve o ambiente dos processos do WAL-G: o ambiente recebido
// (variáveis do PostgreSQL e ajustes avançados WALG_*) sem as variáveis
// RECORVIA_BACKUP_* e sem variáveis do WAL-G que a configuração define, mais a
// tradução da configuração.
func (c Config) WalgEnv(base []string) []string {
	managed := map[string]bool{"WALG_S3_PREFIX": true, "AWS_ENDPOINT": true, "AWS_REGION": true, "AWS_S3_FORCE_PATH_STYLE": true,
		"AWS_ACCESS_KEY_ID": true, "AWS_SECRET_ACCESS_KEY": true, "WALG_LIBSODIUM_KEY": true, "WALG_LIBSODIUM_KEY_PATH": true,
		"WALG_LIBSODIUM_KEY_TRANSFORM": true, "WALG_PGP_KEY": true, "WALG_PGP_KEY_PATH": true, "WALG_GPG_KEY_ID": true,
		"WALG_PREVENT_WAL_OVERWRITE": true, "PGPASSWORD": c.PGPassword != ""}
	env := make([]string, 0, len(base)+12)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "RECORVIA_BACKUP_") || managed[name] {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"WALG_S3_PREFIX="+c.S3Prefix,
		"AWS_REGION="+c.S3Region,
		"AWS_S3_FORCE_PATH_STYLE="+strconv.FormatBool(c.S3ForcePath),
		// O WAL-G recusa sobrescrever no armazenamento um segmento já
		// arquivado com conteúdo diferente (por exemplo, dois servidores
		// arquivando no mesmo destino).
		"WALG_PREVENT_WAL_OVERWRITE=true",
		"WALG_LIBSODIUM_KEY_TRANSFORM=base64",
	)
	if c.S3Endpoint != "" {
		env = append(env, "AWS_ENDPOINT="+c.S3Endpoint)
	}
	if c.AccessKeyID != "" {
		env = append(env, "AWS_ACCESS_KEY_ID="+c.AccessKeyID, "AWS_SECRET_ACCESS_KEY="+c.SecretAccessKey)
	}
	if c.keyFile != "" {
		env = append(env, "WALG_LIBSODIUM_KEY_PATH="+c.keyFile)
	} else {
		env = append(env, "WALG_LIBSODIUM_KEY="+c.keyB64)
	}
	if c.PGPassword != "" {
		env = append(env, "PGPASSWORD="+c.PGPassword)
	}
	return env
}

func parseBool(s string, def bool) (bool, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(s))
	if err != nil {
		return false, fmt.Errorf("valor booleano inválido %q", s)
	}
	return b, nil
}

func parseDuration(s string, def, minimum time.Duration) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d < minimum {
		return 0, fmt.Errorf("duração inválida %q (mínimo %s)", s, minimum)
	}
	return d, nil
}

func parseInt(s string, def, minimum int) (int, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < minimum {
		return 0, fmt.Errorf("número inválido %q (mínimo %d)", s, minimum)
	}
	return n, nil
}
