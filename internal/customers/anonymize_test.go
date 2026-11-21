package customers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/brdoc"
	"github.com/ricartefelipe/recorvia/internal/outbox"
)

const validReason = "Pedido de eliminação, protocolo 2026/0042"

func fullIndividual() Input {
	return Input{Kind: Individual, Name: "Ana Souza", DocumentNumber: "529.982.247-25", Email: "ana@example.com", Phone: "+55 (11) 98888-7777",
		ExternalID: "crm-ana-01", PaymentTermsDays: ptr(10)}
}

func TestCustomerAnonymizeReplacesPersonalFields(t *testing.T) {
	cases := []struct {
		name       string
		in         Input
		wantDoc    string
		wantFields []string
	}{
		{"pessoa física com todos os campos", fullIndividual(), "00000000000", []string{"name", "document_number", "email", "phone", "external_id"}},
		{"pessoa jurídica com razão social", companyInput(), "00000000000000", []string{"name", "document_number", "legal_name"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(uuid.New(), uuid.New(), tc.in, testNow)
			if err != nil {
				t.Fatal(err)
			}
			before := c
			at := testNow.Add(48 * time.Hour)
			fields := c.Anonymize(at)
			if !slices.Equal(fields, tc.wantFields) {
				t.Fatalf("campos: %v, esperado %v", fields, tc.wantFields)
			}
			if c.Name != AnonymizedName || c.DocumentNumber != tc.wantDoc || c.LegalName != "" || c.Email != "" || c.Phone != "" || c.ExternalID != "" {
				t.Fatalf("dados pessoais restantes: %+v", c)
			}
			if !c.Anonymized() || !c.AnonymizedAt.Equal(at) || c.Status != Archived {
				t.Fatalf("situação: %+v", c)
			}
			// O que não identifica ninguém permanece.
			if c.ID != before.ID || c.Kind != before.Kind || c.DocumentType != before.DocumentType || !equalIntPtr(c.PaymentTermsDays, before.PaymentTermsDays) ||
				!c.CreatedAt.Equal(before.CreatedAt) {
				t.Fatalf("campos não pessoais alterados: %+v", c)
			}
			// O documento neutro não é um documento válido de ninguém.
			if _, err := brdoc.Validate(c.DocumentType, c.DocumentNumber); err == nil {
				t.Fatal("o documento neutro não pode passar na validação")
			}
			if containsPersonalData(c, before) {
				t.Fatalf("o cadastro anonimizado ainda contém dados pessoais: %+v", c)
			}
		})
	}
}

func TestValidateAnonymizationReason(t *testing.T) {
	c, err := New(uuid.New(), uuid.New(), fullIndividual(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	company, err := New(uuid.New(), uuid.New(), companyInput(), testNow)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		reason string
		c      Customer
		ok     bool
	}{
		{"válida", validReason, c, true},
		{"espaços normalizados", "  Pedido   de eliminação,\n protocolo 77  ", c, true},
		{"no limite mínimo", "0123456789", c, true},
		{"no limite máximo", strings.Repeat("á", MaxAnonymizationReason), c, true},
		{"vazia", "   ", c, false},
		{"curta", "pedido 1", c, false},
		{"longa", strings.Repeat("a", MaxAnonymizationReason+1), c, false},
		{"com o nome", "Pedido da titular ana souza, protocolo 9", c, false},
		{"com o e-mail", "Pedido recebido de ANA@example.com em maio", c, false},
		{"com o CPF formatado", "Titular do CPF 529.982.247-25 pediu a eliminação", c, false},
		{"com o CPF sem pontuação", "Titular 52998224725 pediu a eliminação", c, false},
		{"com o telefone", "Pedido por telefone +55 11 98888-7777 hoje", c, false},
		{"com a razão social", "Encerramento da Acme Software Ltda, protocolo 12", company, false},
		{"nome curto não é comparado", "Pedido da empresa acme, protocolo 12", company, true},
		{"sem cliente (formato apenas)", "Qualquer texto com tamanho válido", Customer{}, true},
		{"com caractere NUL", "Pedido de eliminação\x00, protocolo 12", c, false},
		{"com caractere de controle", "Pedido de eliminação\x1b[0m, protocolo 12", c, false},
		{"com UTF-8 inválido", "Pedido de eliminação \xff\xfe protocolo 12", c, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateAnonymizationReason(tc.reason, tc.c)
			if tc.ok {
				if err != nil {
					t.Fatalf("erro inesperado: %v", err)
				}
				if got != strings.Join(strings.Fields(tc.reason), " ") {
					t.Fatalf("justificativa normalizada: %q", got)
				}
				return
			}
			if fields := assertAppErr(t, err, apperr.KindInvalid, "invalid_anonymization"); !slices.Equal(fields, []string{"reason"}) {
				t.Fatalf("campos: %v", fields)
			}
		})
	}
}

