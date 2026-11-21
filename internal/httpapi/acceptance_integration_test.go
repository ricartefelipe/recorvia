//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/testsupport/pgtest"
)

// lcDocSeq gera bases de CNPJ distintas das usadas por fixture.NewCustomer.
var lcDocSeq = 70000000

// lcCNPJ devolve um CNPJ com dígitos verificadores válidos e inédito no teste.
func lcCNPJ(t *testing.T) string {
	t.Helper()
	lcDocSeq++
	base := fmt.Sprintf("%08d0001", lcDocSeq)
	dv, err := brdoc.CNPJCheckDigits(base)
	if err != nil {
		t.Fatal(err)
	}
	return base + dv
}

// lcCustomerBody monta o corpo de criação ou alteração de cliente PJ.
func lcCustomerBody(name, document, externalID string) map[string]any {
	return map[string]any{"kind": "company", "name": name, "document_number": document, "external_id": externalID}
}

// RF-03: o identificador externo é único por tenant. A duplicidade responde
// 409 external_id_taken na criação e na alteração, sem efeito no cadastro; o
// mesmo identificador em outra organização é aceito.
func TestAcceptanceCustomerExternalIDUniquePerTenant(t *testing.T) {
	a := newAPI(t)
	tnA, tnB := a.env.NewTenant(t, "ext-a"), a.env.NewTenant(t, "ext-b")
	cA, cB := a.login(tnA.Owner, &tnA.ID), a.login(tnB.Owner, &tnB.ID)
	const ext = "erp:cliente-0042"

	first, _ := cA.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", lcCustomerBody("Empresa Original", lcCNPJ(t), ext))
	if first["external_id"] != ext {
		t.Fatalf("identificador externo gravado: %v", first)
	}
	cA.expectProblem(http.StatusConflict, "external_id_taken", http.MethodPost, "/api/v1/customers",
		lcCustomerBody("Empresa Duplicada", lcCNPJ(t), ext))
	list, _ := cA.expect(http.StatusOK, http.MethodGet, "/api/v1/customers", nil)
	if items := objects(t, list["data"]); len(items) != 1 || items[0]["id"] != first["id"] {
		t.Fatalf("a criação recusada não pode gravar cliente: %v", items)
	}

	// Alteração de outro cliente para o identificador já usado.
	doc := lcCNPJ(t)
	other, h := cA.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", lcCustomerBody("Outra Empresa", doc, "erp:cliente-0043"))
	otherPath := "/api/v1/customers/" + other["id"].(string)
	cA.expectProblem(http.StatusConflict, "external_id_taken", http.MethodPatch, otherPath, lcCustomerBody("Outra Empresa", doc, ext),
		"If-Match", h.Get("ETag"))
	got, gh := cA.expect(http.StatusOK, http.MethodGet, otherPath, nil)
	if got["external_id"] != "erp:cliente-0043" || gh.Get("ETag") != h.Get("ETag") {
		t.Fatalf("a alteração recusada não pode mudar o cliente: %v (ETag %s, antes %s)", got, gh.Get("ETag"), h.Get("ETag"))
	}
	// Manter o próprio identificador numa alteração comum não é duplicidade.
	firstPath := "/api/v1/customers/" + first["id"].(string)
	renamed, _ := cA.expect(http.StatusOK, http.MethodPatch, firstPath, lcCustomerBody("Empresa Renomeada", first["document_number"].(string), ext),
		"If-Match", `"1"`)
	if renamed["name"] != "Empresa Renomeada" || renamed["external_id"] != ext {
		t.Fatalf("alteração mantendo o identificador: %v", renamed)
	}

	// Outra organização usa o mesmo identificador, na criação e na alteração.
	inB, _ := cB.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", lcCustomerBody("Empresa do Tenant B", lcCNPJ(t), ext))
	if inB["external_id"] != ext {
		t.Fatalf("mesmo identificador em outro tenant: %v", inB)
	}
	docB := lcCNPJ(t)
	otherB, hb := cB.expect(http.StatusCreated, http.MethodPost, "/api/v1/customers", lcCustomerBody("Segunda do Tenant B", docB, "erp:b-1"))
	cB.expectProblem(http.StatusConflict, "external_id_taken", http.MethodPatch, "/api/v1/customers/"+otherB["id"].(string),
		lcCustomerBody("Segunda do Tenant B", docB, ext), "If-Match", hb.Get("ETag"))
}

// lcPriceVersionIDs devolve o identificador do preço de cada item da fatura.
func lcPriceVersionIDs(t *testing.T, inv map[string]any) []any {
	t.Helper()
	var out []any
	for _, it := range objects(t, inv["items"]) {
		out = append(out, it["price_version_id"])
	}
	return out
}

