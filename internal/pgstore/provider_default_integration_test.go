//go:build integration

package pgstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/ids"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/testsupport/fixture"
)

// Trocar o provedor padrão tira o padrão da outra conta e lhe dá versão nova:
// quem leu a outra conta antes da troca recebe 412 ao gravá-la, e a troca
// feita por outra pessoa não é desfeita em silêncio (ADR-083).
func TestProviderDefaultChangeBumpsOtherAccountVersion(t *testing.T) {
	ctx := context.Background()
	env := fixture.New(t, 2, time.Now().UTC())
	tn := env.NewTenant(t, "provedor-padrao")
	accountIDs := map[string]uuid.UUID{"simulator": ids.New(), "asaas": ids.New()}
	save := func(provider string, isDefault bool, expected int) error {
		now := env.Clock.Now()
		return env.Store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
			return tx.SaveAccount(ctx, payments.ProviderAccount{ID: accountIDs[provider], TenantID: tn.ID, Provider: provider,
				Environment: "local", Status: "active", IsDefault: isDefault, APIKey: "chave_" + provider + "_0001", APIKeyHint: "0001",
				Secret: strings.Repeat("s", 40), CreatedAt: now, UpdatedAt: now}, []byte("hash-"+provider+"-"+now.String()), expected)
		})
	}
	state := func() map[string]payments.ProviderAccount {
		t.Helper()
		out := map[string]payments.ProviderAccount{}
		err := env.Store.Payments().InTenant(ctx, tn.ID, func(tx payments.Tx) error {
			accounts, err := tx.Accounts(ctx)
			for _, a := range accounts {
				out[a.Provider] = a
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	if err := save("simulator", true, 0); err != nil {
		t.Fatal(err)
	}
	if err := save("asaas", false, 0); err != nil {
		t.Fatal(err)
	}
	before := state()
	if before["simulator"].Version != 1 || !before["simulator"].IsDefault || before["asaas"].Version != 1 {
		t.Fatalf("contas iniciais: %+v", before)
	}

	// Outra pessoa torna o Asaas o provedor padrão.
	if err := save("asaas", true, 1); err != nil {
		t.Fatal(err)
	}
	after := state()
	if after["simulator"].IsDefault || after["simulator"].Version != 2 || !after["asaas"].IsDefault || after["asaas"].Version != 2 {
		t.Fatalf("a conta que perdeu o padrão deveria ganhar versão nova: %+v", after)
	}

	// Quem tinha lido o simulador na versão 1 não desfaz a troca.
	err := save("simulator", true, 1)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "version_mismatch" {
		t.Fatalf("gravação com a versão anterior à troca do padrão: %v", err)
	}
	if final := state(); final["simulator"].IsDefault || !final["asaas"].IsDefault || final["simulator"].Version != 2 || final["asaas"].Version != 2 {
		t.Fatalf("a recusa não pode alterar nenhuma das contas: %+v", final)
	}
}