func (e *svcEnv) seedPersonalData(c Customer) {
	st := e.st()
	e.seedContacts(c.ID, 3)
	st.addresses[c.ID] = Address{Street: "Rua das Flores", Number: "10", District: "Centro", City: "São Paulo", State: "SP", PostalCode: "01001000", Country: "BR"}
	st.providerRefs[c.ID] = []ProviderRef{{Provider: "asaas", ProviderCustomerID: "cus_000123"}, {Provider: "simulator", ProviderCustomerID: "sim_9"}}
	st.portalLinks[c.ID], st.notifications[c.ID], st.webhookPayloads[c.ID] = 1, 7, 4
}

func TestAnonymize(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, fullIndividual())
	other := e.seed(t, companyInput())
	e.seedPersonalData(c)
	e.seedContacts(other.ID, 2)

	got, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, "  "+validReason+"  ")
	if err != nil {
		t.Fatal(err)
	}
	st := e.st()
	saved := st.customers[c.ID]
	if got.AlreadyAnonymized || got.Customer != saved || saved.Version != 2 {
		t.Fatalf("resultado: %+v, gravado: %+v", got, saved)
	}
	// A resposta traz o instante de atualização gravado pelo banco, não o
	// que o cliente tinha antes da alteração.
	if !got.Customer.UpdatedAt.Equal(touchedAt) || got.Customer.UpdatedAt.Equal(c.UpdatedAt) {
		t.Fatalf("updated_at devolvido: %v, gravado: %v, anterior: %v", got.Customer.UpdatedAt, touchedAt, c.UpdatedAt)
	}
	// A pré-condição de faturamento pendente é avaliada no instante do relógio.
	if !st.pendingAskedAt.Equal(testNow) {
		t.Fatalf("instante da verificação de faturamento pendente: %v", st.pendingAskedAt)
	}
	if saved.Name != AnonymizedName || saved.DocumentNumber != "00000000000" || saved.Email != "" || saved.Phone != "" || saved.ExternalID != "" ||
		saved.Status != Archived || saved.AnonymizedAt == nil || !saved.AnonymizedAt.Equal(testNow) {
		t.Fatalf("cadastro gravado: %+v", saved)
	}
	want := Erasure{Contacts: 3, Address: true, PortalLinksRevoked: 1, Notifications: 7, ProviderWebhookPayloads: 4,
		ProviderRefs: []ProviderRef{{Provider: "asaas", ProviderCustomerID: "cus_000123"}, {Provider: "simulator", ProviderCustomerID: "sim_9"}}}
	if got.Erasure.Contacts != want.Contacts || got.Erasure.Address != want.Address || got.Erasure.PortalLinksRevoked != want.PortalLinksRevoked ||
		got.Erasure.Notifications != want.Notifications || got.Erasure.ProviderWebhookPayloads != want.ProviderWebhookPayloads ||
		!slices.Equal(got.Erasure.ProviderRefs, want.ProviderRefs) {
		t.Fatalf("dados removidos: %+v", got.Erasure)
	}
	if len(st.contacts[c.ID]) != 0 || len(st.providerRefs[c.ID]) != 0 || !st.erasedAt.Equal(testNow) {
		t.Fatal("contatos e identificadores no provedor deveriam ser removidos")
	}
	if _, ok := st.addresses[c.ID]; ok {
		t.Fatal("endereço deveria ser removido")
	}
	if len(st.contacts[other.ID]) != 2 || st.customers[other.ID].Anonymized() {
		t.Fatal("outro cliente não pode ser afetado")
	}

	// Evento para as integrações e auditoria com a justificativa, sem
	// valores pessoais. Os identificadores do provedor ficam somente na
	// auditoria, gravada na mesma transação que os remove do cadastro.
	if len(st.events) != 1 || st.events[0].Type != outbox.EventCustomerUpdated || st.events[0].AggregateID != c.ID {
		t.Fatalf("evento: %+v", st.events)
	}
	payload := st.events[0].Payload.(map[string]any)
	if payload["anonymized"] != true || !slices.Equal(payload["fields"].([]string), []string{"name", "document_number", "email", "phone", "external_id"}) {
		t.Fatalf("payload do evento: %+v", payload)
	}
	if len(st.audits) != 1 {
		t.Fatalf("auditoria: %+v", st.audits)
	}
	a := st.audits[0]
	if a.Action != "customer.anonymized" || a.ResourceType != "customer" || a.ResourceID != c.ID.String() || a.Actor != e.actor || !a.OccurredAt.Equal(testNow) {
		t.Fatalf("entrada de auditoria: %+v", a)
	}
	d := a.Details
	if d["reason"] != validReason || d["previous_status"] != Active || d["contacts_removed"] != 3 || d["address_removed"] != true ||
		d["portal_links_revoked"] != 1 || d["notifications_removed"] != 7 || d["provider_webhook_payloads_cleared"] != 4 ||
		!slices.Equal(d["providers"].([]string), []string{"asaas", "simulator"}) ||
		!slices.Equal(d[AuditProviderCustomers].([]ProviderRef), want.ProviderRefs) {
		t.Fatalf("detalhes da auditoria: %+v", d)
	}
	if containsPersonalData(d, c) || containsPersonalData(payload, c) {
		t.Fatal("auditoria e eventos não podem conter dados pessoais")
	}
	if text := fmt.Sprint(payload); strings.Contains(text, "cus_000123") || strings.Contains(text, "sim_9") {
		t.Fatalf("o evento para as integrações não leva os identificadores do provedor: %v", payload)
	}

	// Idempotente: repetir não altera nada e não audita de novo. Os
	// identificadores do provedor, que já não estão no cadastro, voltam na
	// repetição a partir da auditoria: a resposta da primeira chamada pode
	// ter se perdido depois da confirmação da transação.
	calls := e.store.calls["Anonymize"]
	again, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, "Outra justificativa válida")
	if err != nil {
		t.Fatal(err)
	}
	if !again.AlreadyAnonymized || again.Customer != saved || !slices.Equal(again.Erasure.ProviderRefs, want.ProviderRefs) ||
		again.Erasure.Contacts != 0 || again.Erasure.Address || again.Erasure.Notifications != 0 {
		t.Fatalf("repetição: %+v", again)
	}
	if e.store.calls["Anonymize"] != calls || len(e.st().audits) != 1 || len(e.st().events) != 1 || e.st().customers[c.ID].Version != 2 {
		t.Fatal("a repetição não pode ter efeito")
	}
}

