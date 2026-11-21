package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ricartefelipe/recorvia/internal/dbgen"
	"github.com/ricartefelipe/recorvia/internal/platformbilling"
	"github.com/ricartefelipe/recorvia/internal/webhooks"
)

// Webhooks implementa webhooks.Store.
type Webhooks struct{ s *Store }

// Webhooks retorna o adaptador dos webhooks enviados.
func (s *Store) Webhooks() *Webhooks { return &Webhooks{s: s} }

type webhooksTx struct{ base }

// InTenant abre a transação do tenant.
func (a *Webhooks) InTenant(ctx context.Context, tenantID uuid.UUID, fn func(webhooks.Tx) error) error {
	return a.s.inTenant(ctx, tenantID, func(b base) error { return fn(webhooksTx{b}) })
}

// TenantsWithDueDeliveries descobre tenants com entregas a tentar.
func (a *Webhooks) TenantsWithDueDeliveries(ctx context.Context, now time.Time, after *uuid.UUID, limit int) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := a.s.inGlobal(ctx, func(q *dbgen.Queries) error {
		var err error
		out, err = q.TenantsWithDueWebhookDeliveries(ctx, dbgen.TenantsWithDueWebhookDeliveriesParams{Now: now, AfterTenant: after,
			MaxTenants: int32(limit)})
		return err
	})
	return out, err
}

// Attempts lista as tentativas de uma entrega (consulta e testes).
func (a *Webhooks) Attempts(ctx context.Context, tenantID, deliveryID uuid.UUID) ([]webhooks.Attempt, error) {
	var out []webhooks.Attempt
	err := a.s.inTenant(ctx, tenantID, func(b base) error {
		var err error
		out, err = webhooksTx{b}.Attempts(ctx, deliveryID)
		return err
	})
	return out, err
}

// Attempts lista as tentativas de uma entrega em ordem crescente.
func (tx webhooksTx) Attempts(ctx context.Context, deliveryID uuid.UUID) ([]webhooks.Attempt, error) {
	rows, err := tx.q.ListWebhookDeliveryAttempts(ctx, dbgen.ListWebhookDeliveryAttemptsParams{TenantID: tx.tenantID, DeliveryID: deliveryID})
	if err != nil {
		return nil, err
	}
	out := make([]webhooks.Attempt, 0, len(rows))
	for _, r := range rows {
		at := webhooks.Attempt{ID: r.ID, DeliveryID: r.DeliveryID, Number: int(r.Attempt), StartedAt: r.StartedAt,
			Duration: time.Duration(r.DurationMs) * time.Millisecond, Error: deref(r.Error), SignatureCount: int(r.SignatureCount)}
		if r.StatusCode != nil {
			c := int(*r.StatusCode)
			at.StatusCode = &c
		}
		out = append(out, at)
	}
	return out, nil
}

func (tx webhooksTx) PlatformPlan(ctx context.Context) (platformbilling.Plan, error) {
	return tx.platformPlan(ctx)
}

func secretAAD(tenantID, endpointID, secretID uuid.UUID) []byte {
	return []byte(tenantID.String() + "/webhook_endpoint/" + endpointID.String() + "/secret/" + secretID.String())
}

func endpointFromRow(r dbgen.WebhookEndpoint) webhooks.Endpoint {
	return webhooks.Endpoint{ID: r.ID, TenantID: r.TenantID, URL: r.Url, Description: r.Description, EventTypes: r.EventTypes,
		Status: webhooks.Status(r.Status), DeletedAt: r.DeletedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Version: int(r.Version),
		Secrets: []webhooks.SecretInfo{}}
}

