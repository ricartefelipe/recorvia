package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/audit"
	"github.com/ricartefelipe/recorvia/internal/billing"
	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/infra/postgres"
	"github.com/ricartefelipe/recorvia/internal/kernel/apperr"
	"github.com/ricartefelipe/recorvia/internal/kernel/money"
	"github.com/ricartefelipe/recorvia/internal/payments"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
)

// Payments implementa payments.Store.
type Payments struct{ s *Store }

// Payments retorna o adaptador de pagamentos.
func (s *Store) Payments() *Payments { return &Payments{s: s} }

type paymentsTx struct{ base }

// InTenant abre a transação do tenant.
func (p *Payments) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(payments.Tx) error) error {
	return p.s.inTenant(ctx, tenantID, func(b base) error { return fn(paymentsTx{b}) })
}

// ResolveWebhookEndpoint vincula o token do endpoint ao tenant e à conta.
func (p *Payments) ResolveWebhookEndpoint(ctx context.Context, provider string, tokenHash []byte) (uuid.UUID, uuid.UUID, error) {
	var row dbgen.ResolveWebhookEndpointRow
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		row, err = q.ResolveWebhookEndpoint(ctx, dbgen.ResolveWebhookEndpointParams{Provider: provider, TokenHash: tokenHash})
		return notFound(err, "webhook_endpoint_not_found", "Endpoint de webhook desconhecido.")
	})
	return row.TenantID, row.AccountID, err
}

// TenantsWithPendingOperations descobre tenants com operações a retomar.
func (p *Payments) TenantsWithPendingOperations(ctx context.Context, now time.Time) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := p.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsWithPendingOperations(ctx, now)
		return err
	})
	return out, err
}

func (tx paymentsTx) Location(ctx context.Context) (*time.Location, error) {
	return tx.TenantLocation(ctx)
}

func (tx paymentsTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

// aad vincula o ciphertext ao tenant e à finalidade.
func aad(tenantID uuid.UUID, provider, purpose string) []byte {
	return []byte(tenantID.String() + "/" + provider + "/" + purpose)
}

func (tx paymentsTx) accountFromRow(r dbgen.PaymentProviderAccount) (payments.ProviderAccount, error) {
	a := payments.ProviderAccount{ID: r.ID, TenantID: r.TenantID, Provider: r.Provider, Environment: r.Environment, Status: r.Status,
		IsDefault: r.IsDefault, APIKeyHint: deref(r.ApiKeyHint), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}
	kr := tx.store.keyring
	if r.ApiKeyCiphertext != nil && r.ApiKeyKeyID != nil {
		pt, err := kr.Decrypt(r.ApiKeyCiphertext, *r.ApiKeyKeyID, aad(r.TenantID, r.Provider, "api_key"))
		if err != nil {
			return payments.ProviderAccount{}, err
		}
		a.APIKey = string(pt)
	}
	if r.WebhookSecretCiphertext != nil && r.WebhookSecretKeyID != nil {
		pt, err := kr.Decrypt(r.WebhookSecretCiphertext, *r.WebhookSecretKeyID, aad(r.TenantID, r.Provider, "webhook_secret"))
		if err != nil {
			return payments.ProviderAccount{}, err
		}
		a.Secret = string(pt)
	}
	return a, nil
}

func (tx paymentsTx) DefaultAccount(ctx context.Context) (payments.ProviderAccount, error) {
	r, err := tx.q.GetDefaultProviderAccount(ctx, tx.tenantID)
	if err != nil {
		return payments.ProviderAccount{}, notFound(err, "provider_not_configured", "Nenhum provedor de pagamento padrão configurado.")
	}
	return tx.accountFromRow(r)
}

func (tx paymentsTx) Account(ctx context.Context, id uuid.UUID) (payments.ProviderAccount, error) {
	r, err := tx.q.GetProviderAccount(ctx, dbgen.GetProviderAccountParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return payments.ProviderAccount{}, notFound(err, "provider_account_not_found", "Conta do provedor não encontrada.")
	}
	return tx.accountFromRow(r)
}

func (tx paymentsTx) AccountByProvider(ctx context.Context, provider string) (payments.ProviderAccount, error) {
	r, err := tx.q.GetProviderAccountByProvider(ctx, dbgen.GetProviderAccountByProviderParams{TenantID: tx.tenantID, Provider: provider})
	if err != nil {
		return payments.ProviderAccount{}, notFound(err, "provider_account_not_found", "Conta do provedor não encontrada.")
	}
	return tx.accountFromRow(r)
}

func (tx paymentsTx) SaveAccount(ctx context.Context, a payments.ProviderAccount, endpointTokenHash []byte) error {
	kr := tx.store.keyring
	keyCT, keyID, err := kr.Encrypt([]byte(a.APIKey), aad(tx.tenantID, a.Provider, "api_key"))
	if err != nil {
		return err
	}
	secCT, secID, err := kr.Encrypt([]byte(a.Secret), aad(tx.tenantID, a.Provider, "webhook_secret"))
	if err != nil {
		return err
	}
	if a.IsDefault {
		if err := tx.q.ClearDefaultProviderAccount(ctx, dbgen.ClearDefaultProviderAccountParams{TenantID: tx.tenantID, KeepID: a.ID}); err != nil {
			return err
		}
	}
	return tx.q.UpsertProviderAccount(ctx, dbgen.UpsertProviderAccountParams{ID: a.ID, TenantID: tx.tenantID, Provider: a.Provider,
		Environment: a.Environment, IsDefault: a.IsDefault, ApiKeyCiphertext: keyCT, ApiKeyKeyID: &keyID, ApiKeyHint: strPtr(a.APIKeyHint),
		WebhookSecretCiphertext: secCT, WebhookSecretKeyID: &secID, WebhookEndpointTokenHash: endpointTokenHash, CreatedAt: a.CreatedAt})
}

func (tx paymentsTx) Accounts(ctx context.Context) ([]payments.ProviderAccount, error) {
	rows, err := tx.q.ListProviderAccounts(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]payments.ProviderAccount, 0, len(rows))
	for _, r := range rows {
		out = append(out, payments.ProviderAccount{ID: r.ID, TenantID: r.TenantID, Provider: r.Provider, Environment: r.Environment,
			Status: r.Status, IsDefault: r.IsDefault, APIKeyHint: deref(r.ApiKeyHint), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Version: int(r.Version)})
	}
	return out, nil
}