// Cliente já arquivado também pode ser anonimizado; a auditoria registra a
// situação anterior.
func TestAnonymizeArchivedCustomer(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, companyInput())
	if _, err := e.svc.Archive(context.Background(), e.tenantID, e.actor, c.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason)
	if err != nil {
		t.Fatal(err)
	}
	if got.Customer.Version != 3 || !got.Customer.Anonymized() || got.Erasure.Contacts != 0 || got.Erasure.Address {
		t.Fatalf("resultado: %+v", got)
	}
	// Sem identificador no provedor, a auditoria registra a lista vazia e a
	// repetição não devolve nenhum.
	audits := e.st().audits
	last := audits[len(audits)-1]
	if refs, ok := last.Details[AuditProviderCustomers].([]ProviderRef); !ok || refs == nil || len(refs) != 0 {
		t.Fatalf("identificadores na auditoria: %#v", last.Details[AuditProviderCustomers])
	}
	again, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason)
	if err != nil || !again.AlreadyAnonymized || len(again.Erasure.ProviderRefs) != 0 {
		t.Fatalf("repetição sem identificadores: %+v %v", again, err)
	}
	if last.Action != "customer.anonymized" || last.Details["previous_status"] != Archived {
		t.Fatalf("auditoria: %+v", last)
	}
}

