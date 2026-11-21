import { api } from '../../api/client';
import type {
  ApiCredential,
  ApiCredentialWithSecret,
  CursorPage,
  DataList,
  ProviderAccount,
  ProviderAccountConfigured,
  ProviderAccountInput,
  WebhookDelivery,
  WebhookDeliveryAttempt,
  WebhookEndpoint,
  WebhookEndpointInput,
  WebhookEndpointWithSecret,
} from '../../api/types';

const enc = encodeURIComponent;

export type ProviderCode = 'simulator' | 'asaas';

export const integrationsApi = {
  providers: (signal?: AbortSignal) =>
    api.get<DataList<ProviderAccount> & { available: string[] }>('/api/v1/payment-providers', { signal }),
  /** If-Match é a versão da conta do provedor; 0 para o provedor ainda não configurado. */
  configureProvider: (provider: ProviderCode, version: number, input: ProviderAccountInput) =>
    api.put<ProviderAccountConfigured>(`/api/v1/payment-providers/${enc(provider)}`, input, { ifMatch: version }),

  credentials: (params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<ApiCredential>>('/api/v1/api-credentials', { query: { ...params }, signal }),
  createCredential: (input: { name: string; scopes: string[]; expires_in_days?: number }) =>
    api.post<ApiCredentialWithSecret>('/api/v1/api-credentials', input),
  rotateCredential: (credential: ApiCredential) =>
    api.post<ApiCredentialWithSecret>(`/api/v1/api-credentials/${enc(credential.id)}/rotate`, undefined, { ifMatch: credential.version }),
  revokeCredential: (credential: ApiCredential) =>
    api.delete(`/api/v1/api-credentials/${enc(credential.id)}`, { ifMatch: credential.version }),

  eventTypes: (signal?: AbortSignal) => api.get<DataList<string>>('/api/v1/webhook-event-types', { signal }),
  endpoints: (signal?: AbortSignal) => api.get<DataList<WebhookEndpoint>>('/api/v1/webhook-endpoints', { signal }),
  createEndpoint: (input: WebhookEndpointInput) => api.post<WebhookEndpointWithSecret>('/api/v1/webhook-endpoints', input),
  updateEndpoint: (endpoint: WebhookEndpoint, input: WebhookEndpointInput & { status: 'active' | 'disabled' }) =>
    api.patch<WebhookEndpoint>(`/api/v1/webhook-endpoints/${enc(endpoint.id)}`, input, { ifMatch: endpoint.version }),
  deleteEndpoint: (endpoint: WebhookEndpoint) =>
    api.delete(`/api/v1/webhook-endpoints/${enc(endpoint.id)}`, { ifMatch: endpoint.version }),
  /** Os segredos fazem parte do endpoint: a rotação exige a versão do endpoint. */
  rotateSecret: (endpoint: WebhookEndpoint, previousSecretValidHours: number) =>
    api.post<WebhookEndpointWithSecret>(
      `/api/v1/webhook-endpoints/${enc(endpoint.id)}/rotate-secret`,
      { previous_secret_valid_hours: previousSecretValidHours },
      { ifMatch: endpoint.version },
    ),
  deliveries: (id: string, params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<WebhookDelivery>>(`/api/v1/webhook-endpoints/${enc(id)}/deliveries`, { query: { ...params }, signal }),
  deliveryAttempts: (id: string, signal?: AbortSignal) =>
    api.get<DataList<WebhookDeliveryAttempt>>(`/api/v1/webhook-deliveries/${enc(id)}/attempts`, { signal }),
  retryDelivery: (id: string) => api.post<WebhookDelivery>(`/api/v1/webhook-deliveries/${enc(id)}/retry`),
};

/** Segredo aleatório (base64url, 43 caracteres) gerado no navegador. */
export function generateSecret(bytes = 32): string {
  const data = new Uint8Array(bytes);
  crypto.getRandomValues(data);
  let binary = '';
  data.forEach((b) => (binary += String.fromCharCode(b)));
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