func (tx paymentsTx) Invoice(ctx context.Context, id uuid.UUID, forUpdate bool) (billing.Invoice, error) {
	return tx.invoice(ctx, id, forUpdate)
}

func (tx paymentsTx) UpdateInvoiceProgress(ctx context.Context, inv *billing.Invoice) error {
	return tx.updateInvoiceProgress(ctx, inv)
}

func (tx paymentsTx) InsertInvoiceHistory(ctx context.Context, invoiceID uuid.UUID, actor audit.Actor, changes []billing.StatusChange) error {
	return tx.insertInvoiceHistory(ctx, invoiceID, actor, changes)
}

func (tx paymentsTx) RefreshSubscriptionStanding(ctx context.Context, subscriptionID uuid.UUID, actor audit.Actor, now time.Time) error {
	return tx.refreshStanding(ctx, subscriptionID, actor, now)
}

func (tx paymentsTx) CustomerData(ctx context.Context, customerID uuid.UUID, provider string) (payments.CustomerData, error) {
	c, err := tx.customer(ctx, customerID)
	if err != nil {
		return payments.CustomerData{}, err
	}
	d := payments.CustomerData{ID: c.ID, Name: c.Name, DocumentNumber: c.DocumentNumber, Email: c.Email}
	ref, err := tx.q.GetProviderCustomerRef(ctx, dbgen.GetProviderCustomerRefParams{TenantID: tx.tenantID, CustomerID: customerID, Provider: provider})
	if err != nil && !postgres.IsNoRows(err) {
		return payments.CustomerData{}, err
	}
	d.ProviderCustomerID = ref
	return d, nil
}

func (tx paymentsTx) SaveProviderCustomer(ctx context.Context, customerID uuid.UUID, provider, providerCustomerID string) error {
	return tx.q.InsertProviderCustomerRef(ctx, dbgen.InsertProviderCustomerRefParams{TenantID: tx.tenantID, CustomerID: customerID, Provider: provider,
		ProviderCustomerID: providerCustomerID})
}