func TestAnonymizeRejected(t *testing.T) {
	t.Run("justificativa inválida antes de qualquer leitura", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, fullIndividual())
		_, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, "curta")
		assertAppErr(t, err, apperr.KindInvalid, "invalid_anonymization")
		if e.store.calls["InTenant"] != 0 {
			t.Fatal("o formato da justificativa deve ser conferido antes da transação")
		}
	})
	t.Run("justificativa com dados pessoais do cliente", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, fullIndividual())
		_, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, "Pedido de Ana Souza, protocolo 12")
		assertAppErr(t, err, apperr.KindInvalid, "invalid_anonymization")
		if e.st().customers[c.ID].Anonymized() || len(e.st().audits) != 0 || e.store.calls["HasOpenObligations"] != 0 {
			t.Fatal("nada deveria ser alterado")
		}
	})
	t.Run("cliente com assinatura ativa ou fatura em aberto", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, fullIndividual())
		e.seedPersonalData(c)
		e.st().obligations[c.ID] = true
		_, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason)
		assertAppErr(t, err, apperr.KindConflict, "customer_has_obligations")
		st := e.st()
		if st.customers[c.ID] != c || len(st.contacts[c.ID]) != 3 || len(st.audits) != 0 || len(st.events) != 0 || e.store.calls["Anonymize"] != 0 {
			t.Fatal("cliente com obrigações em aberto não pode ser anonimizado")
		}
	})
	// Sem assinatura ativa nem fatura em aberto, mas com faturamento ainda por
	// acontecer (consumo final de assinatura cancelada, item ou correção a
	// faturar, tolerância de consumo atrasado): a fatura nasceria depois, para
	// um cliente já anonimizado.
	t.Run("cliente com faturamento pendente", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, fullIndividual())
		e.seedPersonalData(c)
		e.st().pendingBilling[c.ID] = true
		_, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason)
		assertAppErr(t, err, apperr.KindConflict, "customer_has_obligations")
		st := e.st()
		if st.customers[c.ID] != c || len(st.contacts[c.ID]) != 3 || len(st.providerRefs[c.ID]) != 2 || len(st.audits) != 0 || len(st.events) != 0 ||
			e.store.calls["Anonymize"] != 0 || e.store.calls["ErasePersonalData"] != 0 {
			t.Fatal("cliente com faturamento pendente não pode ser anonimizado")
		}
		// Resolvida a pendência, o mesmo pedido é aceito.
		e.st().pendingBilling[c.ID] = false
		if _, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason); err != nil {
			t.Fatalf("anonimização depois de resolvido o faturamento: %v", err)
		}
	})
	t.Run("cliente inexistente", func(t *testing.T) {
		e := newSvcEnv()
		_, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, uuid.New(), validReason)
		assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")
	})
	t.Run("cliente de outro tenant", func(t *testing.T) {
		e := newSvcEnv()
		c := e.seed(t, fullIndividual())
		_, err := e.svc.Anonymize(context.Background(), uuid.New(), e.actor, c.ID, validReason)
		assertAppErr(t, err, apperr.KindNotFound, "customer_not_found")
		if e.st().customers[c.ID].Anonymized() {
			t.Fatal("cliente de outro tenant anonimizado")
		}
	})
}

// Qualquer falha desfaz a anonimização inteira: cadastro, dados removidos,
// evento e auditoria são gravados juntos ou não são.
func TestAnonymizeIsAtomic(t *testing.T) {
	for _, op := range []string{"Get", "HasOpenObligations", "HasPendingBilling", "Anonymize", "ErasePersonalData", "Emit", "Audit"} {
		t.Run(op, func(t *testing.T) {
			e := newSvcEnv()
			c := e.seed(t, fullIndividual())
			e.seedPersonalData(c)
			e.store.fail[op] = errBoom
			got, err := e.svc.Anonymize(context.Background(), e.tenantID, e.actor, c.ID, validReason)
			if !errors.Is(err, errBoom) {
				t.Fatalf("esperado erro propagado, obtido %v", err)
			}
			if got.Customer.ID != uuid.Nil || len(got.Erasure.ProviderRefs) != 0 {
				t.Fatalf("resultado parcial devolvido: %+v", got)
			}
			st := e.st()
			if st.customers[c.ID] != c || len(st.contacts[c.ID]) != 3 || len(st.providerRefs[c.ID]) != 2 || len(st.audits) != 0 || len(st.events) != 0 {
				t.Fatal("a anonimização deveria ser desfeita por inteiro")
			}
			if _, ok := st.addresses[c.ID]; !ok {
				t.Fatal("o endereço deveria permanecer")
			}
		})
	}
}

