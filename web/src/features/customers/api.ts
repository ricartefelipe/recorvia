import { api } from '../../api/client';
import type {
  Address,
  AuditEvent,
  Contact,
  ContactInput,
  CreditBalance,
  CustomerAnonymization,
  CustomerDetails,
  CustomerInput,
  CursorPage,
  CustomerPage,
  PortalLinkPage,
} from '../../api/types';

const enc = encodeURIComponent;

export interface CustomerListParams {
  q?: string;
  status?: 'active' | 'archived';
  sort?: 'name' | '-name' | 'created_at' | '-created_at';
  cursor?: string;
  limit?: number;
}

export const customersApi = {
  list: (params: CustomerListParams, signal?: AbortSignal) =>
    api.get<CustomerPage>('/api/v1/customers', { query: { ...params }, signal }),
  get: (id: string, signal?: AbortSignal) => api.get<CustomerDetails>(`/api/v1/customers/${enc(id)}`, { signal }),
  create: (input: CustomerInput) => api.post<CustomerDetails>('/api/v1/customers', input),
  update: (id: string, version: number, input: CustomerInput) =>
    api.patch<CustomerDetails>(`/api/v1/customers/${enc(id)}`, input, { ifMatch: version }),
  archive: (id: string, version: number) =>
    api.post<CustomerDetails>(`/api/v1/customers/${enc(id)}/archive`, undefined, { ifMatch: version }),
  anonymize: (id: string, input: { reason: string }, idempotencyKey: string) =>
    api.post<CustomerAnonymization>(`/api/v1/customers/${enc(id)}/anonymize`, input, { idempotencyKey }),
  addContact: (id: string, input: ContactInput) => api.post<Contact>(`/api/v1/customers/${enc(id)}/contacts`, input),
  updateContact: (id: string, contactId: string, input: ContactInput) =>
    api.patch<Contact>(`/api/v1/customers/${enc(id)}/contacts/${enc(contactId)}`, input),
  removeContact: (id: string, contactId: string) =>
    api.delete(`/api/v1/customers/${enc(id)}/contacts/${enc(contactId)}`),
  /** O endereço faz parte da ficha: If-Match é a versão do cliente, que a gravação incrementa. */
  setAddress: (id: string, version: number, address: Address) =>
    api.put<Address>(`/api/v1/customers/${enc(id)}/billing-address`, address, { ifMatch: version }),
  history: (id: string, params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CursorPage<AuditEvent>>(`/api/v1/customers/${enc(id)}/history`, { query: { ...params }, signal }),
  /** Saldo e uma página dos movimentos de crédito (next_cursor pede a seguinte). */
  credits: (id: string, params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<CreditBalance>(`/api/v1/customers/${enc(id)}/credits`, { query: { ...params }, signal }),
  grantCredit: (id: string, input: { amount: number; reason: string }, idempotencyKey: string) =>
    api.post<CreditBalance>(`/api/v1/customers/${enc(id)}/credits`, input, { idempotencyKey }),
  createPortalLink: (id: string) =>
    api.post<{ id: string; url: string; expires_at: string }>(`/api/v1/customers/${enc(id)}/portal-links`),
  portalLinks: (id: string, params: { cursor?: string; limit?: number }, signal?: AbortSignal) =>
    api.get<PortalLinkPage>(`/api/v1/customers/${enc(id)}/portal-links`, { query: { ...params }, signal }),
  revokePortalLink: (id: string, linkId: string) =>
    api.delete(`/api/v1/customers/${enc(id)}/portal-links/${enc(linkId)}`),
};