// RF-04: nova versão de preço pela API não altera a assinatura existente, a
// fatura já emitida nem o ciclo seguinte dessa assinatura; só assinaturas
// novas usam a versão nova.
func TestAcceptanceNewPriceVersionKeepsExistingContracts(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "preco-versao")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Versionado", flatPrice(10000))
	v1 := plan.CurrentPriceVersionID.String()
	sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
	subPath := "/api/v1/subscriptions/" + sub.ID.String()
	issued := issuedInvoice(c, sub.ID)
	invPath := "/api/v1/invoices/" + issued["id"].(string)
	before, beforeH := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	if before["total"] != float64(10000) || !reflect.DeepEqual(lcPriceVersionIDs(t, before), []any{v1}) {
		t.Fatalf("fatura emitida com a versão 1: %v", before)
	}
	_, subH := c.expect(http.StatusOK, http.MethodGet, subPath, nil)

	pv, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/plans/"+plan.ID.String()+"/prices",
		map[string]any{"pricing_model": "flat", "flat_amount": 15000})
	v2, _ := pv["id"].(string)
	if pv["version"] != float64(2) || pv["flat_amount"] != float64(15000) || v2 == "" || v2 == v1 {
		t.Fatalf("nova versão de preço: %v", pv)
	}
	gotPlan, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/plans/"+plan.ID.String(), nil)
	cur, _ := gotPlan["current_price"].(map[string]any)
	if cur["id"] != v2 || len(objects(t, gotPlan["prices"])) != 2 {
		t.Fatalf("plano deveria ter a versão 2 vigente e as duas versões: %v", gotPlan)
	}

	gotSub, h := c.expect(http.StatusOK, http.MethodGet, subPath, nil)
	if gotSub["price_version_id"] != v1 || h.Get("ETag") != subH.Get("ETag") {
		t.Fatalf("assinatura existente alterada pela nova versão: %v (ETag %s, antes %s)", gotSub, h.Get("ETag"), subH.Get("ETag"))
	}
	after, afterH := c.expect(http.StatusOK, http.MethodGet, invPath, nil)
	if !reflect.DeepEqual(after["items"], before["items"]) || after["total"] != before["total"] || after["amount_due"] != before["amount_due"] ||
		afterH.Get("ETag") != beforeH.Get("ETag") {
		t.Fatalf("fatura emitida alterada pela nova versão:\n%v\n%v", before, after)
	}

	// Assinatura criada depois da nova versão usa a versão 2.
	newer, _ := c.expect(http.StatusCreated, http.MethodPost, "/api/v1/subscriptions", map[string]any{"customer_id": a.env.NewCustomer(t, tn).ID,
		"plan_id": plan.ID, "trial_days": 0})
	if newer["price_version_id"] != v2 {
		t.Fatalf("assinatura nova deveria usar a versão 2: %v", newer)
	}

	// Ciclo seguinte da assinatura existente: continua na versão 1.
	a.env.Clock.Set(sub.CurrentPeriod.End.Add(10 * time.Hour))
	c = a.login(tn.Owner, &tn.ID)
	runBilling(c)
	invoices := subscriptionInvoices(c, sub.ID)
	if len(invoices) != 2 {
		t.Fatalf("esperadas duas faturas da assinatura existente: %v", invoices)
	}
	for _, inv := range invoices {
		if inv["id"] == issued["id"] {
			continue
		}
		next, _ := c.expect(http.StatusOK, http.MethodGet, "/api/v1/invoices/"+inv["id"].(string), nil)
		if next["total"] != float64(10000) || !reflect.DeepEqual(lcPriceVersionIDs(t, next), []any{v1}) {
			t.Fatalf("ciclo seguinte deveria manter a versão 1: %v", next)
		}
		if !instant(t, next["billing_period_start"]).Equal(sub.CurrentPeriod.End) {
			t.Fatalf("período do ciclo seguinte: %v, esperado início %s", next["billing_period_start"], sub.CurrentPeriod.End)
		}
	}
	newerInvoices := subscriptionInvoices(c, uuid.MustParse(newer["id"].(string)))
	if len(newerInvoices) == 0 {
		t.Fatal("a assinatura nova deveria ter sido faturada")
	}
	for _, inv := range newerInvoices {
		if inv["total"] != float64(15000) {
			t.Fatalf("fatura da assinatura nova deveria usar a versão 2: %v", inv)
		}
	}
}

