//go:build !unix

package pgbackup

// FileLock não bloqueia fora de sistemas unix (o agente só roda no container
// Linux do PostgreSQL).
func FileLock(string) (func(), error) { return func() {}, nil }