// A repetição que não consegue reler os identificadores do provedor falha em
// vez de devolver uma resposta sem eles.
func TestAnonymizeRepeatedPropagatesProviderRefsFailure(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, fullIndividual())
	e.seedPersonalData(c)
	ctx := context.Background()
	if _, err := e.svc.Anonymize(ctx, e.tenantID, e.actor, c.ID, validReason); err != nil {
		t.Fatal(err)
	}
	e.store.fail["AnonymizationProviderRefs"] = errBoom
	got, err := e.svc.Anonymize(ctx, e.tenantID, e.actor, c.ID, validReason)
	if !errors.Is(err, errBoom) || got.Customer.ID != uuid.Nil || got.AlreadyAnonymized {
		t.Fatalf("repetição com falha na leitura dos identificadores: %+v %v", got, err)
	}
}

// Depois da anonimização, dados pessoais não voltam ao cliente por nenhuma
// operação do cadastro.
func TestAnonymizedCustomerRejectsNewPersonalData(t *testing.T) {
	e := newSvcEnv()
	c := e.seed(t, fullIndividual())
	ctx := context.Background()
	res, err := e.svc.Anonymize(ctx, e.tenantID, e.actor, c.ID, validReason)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Update(ctx, e.tenantID, e.actor, c.ID, fullIndividual(), res.Customer.Version)
	assertAppErr(t, err, apperr.KindConflict, "customer_archived")
	_, err = e.svc.AddContact(ctx, e.tenantID, e.actor, c.ID, ContactInput{Name: "Ana", Email: "ana@example.com"})
	assertAppErr(t, err, apperr.KindConflict, "customer_anonymized")
	_, err = e.svc.SetAddress(ctx, e.tenantID, e.actor, c.ID, Address{Street: "Rua A", Number: "1", District: "Centro", City: "Recife", State: "PE",
		PostalCode: "50000000"})
	assertAppErr(t, err, apperr.KindConflict, "customer_anonymized")
	st := e.st()
	if len(st.contacts[c.ID]) != 0 || st.customers[c.ID] != res.Customer {
		t.Fatal("o cliente anonimizado foi alterado")
	}
	if _, ok := st.addresses[c.ID]; ok {
		t.Fatal("endereço gravado em cliente anonimizado")
	}
	// Cliente apenas arquivado continua aceitando contato e endereço, como antes.
	archived := e.seed(t, companyInput())
	if _, err := e.svc.Archive(ctx, e.tenantID, e.actor, archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AddContact(ctx, e.tenantID, e.actor, archived.ID, ContactInput{Name: "Financeiro", Email: "fin@acme.com"}); err != nil {
		t.Fatalf("contato em cliente arquivado: %v", err)
	}
}

// A justificativa aceita nunca traz os dados do cliente, respeita os limites,
// não tem caracteres de controle e já está normalizada.
func FuzzValidateAnonymizationReason(f *testing.F) {
	for _, seed := range []string{validReason, "", "curta", "Titular do CPF 529.982.247-25 pediu a eliminação", "Pedido de ANA SOUZA, protocolo 9",
		"Pedido recebido de ana@example.com", "Telefone +55 11 98888-7777, protocolo 1", "Pedido\x00 de eliminação", strings.Repeat("ç", 600),
		"  espaços \t e \n quebras  no  pedido  "} {
		f.Add(seed)
	}
	c, err := New(uuid.New(), uuid.New(), fullIndividual(), testNow)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, reason string) {
		got, err := ValidateAnonymizationReason(reason, c)
		if err != nil {
			if got != "" {
				t.Fatalf("justificativa devolvida junto com erro: %q", got)
			}
			if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Code != "invalid_anonymization" {
				t.Fatalf("erro inesperado: %v", err)
			}
			return
		}
		if n := len([]rune(got)); n < MinAnonymizationReason || n > MaxAnonymizationReason {
			t.Fatalf("tamanho fora dos limites: %d", n)
		}
		if !utf8.ValidString(got) || strings.ContainsFunc(got, unicode.IsControl) {
			t.Fatalf("caracteres inválidos aceitos: %q", got)
		}
		lower := strings.ToLower(got)
		if strings.Contains(lower, strings.ToLower(c.Name)) || strings.Contains(lower, c.Email) || strings.Contains(got, c.DocumentNumber) {
			t.Fatalf("dados pessoais aceitos na justificativa: %q", got)
		}
		if again, err := ValidateAnonymizationReason(got, c); err != nil || again != got {
			t.Fatalf("a justificativa aceita deveria ser estável: %q %v", again, err)
		}
	})
}
