import type { Charge, InvoiceDetails, Session } from '../api/types';
import { PERMISSIONS, type Permission } from '../session/permissions';

export const USER_ID = '0195f1a0-0000-7000-8000-000000000001';
export const TENANT_A = '0195f1a0-0000-7000-8000-0000000000aa';
export const TENANT_B = '0195f1a0-0000-7000-8000-0000000000bb';

export function makeSession(
  options: { permissions?: readonly Permission[]; tenantId?: string | null; csrf?: string } = {},
): Session {
  const tenantId = options.tenantId === undefined ? TENANT_A : options.tenantId;
  const organizations = [
    { id: TENANT_A, name: 'Acme Software', slug: 'acme', role: 'owner' as const, status: 'active' },
    { id: TENANT_B, name: 'Beta Consultoria', slug: 'beta', role: 'finance' as const, status: 'active' },
  ];
  const active = organizations.find((o) => o.id === tenantId);
  return {
    user: { id: USER_ID, email: 'ana.owner@acme.test', display_name: 'Ana Owner' },
    csrf_token: options.csrf ?? 'csrf-123',
    platform_staff: false,
    organizations,
    active_tenant: active
      ? { ...active, permissions: [...(options.permissions ?? PERMISSIONS)], timezone: 'America/Sao_Paulo' }
      : null,
  };
}

export const ALL_PERMISSIONS = PERMISSIONS;

export const AUDITOR_PERMISSIONS: Permission[] = PERMISSIONS.filter(
  (p) => p.endsWith('.read') || p === 'audit.read',
);

export function makeCharge(overrides: Partial<Charge> = {}): Charge {
  return {
    id: '0195f1a0-0000-7000-8000-00000000c001',
    invoice_id: '0195f1a0-0000-7000-8000-00000000f001',
    customer_id: '0195f1a0-0000-7000-8000-00000000d001',
    provider: 'simulator',
    attempt: 1,
    status: 'awaiting_payment',
    amount: 15990,
    amount_paid: 0,
    fee_amount: 0,
    amount_refunded: 0,
    amount_charged_back: 0,
    refundable_amount: 0,
    currency: 'BRL',
    checkout_url: 'https://pagamentos.exemplo.test/checkout/abc',
    due_date: '2026-11-26',
    created_at: '2026-11-16T12:00:00Z',
    version: 1,
    ...overrides,
  };
}

export function makeInvoice(overrides: Partial<InvoiceDetails> = {}): InvoiceDetails {
  return {
    id: '0195f1a0-0000-7000-8000-00000000f001',
    number: 'ACME-000123',
    customer_id: '0195f1a0-0000-7000-8000-00000000d001',
    customer_name: 'Cliente Exemplo Ltda',
    subscription_id: '0195f1a0-0000-7000-8000-00000000e001',
    status: 'partially_paid',
    kind: 'subscription_cycle',
    currency: 'BRL',
    billing_period_start: '2026-11-01T03:00:00Z',
    billing_period_end: '2026-12-01T03:00:00Z',
    issued_at: '2026-11-01T12:00:00Z',
    due_at: '2026-11-11T02:59:59Z',
    due_date: '2026-11-10',
    subtotal: 123456,
    discount_total: 3456,
    total: 120000,
    credit_applied: 5000,
    amount_paid: 50000,
    amount_credited: 10000,
    amount_written_off: 0,
    amount_due: 55000,
    overdue: true,
    created_at: '2026-11-01T12:00:00Z',
    version: 4,
    items: [
      {
        id: '0195f1a0-0000-7000-8000-00000000a001',
        position: 1,
        kind: 'subscription_fee',
        description: 'Plano Profissional (novembro)',
        quantity: '3',
        unit_amount_decimal: '41152',
        amount: 123456,
        discount_amount: 3456,
        period_start: '2026-11-01T03:00:00Z',
        period_end: '2026-12-01T03:00:00Z',
        price_version_id: '0195f1a0-0000-7000-8000-00000000b001',
        price_snapshot: { pricing_model: 'per_unit', unit_amount_decimal: '41152', version: 2 },
      },
    ],
    history: [
      {
        id: '0195f1a0-0000-7000-8000-000000009001',
        from_status: 'draft',
        to_status: 'open',
        occurred_at: '2026-11-01T12:00:00Z',
        actor_type: 'system',
        reason: 'Emitida pelo motor de recorrência',
      },
    ],
    charges: [makeCharge()],
    customer_snapshot: { name: 'Cliente Exemplo Ltda', document_number: '11222333000181' },
    ...overrides,
  };
}