func (tx webhooksTx) withSecrets(ctx context.Context, eps []webhooks.Endpoint, now time.Time) error {
	if len(eps) == 0 {
		return nil
	}
	idx := make(map[uuid.UUID]int, len(eps))
	ids := make([]uuid.UUID, len(eps))
	for i, e := range eps {
		idx[e.ID], ids[i] = i, e.ID
	}
	rows, err := tx.q.ListValidWebhookSecretInfos(ctx, dbgen.ListValidWebhookSecretInfosParams{TenantID: tx.tenantID, EndpointIds: ids, Now: &now})
	if err != nil {
		return err
	}
	for _, r := range rows {
		i := idx[r.EndpointID]
		eps[i].Secrets = append(eps[i].Secrets, webhooks.SecretInfo{ID: r.ID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt})
	}
	return nil
}

func (tx webhooksTx) Endpoints(ctx context.Context, now time.Time) ([]webhooks.Endpoint, error) {
	rows, err := tx.q.ListWebhookEndpoints(ctx, tx.tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]webhooks.Endpoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, endpointFromRow(r))
	}
	return out, tx.withSecrets(ctx, out, now)
}

func (tx webhooksTx) Endpoint(ctx context.Context, id uuid.UUID, forUpdate bool, now time.Time) (webhooks.Endpoint, error) {
	var r dbgen.WebhookEndpoint
	var err error
	if forUpdate {
		r, err = tx.q.GetWebhookEndpointForUpdate(ctx, dbgen.GetWebhookEndpointForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetWebhookEndpoint(ctx, dbgen.GetWebhookEndpointParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return webhooks.Endpoint{}, notFound(err, "webhook_endpoint_not_found", "Endpoint de webhook não encontrado.")
	}
	eps := []webhooks.Endpoint{endpointFromRow(r)}
	if err := tx.withSecrets(ctx, eps, now); err != nil {
		return webhooks.Endpoint{}, err
	}
	return eps[0], nil
}

func (tx webhooksTx) LockEndpoints(ctx context.Context) error {
	return tx.q.LockWebhookEndpoints(ctx, "webhook_endpoints:"+tx.tenantID.String())
}

func (tx webhooksTx) CountEndpoints(ctx context.Context) (int, error) {
	n, err := tx.q.CountWebhookEndpoints(ctx, tx.tenantID)
	return int(n), err
}

func (tx webhooksTx) InsertEndpoint(ctx context.Context, e webhooks.Endpoint, createdBy *uuid.UUID) error {
	return tx.q.InsertWebhookEndpoint(ctx, dbgen.InsertWebhookEndpointParams{ID: e.ID, TenantID: tx.tenantID, Url: e.URL, Description: e.Description,
		EventTypes: e.EventTypes, Status: string(e.Status), CreatedBy: createdBy, CreatedAt: e.CreatedAt})
}

func (tx webhooksTx) UpdateEndpoint(ctx context.Context, e webhooks.Endpoint, expectedVersion int) error {
	n, err := tx.q.UpdateWebhookEndpoint(ctx, dbgen.UpdateWebhookEndpointParams{Url: e.URL, Description: e.Description, EventTypes: e.EventTypes,
		Status: string(e.Status), DeletedAt: e.DeletedAt, UpdatedAt: e.UpdatedAt, TenantID: tx.tenantID, ID: e.ID, Version: int32(expectedVersion)})
	if err != nil {
		return err
	}
	return versionConflict(n, "O endpoint foi alterado por outra pessoa.")
}

func (tx webhooksTx) InsertSecret(ctx context.Context, endpointID uuid.UUID, s webhooks.Secret) error {
	ct, keyID, err := tx.store.keyring.Encrypt([]byte(s.Value), secretAAD(tx.tenantID, endpointID, s.ID))
	if err != nil {
		return err
	}
	return tx.q.InsertWebhookSecret(ctx, dbgen.InsertWebhookSecretParams{ID: s.ID, TenantID: tx.tenantID, EndpointID: endpointID,
		SecretCiphertext: ct, SecretKeyID: keyID, CreatedAt: s.CreatedAt})
}

func (tx webhooksTx) ExpireSecrets(ctx context.Context, endpointID uuid.UUID, at time.Time) error {
	return tx.q.ExpireWebhookSecrets(ctx, dbgen.ExpireWebhookSecretsParams{ExpiresAt: &at, TenantID: tx.tenantID, EndpointID: endpointID})
}

func (tx webhooksTx) RetainSecrets(ctx context.Context, endpointID uuid.UUID, keep int, now time.Time) error {
	return tx.q.RetainWebhookSecrets(ctx, dbgen.RetainWebhookSecretsParams{Now: now, TenantID: tx.tenantID, EndpointID: endpointID, Keep: int32(keep)})
}

// Secrets decifra os segredos válidos. Um segredo que não decifra (chave
// retirada do keyring, AAD divergente ou dado corrompido) vem marcado como
// indisponível, sem interromper a leitura dos demais.
func (tx webhooksTx) Secrets(ctx context.Context, endpointID uuid.UUID, now time.Time) ([]webhooks.Secret, error) {
	rows, err := tx.q.ListValidWebhookSecrets(ctx, dbgen.ListValidWebhookSecretsParams{TenantID: tx.tenantID, EndpointID: endpointID, Now: &now})
	if err != nil {
		return nil, err
	}
	out := make([]webhooks.Secret, 0, len(rows))
	for _, r := range rows {
		sec := webhooks.Secret{SecretInfo: webhooks.SecretInfo{ID: r.ID, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}}
		if pt, err := tx.store.keyring.Decrypt(r.SecretCiphertext, r.SecretKeyID, secretAAD(r.TenantID, r.EndpointID, r.ID)); err != nil {
			sec.Unavailable = true
		} else {
			sec.Value = string(pt)
		}
		out = append(out, sec)
	}
	return out, nil
}

func (tx webhooksTx) SubscribedEndpoints(ctx context.Context, eventType string) ([]uuid.UUID, error) {
	return tx.q.SubscribedWebhookEndpoints(ctx, dbgen.SubscribedWebhookEndpointsParams{TenantID: tx.tenantID, EventType: eventType})
}

func (tx webhooksTx) InsertEvent(ctx context.Context, e webhooks.Event) (bool, error) {
	n, err := tx.q.InsertOutboundWebhookEvent(ctx, dbgen.InsertOutboundWebhookEventParams{ID: e.ID, TenantID: tx.tenantID, EventType: e.Type,
		Payload: e.Payload, OccurredAt: e.OccurredAt, CreatedAt: tx.store.clock.Now()})
	return n == 1, err
}

func (tx webhooksTx) Event(ctx context.Context, id uuid.UUID) (webhooks.Event, error) {
	r, err := tx.q.GetOutboundWebhookEvent(ctx, dbgen.GetOutboundWebhookEventParams{TenantID: tx.tenantID, ID: id})
	if err != nil {
		return webhooks.Event{}, notFound(err, "webhook_event_not_found", "Evento não encontrado.")
	}
	return webhooks.Event{ID: r.ID, Type: r.EventType, Payload: r.Payload, OccurredAt: r.OccurredAt}, nil
}

func deliveryFromRow(r dbgen.WebhookDelivery) webhooks.Delivery {
	d := webhooks.Delivery{ID: r.ID, TenantID: r.TenantID, EndpointID: r.EndpointID, EventID: r.EventID, EventType: r.EventType,
		Status: webhooks.DeliveryStatus(r.Status), Attempts: int(r.Attempts), MaxAttempts: int(r.MaxAttempts), NextAttemptAt: r.NextAttemptAt,
		LeaseUntil: r.LeaseUntil, LastError: deref(r.LastError), DeliveredAt: r.DeliveredAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.LastStatusCode != nil {
		c := int(*r.LastStatusCode)
		d.LastStatusCode = &c
	}
	return d
}

func (tx webhooksTx) InsertDelivery(ctx context.Context, d webhooks.Delivery) (bool, error) {
	n, err := tx.q.InsertWebhookDelivery(ctx, dbgen.InsertWebhookDeliveryParams{ID: d.ID, TenantID: tx.tenantID, EndpointID: d.EndpointID,
		EventID: d.EventID, EventType: d.EventType, Status: string(d.Status), MaxAttempts: int32(d.MaxAttempts), NextAttemptAt: d.NextAttemptAt,
		CreatedAt: d.CreatedAt})
	return n == 1, err
}

func (tx webhooksTx) Deliveries(ctx context.Context, endpointID uuid.UUID, limit int, cursor string) (webhooks.DeliveryPage, error) {
	cts, cid, err := parseTimeCursor(cursor)
	if err != nil {
		return webhooks.DeliveryPage{}, err
	}
	limit = pageLimit(limit)
	rows, err := tx.q.ListWebhookDeliveries(ctx, dbgen.ListWebhookDeliveriesParams{TenantID: tx.tenantID, EndpointID: endpointID, CursorTs: cts,
		CursorID: cid, RowLimit: int32(limit + 1)})
	if err != nil {
		return webhooks.DeliveryPage{}, err
	}
	var p webhooks.DeliveryPage
	for i, r := range rows {
		if i == limit {
			p.NextCursor = timeCursor(rows[i-1].CreatedAt, rows[i-1].ID)
			break
		}
		p.Items = append(p.Items, deliveryFromRow(r))
	}
	return p, nil
}

func (tx webhooksTx) Delivery(ctx context.Context, id uuid.UUID, forUpdate bool) (webhooks.Delivery, error) {
	var r dbgen.WebhookDelivery
	var err error
	if forUpdate {
		r, err = tx.q.GetWebhookDeliveryForUpdate(ctx, dbgen.GetWebhookDeliveryForUpdateParams{TenantID: tx.tenantID, ID: id})
	} else {
		r, err = tx.q.GetWebhookDelivery(ctx, dbgen.GetWebhookDeliveryParams{TenantID: tx.tenantID, ID: id})
	}
	if err != nil {
		return webhooks.Delivery{}, notFound(err, "webhook_delivery_not_found", "Entrega de webhook não encontrada.")
	}
	return deliveryFromRow(r), nil
}

func (tx webhooksTx) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]webhooks.Delivery, error) {
	rows, err := tx.q.DueWebhookDeliveries(ctx, dbgen.DueWebhookDeliveriesParams{TenantID: tx.tenantID, Now: &now, RowLimit: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := make([]webhooks.Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryFromRow(r))
	}
	return out, nil
}

func (tx webhooksTx) UpdateDelivery(ctx context.Context, d webhooks.Delivery, expectedAttempts int) (bool, error) {
	n, err := tx.q.UpdateWebhookDelivery(ctx, dbgen.UpdateWebhookDeliveryParams{Status: string(d.Status), Attempts: int32(d.Attempts),
		MaxAttempts: int32(d.MaxAttempts), NextAttemptAt: d.NextAttemptAt, LeaseUntil: d.LeaseUntil, LastStatusCode: intPtr32(d.LastStatusCode),
		LastError: strPtr(d.LastError), DeliveredAt: d.DeliveredAt, UpdatedAt: d.UpdatedAt, TenantID: tx.tenantID, ID: d.ID,
		ExpectedAttempts: int32(expectedAttempts)})
	return n == 1, err
}

func (tx webhooksTx) InsertAttempt(ctx context.Context, a webhooks.Attempt) error {
	return tx.q.InsertWebhookDeliveryAttempt(ctx, dbgen.InsertWebhookDeliveryAttemptParams{ID: a.ID, TenantID: tx.tenantID, DeliveryID: a.DeliveryID,
		Attempt: int32(a.Number), StartedAt: a.StartedAt, DurationMs: int32(a.Duration / time.Millisecond), StatusCode: intPtr32(a.StatusCode),
		Error: strPtr(a.Error), SignatureCount: int32(a.SignatureCount)})
}