// RF-04: a versão de preço é imutável no banco. O dono das tabelas (que tem
// privilégio de UPDATE e DELETE e não é limitado pela RLS) é barrado pelo
// gatilho; o papel da aplicação nem tem o privilégio.
func TestAcceptancePriceVersionImmutableForTableOwner(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "preco-imutavel")
	plan := newPlan(t, a.env, tn, "Plano Imutável", flatPrice(10000))
	pvID := *plan.CurrentPriceVersionID
	ctx := context.Background()
	owner := pgtest.OwnerPool(t)

	for _, tc := range []struct {
		op, stmt string
	}{
		{"UPDATE", `UPDATE plan_price_versions SET flat_amount = 1 WHERE tenant_id = $1 AND id = $2`},
		{"DELETE", `DELETE FROM plan_price_versions WHERE tenant_id = $1 AND id = $2`},
	} {
		t.Run(tc.op, func(t *testing.T) {
			_, err := owner.Exec(ctx, tc.stmt, tn.ID, pvID)
			var pgErr *pgconn.PgError
			want := "tabela plan_price_versions é imutável: operação " + tc.op + " não permitida"
			if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || pgErr.Message != want {
				t.Fatalf("dono das tabelas: esperado P0001 %q, obtido %v", want, err)
			}
			err = a.env.DB.InTenantTx(ctx, tn.ID, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.stmt, tn.ID, pvID)
				return err
			})
			if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Fatalf("papel da aplicação: esperado 42501 (sem privilégio), obtido %v", err)
			}
		})
	}
	var amount int64
	if err := owner.QueryRow(ctx, `SELECT flat_amount FROM plan_price_versions WHERE tenant_id = $1 AND id = $2`, tn.ID, pvID).Scan(&amount); err != nil ||
		amount != 10000 {
		t.Fatalf("versão de preço deveria continuar intacta: %d %v", amount, err)
	}
}

// RF-05: transições inválidas respondem 409 com código estável pela API e
// não alteram a assinatura (a versão permanece).
func TestAcceptanceSubscriptionInvalidTransitionsOverHTTP(t *testing.T) {
	a := newAPI(t)
	tn := a.env.NewTenant(t, "transicoes")
	c := a.login(tn.Owner, &tn.ID)
	plan := newPlan(t, a.env, tn, "Plano Transições", flatPrice(10000))

	// prepare cria a assinatura e executa as ações que levam à situação.
	prepare := func(t *testing.T, actions ...[2]any) string {
		t.Helper()
		sub := newSubscription(t, a.env, tn, plan.ID, 1, nil)
		path := "/api/v1/subscriptions/" + sub.ID.String()
		for _, act := range actions {
			c.expect(http.StatusOK, http.MethodPost, path+act[0].(string), act[1], "If-Match", subscriptionETag(c, sub.ID))
		}
		return path
	}
	cancelNow := [2]any{"/cancel", map[string]any{"mode": "immediate", "reason": "Encerramento"}}
	cancelLater := [2]any{"/cancel", map[string]any{"mode": "period_end", "reason": "Encerramento"}}
	suspend := [2]any{"/suspend", map[string]any{"reason": "Inadimplência"}}

	for _, tc := range []struct {
		name   string
		setup  [][2]any
		action string
		body   any
		code   string
	}{
		{"suspender cancelada", [][2]any{cancelNow}, "/suspend", map[string]any{"reason": "Teste"}, "invalid_transition"},
		{"suspender suspensa", [][2]any{suspend}, "/suspend", map[string]any{"reason": "Teste"}, "invalid_transition"},
		{"reativar ativa", nil, "/reactivate", nil, "invalid_transition"},
		{"reativar cancelada", [][2]any{cancelNow}, "/reactivate", nil, "invalid_transition"},
		{"cancelar de imediato cancelada", [][2]any{cancelNow}, "/cancel", map[string]any{"mode": "immediate"}, "invalid_transition"},
		{"cancelar ao fim do período cancelada", [][2]any{cancelNow}, "/cancel", map[string]any{"mode": "period_end"}, "invalid_transition"},
		{"agendar cancelamento já agendado", [][2]any{cancelLater}, "/cancel", map[string]any{"mode": "period_end"}, "already_scheduled"},
		{"retomar renovação sem agendamento", nil, "/resume-renewal", nil, "no_scheduled_cancel"},
		{"retomar renovação de cancelada", [][2]any{cancelNow}, "/resume-renewal", nil, "no_scheduled_cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := prepare(t, tc.setup...)
			before, h := c.expect(http.StatusOK, http.MethodGet, path, nil)
			c.expectProblem(http.StatusConflict, tc.code, http.MethodPost, path+tc.action, tc.body, "If-Match", h.Get("ETag"))
			after, ah := c.expect(http.StatusOK, http.MethodGet, path, nil)
			if ah.Get("ETag") != h.Get("ETag") || after["status"] != before["status"] || after["cancel_at_period_end"] != before["cancel_at_period_end"] {
				t.Fatalf("transição recusada alterou a assinatura:\n%v\n%v", before, after)
			}
		})
	}
}
