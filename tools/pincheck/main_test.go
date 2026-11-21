package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// pinned é um repositório mínimo com tudo fixado.
func pinned() map[string]string {
	return map[string]string{
		"go.mod": "module exemplo\n\ngo 1.27\n\ntoolchain go1.27.4\n",
		".github/workflows/ci.yml": `jobs:
  a:
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v7.0.1
      - uses: ./.github/actions/local
      - name: x
        uses: "golangci/golangci-lint-action@ba0d7d2ec06a0ea1cb5fa41b2e4a3ab91d21278a"
`,
		"web/package.json": `{"dependencies": {"react": "19.3.1"}, "devDependencies": {"vite": "7.3.8", "x": "1.0.0-rc.1"}}`,
		"compose.yaml": `services:
  db:
    image: postgres:18.7-alpine
  app:
    image: "recorvia-api:local"
  cache:
    image: registry.example.test:5000/redis@sha256:` + strings.Repeat("a", 64) + `
`,
		"deploy/backup/dr.compose.yaml": "services:\n  api:\n    image: recorvia-api:${RECORVIA_DR_IMAGE_TAG:-dr}\n" +
			"  db:\n    image: postgres:${PG_TAG:-18.7-alpine}\n",
		"deploy/Dockerfile": `ARG BASE=golang:1.27.4-alpine3.24
FROM ${BASE} AS build
FROM build AS test
FROM scratch
COPY --from=build /x /x
`,
		"web/Dockerfile": "FROM --platform=linux/amd64 node:22.23.5-alpine3.24 AS build\nFROM nginxinc/nginx-unprivileged:1.31.7-alpine3.24\n",
		"internal/x/x_integration_test.go": `package x

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

const Image = "axllent/mailpit:v1.33.1"

func TestX(t *testing.T) {
	_, _ = tcredis.Run(t.Context(), "redis:8.8.4-alpine")
	_, _ = testcontainers.Run(t.Context(), Image)
	_, _ = testcontainers.Run(t.Context(), "")
	_ = testcontainers.ContainerRequest{Image: "rabbitmq:4.2.9-management-alpine"}
}
`,
		"internal/y/y.go": `package y

import "github.com/ricartefelipe/recorvia/internal/x"

func Run(a, b string) {}

var _ = x.Image
`,
		"internal/z/z_test.go": `package z

import (
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"exemplo/internal/x"
)

func TestZ(t *testing.T) { _, _ = testcontainers.Run(t.Context(), x.Image) }
`,
	}
}

func TestPinnedRepositoryPasses(t *testing.T) {
	root := t.TempDir()
	write(t, root, pinned())
	problems, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("violações inesperadas: %v", problems)
	}
}