func chargeFromRow(r dbgen.Charge) (payments.Charge, error) {
	cur, err := money.ParseCurrency(r.Currency)
	if err != nil {
		return payments.Charge{}, err
	}
	m := func(v int64) money.Money { return money.New(v, cur) }
	return payments.Charge{ID: r.ID, TenantID: r.TenantID, InvoiceID: r.InvoiceID, CustomerID: r.CustomerID, ProviderAccountID: r.ProviderAccountID,
		Provider: r.Provider, Attempt: int(r.Attempt), ProviderChargeID: deref(r.ProviderChargeID), Status: payments.ChargeStatus(r.Status),
		Amount: m(r.Amount), AmountPaid: m(r.AmountPaid), Fee: m(r.FeeAmount), AmountRefunded: m(r.AmountRefunded),
		AmountChargedBack: m(r.AmountChargedBack), CheckoutURL: deref(r.CheckoutUrl), DueDate: dateFromPg(r.DueDate), PaidAt: r.PaidAt,
		FailureReason: deref(r.FailureReason), ProviderStatus: deref(r.ProviderStatus), ProviderStatusAt: r.ProviderStatusAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version)}, nil
}

func (tx paymentsTx) LatestCharge(ctx context.Context, invoiceID uuid.UUID) (*payments.Charge, error) {
	r, err := tx.q.LatestCharge(ctx, dbgen.LatestChargeParams{TenantID: tx.tenantID, InvoiceID: invoiceID})
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := chargeFromRow(r)
	return &c, err
}

func (tx paymentsTx) InsertCharge(ctx context.Context, c payments.Charge) error {
	err := tx.q.InsertCharge(ctx, dbgen.InsertChargeParams{ID: c.ID, TenantID: tx.tenantID, InvoiceID: c.InvoiceID, CustomerID: c.CustomerID,
		ProviderAccountID: c.ProviderAccountID, Provider: c.Provider, Attempt: int32(c.Attempt), ProviderChargeID: strPtr(c.ProviderChargeID),
		Status: string(c.Status), Amount: c.Amount.Amount(), Currency: c.Amount.Currency().Code(), AmountPaid: c.AmountPaid.Amount(),
		FeeAmount: c.Fee.Amount(), AmountRefunded: c.AmountRefunded.Amount(), AmountChargedBack: c.AmountChargedBack.Amount(),
		CheckoutUrl: strPtr(c.CheckoutURL), DueDate: dateToPg(c.DueDate), PaidAt: c.PaidAt, FailureReason: strPtr(c.FailureReason),
		ProviderStatus: strPtr(c.ProviderStatus), ProviderStatusAt: c.ProviderStatusAt, CreatedAt: c.CreatedAt})
	if postgres.IsUniqueViolation(err, "charges_invoice_attempt_key") {
		return apperr.Conflict("charge_attempt_exists", "Outra tentativa de cobrança foi criada ao mesmo tempo.")
	}
	return err
}

