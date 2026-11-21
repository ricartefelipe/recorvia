import type { components } from './schema';

type S = components['schemas'];

export type Session = S['Session'];
export type ActiveTenant = NonNullable<Session['active_tenant']>;
export type OrganizationSummary = S['OrganizationSummary'];
export type OrganizationInput = S['OrganizationInput'];
export type Organization = S['Organization'];
export type OrganizationUpdate = S['OrganizationUpdate'];
export type Role = S['Role'];
export type RolePermissions = S['RolePermissions'];
export type Member = S['Member'];
export type Invitation = S['Invitation'];
export type InvitationCreated = S['InvitationCreated'];

export type PlatformPlan = S['PlatformPlan'];
export type PlatformOverview = S['PlatformOverview'];
export type PlatformAddon = S['PlatformAddon'];
export type AddonOrder = S['AddonOrder'];
export type PlatformStatement = S['PlatformStatement'];

export type CustomerInput = S['CustomerInput'];
export type Customer = S['Customer'];
export type CustomerDetails = S['CustomerDetails'];
export type CustomerAnonymization = S['CustomerAnonymization'];
export type CustomerPage = S['CustomerPage'];
export type ContactInput = S['ContactInput'];
export type Contact = S['Contact'];
export type Address = S['Address'];
export type CreditBalance = S['CreditBalance'];

export type ProductInput = S['ProductInput'];
export type Product = S['Product'];
export type Aggregation = S['Aggregation'];
export type MetricInput = S['MetricInput'];
export type Metric = S['Metric'];
export type PricingModel = S['PricingModel'];
export type PriceInput = S['PriceInput'];
export type PriceVersion = S['PriceVersion'];
export type PlanInput = S['PlanInput'];
export type PlanUpdate = S['PlanUpdate'];
export type Plan = S['Plan'];
export type PlanDetails = S['PlanDetails'];
export type CouponInput = S['CouponInput'];
export type Coupon = S['Coupon'];

export type SubscriptionStatus = S['SubscriptionStatus'];
export type ChangeTiming = S['ChangeTiming'];
export type SubscriptionInput = S['SubscriptionInput'];
export type Subscription = S['Subscription'];
export type SubscriptionPage = S['SubscriptionPage'];
export type SubscriptionHistoryEntry = S['SubscriptionHistoryEntry'];
export type SubscriptionDiscount = S['SubscriptionDiscount'];
export type PriceAdjustmentInput = S['PriceAdjustmentInput'];
export type PriceAdjustment = S['PriceAdjustment'];
export type SubscriptionUsage = S['SubscriptionUsage'];
export type UsageEvent = S['UsageEvent'];
export type UsageEventPage = S['UsageEventPage'];

export type InvoiceStatus = S['InvoiceStatus'];
export type Invoice = S['Invoice'];
export type InvoiceItem = S['InvoiceItem'];
export type InvoiceHistoryEntry = S['InvoiceHistoryEntry'];
export type InvoiceDetails = S['InvoiceDetails'];
export type InvoicePage = S['InvoicePage'];
export type ManualPaymentInput = S['ManualPaymentInput'];
export type BillingRunResult = S['BillingRunResult'];

export type ChargeStatus = S['ChargeStatus'];
export type Charge = S['Charge'];
export type ChargePage = S['ChargePage'];
export type Refund = S['Refund'];
export type Payment = S['Payment'];
export type Dispute = S['Dispute'];
export type PaymentOperation = S['PaymentOperation'];

export type ProviderAccountInput = S['ProviderAccountInput'];
export type ProviderAccount = S['ProviderAccount'];
export type ProviderAccountConfigured = S['ProviderAccountConfigured'];

export type LedgerAccount = S['LedgerAccount'];
export type LedgerTransaction = S['LedgerTransaction'];

export type DiscrepancyKind = S['DiscrepancyKind'];
export type ReconciliationRun = S['ReconciliationRun'];
export type Discrepancy = S['Discrepancy'];

export type TemplateKey = S['TemplateKey'];
export type CollectionPolicyInput = S['CollectionPolicyInput'];
export type CollectionPolicy = S['CollectionPolicy'];
export type NotificationTemplate = S['NotificationTemplate'];
export type Notification = S['Notification'];

export type WebhookEndpointInput = S['WebhookEndpointInput'];
export type WebhookEndpoint = S['WebhookEndpoint'];
export type WebhookEndpointWithSecret = S['WebhookEndpointWithSecret'];
export type WebhookDelivery = S['WebhookDelivery'];

export type ApiCredential = S['ApiCredential'];
export type ApiCredentialWithSecret = S['ApiCredentialWithSecret'];

export type DeadLetter = S['DeadLetter'];
export type AuditEvent = S['AuditEvent'];
export type Dashboard = S['Dashboard'];

export type PortalSession = S['PortalSession'];
export type PortalInvoice = S['PortalInvoice'];
export type PortalInvoicePage = S['PortalInvoicePage'];
export type PortalLink = S['PortalLink'];
export type PortalLinkPage = S['PortalLinkPage'];

/** Resposta de lista simples do contrato. */
export interface DataList<T> {
  data: T[];
}

/** Resposta de lista paginada por cursor. */
export interface CursorPage<T> {
  data: T[];
  next_cursor?: string | null;
}
