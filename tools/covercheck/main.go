// Comando covercheck calcula a cobertura de statements dos pacotes de
// domínio e aplicação a partir de um coverprofile e falha abaixo do limite.
//
// Pacotes considerados: internal/kernel/* e os pacotes raiz de cada módulo de
// negócio (internal/<modulo>). Excluídos por regra: código gerado
// (internal/dbgen), adaptadores (subpacotes de módulos, como <modulo>pg e
// gateways), internal/httpapi, internal/pgstore (adaptador), internal/app
// (raiz de composição), internal/infra/* e suporte a testes.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

const modulePrefix = "github.com/ricartefelipe/recorvia/"

var excludedRoots = map[string]bool{
	"internal/httpapi": true, "internal/dbgen": true, "internal/infra": true,
	"internal/testsupport": true, "internal/paysim": true,
	"internal/app": true, "internal/pgstore": true,
}

func included(pkg string) bool {
	rel := strings.TrimPrefix(pkg, modulePrefix)
	parts := strings.Split(rel, "/")
	if len(parts) < 2 || parts[0] != "internal" {
		return false
	}
	if excludedRoots[parts[0]+"/"+parts[1]] {
		return false
	}
	if parts[1] == "kernel" {
		return len(parts) == 3
	}
	return len(parts) == 2
}

type stat struct{ total, covered int }

func main() {
	profile := flag.String("profile", "coverage.out", "arquivo coverprofile")
	threshold := flag.Float64("threshold", 90, "cobertura mínima agregada (%)")
	flag.Parse()

	f, err := os.Open(*profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Um mesmo bloco pode aparecer várias vezes (vários binários de teste);
	// conta-se cada bloco uma vez, coberto se qualquer execução o cobriu.
	type block struct{ stmts, count int }
	blocks := map[string]block{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		stmts, _ := strconv.Atoi(fields[1])
		count, _ := strconv.Atoi(fields[2])
		b := blocks[fields[0]]
		b.stmts = stmts
		if count > 0 {
			b.count = count
		}
		blocks[fields[0]] = b
	}
	if err := errors.Join(sc.Err(), f.Close()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	perPkg := map[string]*stat{}
	for key, b := range blocks {
		file, _, _ := strings.Cut(key, ":")
		pkg := path.Dir(file)
		if !included(pkg) {
			continue
		}
		s := perPkg[pkg]
		if s == nil {
			s = &stat{}
			perPkg[pkg] = s
		}
		s.total += b.stmts
		if b.count > 0 {
			s.covered += b.stmts
		}
	}
	names := make([]string, 0, len(perPkg))
	for n := range perPkg {
		names = append(names, n)
	}
	sort.Strings(names)
	var agg stat
	fmt.Printf("%-60s %10s\n", "pacote (domínio e aplicação)", "cobertura")
	for _, n := range names {
		s := perPkg[n]
		agg.total += s.total
		agg.covered += s.covered
		fmt.Printf("%-60s %9.1f%%\n", strings.TrimPrefix(n, modulePrefix), pct(*s))
	}
	total := pct(agg)
	fmt.Printf("%-60s %9.1f%% (%d/%d statements)\n", "TOTAL", total, agg.covered, agg.total)
	if agg.total == 0 || total < *threshold {
		fmt.Fprintf(os.Stderr, "cobertura %.1f%% abaixo do mínimo de %.1f%%\n", total, *threshold)
		os.Exit(1)
	}
}

func pct(s stat) float64 {
	if s.total == 0 {
		return 0
	}
	return 100 * float64(s.covered) / float64(s.total)
}
