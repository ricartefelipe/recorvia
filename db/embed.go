// Package db embute as migrations SQL versionadas (Goose) no binário de
// migração, garantindo que o schema aplicado corresponda à versão do código.
package db

import "embed"

// Migrations contém os arquivos de db/migrations.
//
//go:embed migrations/*.sql
var Migrations embed.FS