// Cada regressão de fixação é apontada com arquivo, linha e motivo.
func TestRegressionsAreReported(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"action por tag", map[string]string{".github/workflows/ci.yml": "steps:\n  - uses: actions/checkout@v4\n"},
			".github/workflows/ci.yml:2: action sem SHA de 40 caracteres: actions/checkout@v4"},
		{"action por SHA curto", map[string]string{".github/workflows/ci.yml": "steps:\n  - uses: actions/checkout@3d3c42e\n"},
			"action sem SHA de 40 caracteres"},
		{"action sem versão", map[string]string{".github/actions/a/action.yml": "runs:\n  steps:\n    - uses: owner/repo\n"},
			".github/actions/a/action.yml:3: action sem SHA de 40 caracteres: owner/repo"},
		{"action docker sem digest", map[string]string{".github/workflows/ci.yml": "steps:\n  - uses: docker://alpine:3.24\n"},
			"imagem de action sem digest"},
		{"dependência com circunflexo", map[string]string{"web/package.json": `{"dependencies": {"react": "^19.3.1"}}`},
			"web/package.json: dependencies.react sem versão exata: ^19.3.1"},
		{"dependência de desenvolvimento com til", map[string]string{"web/package.json": `{"devDependencies": {"vite": "~7.3.8"}}`},
			"devDependencies.vite sem versão exata"},
		{"dependência latest", map[string]string{"web/package.json": `{"dependencies": {"zod": "latest"}}`}, "dependencies.zod sem versão exata"},
		{"dependência por intervalo", map[string]string{"web/package.json": `{"dependencies": {"zod": ">=4 <5"}}`}, "dependencies.zod sem versão exata"},
		{"dependência por URL", map[string]string{"web/package.json": `{"dependencies": {"zod": "github:colinhacks/zod"}}`},
			"dependencies.zod sem versão exata"},
		{"compose com latest", map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:latest\n"},
			"compose.yaml:3: imagem com tag latest: postgres:latest"},
		{"compose sem tag", map[string]string{"compose.yaml": "services:\n  db:\n    image: \"registry.example.test:5000/postgres\"\n"},
			"compose.yaml:3: imagem sem tag fixa: registry.example.test:5000/postgres"},
		{"compose com prefixo e padrão latest", map[string]string{"deploy/backup/dr.compose.yaml": "services:\n  db:\n    image: postgres:${PG_TAG:-latest}\n"},
			"deploy/backup/dr.compose.yaml:3: imagem com tag latest: postgres:latest"},
		{"compose com variável sem padrão", map[string]string{"compose.override.yaml": "services:\n  db:\n    image: postgres:${PG_TAG}\n"},
			"compose.override.yaml:3: imagem com variável não resolvida: postgres:${PG_TAG}"},
		{"compose com tag só de major", map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:18\n"},
			"compose.yaml:3: imagem com tag flutuante (use versão com ao menos major.minor): postgres:18"},
		{"compose com tag lts", map[string]string{"compose.yaml": "services:\n  web:\n    image: node:lts-alpine\n"}, "imagem com tag flutuante"},
		{"imagem local com latest", map[string]string{"compose.yaml": "services:\n  api:\n    image: recorvia-api:latest\n"},
			"imagem com tag latest: recorvia-api:latest"},
		{"imagem de registro com prefixo local", map[string]string{"compose.yaml": "services:\n  api:\n    image: ghcr.io/x/recorvia-api:main\n"},
			"imagem com tag flutuante"},
		{"Dockerfile com tag alpine", map[string]string{"deploy/x/Dockerfile": "FROM redis:alpine\n"}, "deploy/x/Dockerfile:1: imagem com tag flutuante"},
		{"Testcontainers com tag de major", map[string]string{"internal/a/a_test.go": `package a

import tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

func f() { _, _ = tcredis.Run(nil, "redis:8-alpine") }
`}, "internal/a/a_test.go:5: imagem com tag flutuante"},
		{"Dockerfile sem tag", map[string]string{"deploy/x/Dockerfile": "FROM alpine\n"}, "deploy/x/Dockerfile:1: imagem sem tag fixa: alpine"},
		{"Dockerfile com latest", map[string]string{"deploy/go.Dockerfile": "FROM golang:1.27 AS build\nFROM gcr.io/distroless/static:latest\n"},
			"deploy/go.Dockerfile:2: imagem com tag latest: gcr.io/distroless/static:latest"},
		{"Dockerfile com variável sem valor", map[string]string{"Dockerfile": "ARG BASE\nFROM ${BASE}\n"},
			"Dockerfile:2: imagem com variável não resolvida: ${BASE}"},
		{"Testcontainers com latest", map[string]string{"internal/a/a_test.go": `package a

import tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

func f() { _, _ = tcredis.Run(nil, "redis:latest") }
`}, "internal/a/a_test.go:5: imagem com tag latest: redis:latest"},
		{"Testcontainers por constante sem tag", map[string]string{"internal/a/a.go": `package a

import "github.com/testcontainers/testcontainers-go"

const Image = "axllent/mailpit"

func f() { _, _ = testcontainers.Run(nil, Image) }
`}, "internal/a/a.go:7: imagem sem tag fixa: axllent/mailpit"},
		{"Testcontainers em requisição", map[string]string{"internal/a/a.go": `package a

import "github.com/testcontainers/testcontainers-go"

var r = testcontainers.ContainerRequest{Image: "rabbitmq"}
`}, "internal/a/a.go:5: imagem sem tag fixa: rabbitmq"},
		{"Testcontainers com imagem calculada", map[string]string{"internal/a/a.go": `package a

import "github.com/testcontainers/testcontainers-go"

func f(img string) { _, _ = testcontainers.Run(nil, img) }
`}, "internal/a/a.go:5: imagem do Testcontainers não verificável"},
		{"go.mod sem toolchain", map[string]string{"go.mod": "module exemplo\n\ngo 1.27\n"}, "go.mod: sem diretiva toolchain"},
		{"go.mod com toolchain incompleta", map[string]string{"go.mod": "module exemplo\n\ngo 1.27\n\ntoolchain go1.27\n"},
			"go.mod: sem diretiva toolchain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := pinned()
			for k, v := range tc.files {
				files[k] = v
			}
			write(t, root, files)
			problems, err := Check(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("violações %q, esperado uma com %q", problems, tc.want)
			}
		})
	}
}

// O próprio repositório está fixado: make test e a CI falham com uma
// regressão.
func TestRepositoryIsPinned(t *testing.T) {
	problems, err := Check(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("versões sem fixação no repositório:\n%s", strings.Join(problems, "\n"))
	}
}