func (tx paymentsTx) Charge(ctx context.Context, id uuid.UUID, forUpdate bool) (payments.Charge, error) {
	var r dbgen.Charge
	var err error
	if forUpdate {
		r, err = tx.q.GetChargeForUpdate(ctx, dbgen.GetChargeForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetCharge(ctx, dbgen.GetChargeParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return payments.Charge{}, notFound(err, "charge_not_found", "Cobrança não encontrada.")
	}
	return chargeFromRow(r)
}

func (tx paymentsTx) ChargeByProviderID(ctx context.Context, provider, providerChargeID string, forUpdate bool) (payments.Charge, error) {
	var r dbgen.Charge
	var err error
	pid := providerChargeID
	if forUpdate {
		r, err = tx.q.GetChargeByProviderIDForUpdate(ctx, dbgen.GetChargeByProviderIDForUpdateParams{TenantID: tx.tenantID, Provider: provider, ProviderChargeID: &pid})
	} else {
		r, err = tx.q.GetChargeByProviderID(ctx, dbgen.GetChargeByProviderIDParams{TenantID: tx.tenantID, Provider: provider, ProviderChargeID: &pid})
	}
	if err != nil {
		return payments.Charge{}, notFound(err, "charge_not_found", "Cobrança não encontrada.")
	}
	return chargeFromRow(r)
}

func (tx paymentsTx) UpdateCharge(ctx context.Context, c *payments.Charge) error {
	n, err := tx.q.UpdateCharge(ctx, dbgen.UpdateChargeParams{TenantID: tx.tenantID, ID: c.ID, ProviderChargeID: strPtr(c.ProviderChargeID),
		Status: string(c.Status), AmountPaid: c.AmountPaid.Amount(), FeeAmount: c.Fee.Amount(), AmountRefunded: c.AmountRefunded.Amount(),
		AmountChargedBack: c.AmountChargedBack.Amount(), CheckoutUrl: strPtr(c.CheckoutURL), PaidAt: c.PaidAt, FailureReason: strPtr(c.FailureReason),
		ProviderStatus: strPtr(c.ProviderStatus), ProviderStatusAt: c.ProviderStatusAt, Version: int32(c.Version)})
	if err != nil {
		if postgres.IsUniqueViolation(err, "charges_provider_charge_key") {
			return apperr.Conflict("provider_charge_linked", "A cobrança do provedor já está vinculada a outra cobrança.")
		}
		return err
	}
	if err := versionConflict(n, "A cobrança foi alterada concorrentemente."); err != nil {
		return err
	}
	c.Version++
	return nil
}

func (tx paymentsTx) OpenChargesForInvoice(ctx context.Context, invoiceID uuid.UUID) ([]payments.Charge, error) {
	rows, err := tx.q.OpenChargesForInvoice(ctx, dbgen.OpenChargesForInvoiceParams{TenantID: tx.tenantID, InvoiceID: invoiceID})
	if err != nil {
		return nil, err
	}
	out := make([]payments.Charge, 0, len(rows))
	for _, r := range rows {
		c, err := chargeFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func operationFromRow(r dbgen.PaymentOperation) (payments.Operation, error) {
	resp, err := jsonMap(r.ProviderResponse)
	if err != nil {
		return payments.Operation{}, err
	}
	return payments.Operation{ID: r.ID, TenantID: r.TenantID, ChargeID: r.ChargeID, RefundID: r.RefundID, Kind: payments.OperationKind(r.Kind),
		Status: payments.OperationStatus(r.Status), ExternalReference: r.ExternalReference, Attempts: int(r.Attempts), LeaseUntil: r.LeaseUntil,
		NextAttemptAt: r.NextAttemptAt, LastError: deref(r.LastError), ProviderResponse: resp, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		CompletedAt: r.CompletedAt}, nil
}

func (tx paymentsTx) InsertOperation(ctx context.Context, op payments.Operation) error {
	return tx.q.InsertOperation(ctx, dbgen.InsertOperationParams{ID: op.ID, TenantID: tx.tenantID, ChargeID: op.ChargeID, RefundID: op.RefundID,
		Kind: string(op.Kind), Status: string(op.Status), ExternalReference: op.ExternalReference, NextAttemptAt: op.NextAttemptAt, CreatedAt: op.CreatedAt})
}

func (tx paymentsTx) Operation(ctx context.Context, id uuid.UUID, forUpdate bool) (payments.Operation, error) {
	var r dbgen.PaymentOperation
	var err error
	if forUpdate {
		r, err = tx.q.GetOperationForUpdate(ctx, dbgen.GetOperationForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetOperation(ctx, dbgen.GetOperationParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return payments.Operation{}, notFound(err, "operation_not_found", "Operação não encontrada.")
	}
	return operationFromRow(r)
}

func (tx paymentsTx) UpdateOperation(ctx context.Context, op payments.Operation) error {
	var resp []byte
	if op.ProviderResponse != nil {
		b, err := json.Marshal(op.ProviderResponse)
		if err != nil {
			return err
		}
		resp = b
	}
	return tx.q.UpdateOperation(ctx, dbgen.UpdateOperationParams{TenantID: tx.tenantID, ID: op.ID, Status: string(op.Status), Attempts: int32(op.Attempts),
		LeaseUntil: op.LeaseUntil, NextAttemptAt: op.NextAttemptAt, LastError: strPtr(op.LastError), ProviderResponse: resp, CompletedAt: op.CompletedAt})
}

func (tx paymentsTx) DueOperationIDs(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	return tx.q.DueOperationIDs(ctx, dbgen.DueOperationIDsParams{TenantID: tx.tenantID, Now: now, MaxAttempts: payments.MaxOperationAttempts,
		RowLimit: int32(limit)})
}

func (tx paymentsTx) InsertPayment(ctx context.Context, p payments.Payment) (bool, error) {
	n, err := tx.q.InsertPayment(ctx, dbgen.InsertPaymentParams{ID: p.ID, TenantID: tx.tenantID, InvoiceID: p.InvoiceID, ChargeID: p.ChargeID,
		CustomerID: p.CustomerID, Amount: p.Amount.Amount(), FeeAmount: p.Fee.Amount(), Currency: p.Amount.Currency().Code(), Method: p.Method,
		Source: p.Source, PaidAt: p.PaidAt, Note: p.Note, RecordedBy: p.RecordedBy, RequestKey: strPtr(p.RequestKey), CreatedAt: p.CreatedAt})
	return n == 1, err
}

func paymentFromRow(r dbgen.Payment) payments.Payment {
	return payments.Payment{ID: r.ID, TenantID: r.TenantID, InvoiceID: r.InvoiceID, ChargeID: r.ChargeID, CustomerID: r.CustomerID,
		Amount: brl(r.Amount), Fee: brl(r.FeeAmount), Method: r.Method, Source: r.Source, PaidAt: r.PaidAt, Note: r.Note,
		RecordedBy: r.RecordedBy, RequestKey: deref(r.RequestKey), CreatedAt: r.CreatedAt}
}

func (tx paymentsTx) PaymentByRequestKey(ctx context.Context, key string) (payments.Payment, error) {
	r, err := tx.q.GetPaymentByRequestKey(ctx, dbgen.GetPaymentByRequestKeyParams{TenantID: tx.tenantID, RequestKey: &key})
	if err != nil {
		return payments.Payment{}, notFound(err, "payment_not_found", "Pagamento não encontrado.")
	}
	return paymentFromRow(r), nil
}

func (tx paymentsTx) GrantCustomerCredit(ctx context.Context, customerID uuid.UUID, amount money.Money, kind string, invoiceID *uuid.UUID, reason string, _ time.Time) error {
	return tx.addCredit(ctx, customerID, amount.Amount(), kind, invoiceID, reason, nil)
}

func (tx paymentsTx) InsertWebhook(ctx context.Context, w payments.StoredWebhook) (bool, error) {
	n, err := tx.q.InsertWebhookEvent(ctx, dbgen.InsertWebhookEventParams{ID: w.ID, TenantID: tx.tenantID, ProviderAccountID: w.ProviderAccountID,
		Provider: w.Provider, ProviderEventID: w.EventID, EventType: w.EventType, ProviderChargeID: strPtr(w.ProviderChargeID),
		OccurredAt: w.OccurredAt, ReceivedAt: w.ReceivedAt, Payload: w.Payload})
	return n == 1, err
}

func (tx paymentsTx) Webhook(ctx context.Context, id uuid.UUID, _ bool) (payments.StoredWebhook, error) {
	r, err := tx.q.GetWebhookEvent(ctx, dbgen.GetWebhookEventParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return payments.StoredWebhook{}, notFound(err, "webhook_event_not_found", "Evento de webhook não encontrado.")
	}
	return payments.StoredWebhook{ID: r.ID, TenantID: r.TenantID, ProviderAccountID: r.ProviderAccountID, Provider: r.Provider,
		EventID: r.ProviderEventID, EventType: r.EventType, ProviderChargeID: deref(r.ProviderChargeID), OccurredAt: r.OccurredAt,
		ReceivedAt: r.ReceivedAt, Payload: r.Payload, Status: r.Status, Error: deref(r.Error)}, nil
}

func (tx paymentsTx) UpdateWebhookStatus(ctx context.Context, id uuid.UUID, status, errText string, at time.Time) error {
	return tx.q.UpdateWebhookEventStatus(ctx, dbgen.UpdateWebhookEventStatusParams{TenantID: tx.tenantID, ID: id, Status: status,
		Error: strPtr(errText), ProcessedAt: &at})
}
