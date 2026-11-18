package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ricartefelipe/recorvia/internal/tenancy"
)

var (
	jsonMarshal   = json.Marshal
	jsonUnmarshal = json.Unmarshal
)

// registerTenantRoutes declara as rotas com organização ativa. Cada rota
// informa a permissão exigida; operações financeiras de criação exigem
// Idempotency-Key.
func (a *API) registerTenantRoutes(r chi.Router) {
	t := func(method, pattern string, perm tenancy.Permission, h http.HandlerFunc, opts ...routeOpt) {
		a.handle(r, method, pattern, TenantScoped, perm, h, opts...)
	}
	const (
		get, post, put, patch, del = http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete
	)

	t(get, "/api/v1/organization", tenancy.PermOrganizationRead, a.getOrganization)
	t(patch, "/api/v1/organization", tenancy.PermOrganizationUpdate, a.updateOrganization)
	t(get, "/api/v1/roles", tenancy.PermMembersRead, a.listRoles)
	t(get, "/api/v1/members", tenancy.PermMembersRead, a.listMembers)
	t(patch, "/api/v1/members/{user_id}", tenancy.PermMembersManage, a.updateMemberRole)
	t(del, "/api/v1/members/{user_id}", tenancy.PermMembersManage, a.removeMember)
	t(get, "/api/v1/invitations", tenancy.PermMembersRead, a.listInvitations)
	t(post, "/api/v1/invitations", tenancy.PermMembersManage, a.createInvitation)
	t(del, "/api/v1/invitations/{id}", tenancy.PermMembersManage, a.revokeInvitation)

	t(get, "/api/v1/platform/overview", tenancy.PermPlatformRead, a.platformOverview)
	t(get, "/api/v1/platform/plans", tenancy.PermPlatformRead, a.platformPlans)
	t(put, "/api/v1/platform/plan", tenancy.PermPlatformManage, a.changePlatformPlan)
	t(get, "/api/v1/platform/addons", tenancy.PermPlatformRead, a.platformAddons)
	t(get, "/api/v1/platform/addon-orders", tenancy.PermPlatformRead, a.listAddonOrders)
	t(post, "/api/v1/platform/addon-orders", tenancy.PermPlatformManage, a.createAddonOrder)
	t(get, "/api/v1/platform/statements", tenancy.PermPlatformRead, a.listPlatformStatements)

	t(get, "/api/v1/customers", tenancy.PermCustomersRead, a.listCustomers)
	t(post, "/api/v1/customers", tenancy.PermCustomersWrite, a.createCustomer)
	t(get, "/api/v1/customers/{id}", tenancy.PermCustomersRead, a.getCustomer)
	t(patch, "/api/v1/customers/{id}", tenancy.PermCustomersWrite, a.updateCustomer)
	t(post, "/api/v1/customers/{id}/archive", tenancy.PermCustomersWrite, a.archiveCustomer)
	t(post, "/api/v1/customers/{id}/contacts", tenancy.PermCustomersWrite, a.addContact)
	t(del, "/api/v1/customers/{id}/contacts/{contact_id}", tenancy.PermCustomersWrite, a.removeContact)
	t(put, "/api/v1/customers/{id}/billing-address", tenancy.PermCustomersWrite, a.setAddress)
	t(get, "/api/v1/customers/{id}/history", tenancy.PermCustomersRead, a.customerHistory)
	t(get, "/api/v1/customers/{id}/credits", tenancy.PermCustomersRead, a.getCredits)
	t(post, "/api/v1/customers/{id}/credits", tenancy.PermCreditsWrite, a.grantCredit, idempotent())
	t(post, "/api/v1/customers/{id}/portal-links", tenancy.PermPortalLinks, a.createPortalLink)

	t(get, "/api/v1/products", tenancy.PermCatalogRead, a.listProducts)
	t(post, "/api/v1/products", tenancy.PermCatalogWrite, a.createProduct)
	t(patch, "/api/v1/products/{id}", tenancy.PermCatalogWrite, a.updateProduct)
	t(get, "/api/v1/metrics", tenancy.PermCatalogRead, a.listMetrics)
	t(post, "/api/v1/metrics", tenancy.PermCatalogWrite, a.createMetric)
	t(get, "/api/v1/plans", tenancy.PermCatalogRead, a.listPlans)
	t(post, "/api/v1/plans", tenancy.PermCatalogWrite, a.createPlan)
	t(get, "/api/v1/plans/{id}", tenancy.PermCatalogRead, a.getPlan)
	t(patch, "/api/v1/plans/{id}", tenancy.PermCatalogWrite, a.updatePlan)
	t(post, "/api/v1/plans/{id}/prices", tenancy.PermCatalogWrite, a.addPrice)

	t(get, "/api/v1/subscriptions", tenancy.PermSubscriptionsRead, a.listSubscriptions)
	t(post, "/api/v1/subscriptions", tenancy.PermSubscriptionsWrite, a.createSubscription)
	t(get, "/api/v1/subscriptions/{id}", tenancy.PermSubscriptionsRead, a.getSubscription)
	t(get, "/api/v1/subscriptions/{id}/history", tenancy.PermSubscriptionsRead, a.subscriptionHistory)
	t(post, "/api/v1/subscriptions/{id}/cancel", tenancy.PermSubscriptionsCancel, a.cancelSubscription)
	t(post, "/api/v1/subscriptions/{id}/resume-renewal", tenancy.PermSubscriptionsWrite, a.resumeRenewal)
	t(post, "/api/v1/subscriptions/{id}/suspend", tenancy.PermSubscriptionsWrite, a.suspendSubscription)
	t(post, "/api/v1/subscriptions/{id}/reactivate", tenancy.PermSubscriptionsWrite, a.reactivateSubscription)
	t(post, "/api/v1/subscriptions/{id}/change-plan", tenancy.PermSubscriptionsWrite, a.changeSubscriptionPlan)
	t(post, "/api/v1/subscriptions/{id}/change-quantity", tenancy.PermSubscriptionsWrite, a.changeSubscriptionQuantity)
	t(get, "/api/v1/subscriptions/{id}/price-adjustments", tenancy.PermSubscriptionsRead, a.listPriceAdjustments)
	t(post, "/api/v1/subscriptions/{id}/price-adjustments", tenancy.PermSubscriptionsWrite, a.schedulePriceAdjustment)
	t(del, "/api/v1/subscriptions/{id}/price-adjustments/{adjustment_id}", tenancy.PermSubscriptionsWrite, a.cancelPriceAdjustment)
	t(post, "/api/v1/subscriptions/{id}/discounts", tenancy.PermSubscriptionsWrite, a.applySubscriptionCoupon)
	t(del, "/api/v1/subscriptions/{id}/discounts", tenancy.PermSubscriptionsWrite, a.removeSubscriptionDiscount)
	t(get, "/api/v1/subscriptions/{id}/usage", tenancy.PermUsageRead, a.getSubscriptionUsage)
	t(get, "/api/v1/usage-events", tenancy.PermUsageRead, a.listUsageEvents)
	t(post, "/api/v1/usage-events", tenancy.PermUsageWrite, a.ingestUsageEvents)
	t(get, "/api/v1/coupons", tenancy.PermCatalogRead, a.listCoupons)
	t(post, "/api/v1/coupons", tenancy.PermCatalogWrite, a.createCoupon)
	t(post, "/api/v1/coupons/{id}/archive", tenancy.PermCatalogWrite, a.archiveCoupon)

	t(get, "/api/v1/invoices", tenancy.PermInvoicesRead, a.listInvoices)
	t(get, "/api/v1/invoices/export", tenancy.PermInvoicesExport, a.exportInvoices)
	t(get, "/api/v1/invoices/{id}", tenancy.PermInvoicesRead, a.getInvoice)
	t(get, "/api/v1/invoices/{id}/document", tenancy.PermInvoicesRead, a.invoiceDocument)
	t(post, "/api/v1/invoices/{id}/void", tenancy.PermInvoicesVoid, a.voidInvoice)
	t(post, "/api/v1/invoices/{id}/mark-uncollectible", tenancy.PermInvoicesVoid, a.markUncollectible)
	t(post, "/api/v1/invoices/{id}/payments", tenancy.PermPaymentsManual, a.recordManualPayment, idempotent())
	t(post, "/api/v1/invoices/{id}/credit-notes", tenancy.PermInvoicesWrite, a.createCreditNote, idempotent())
	t(post, "/api/v1/invoices/{id}/charges", tenancy.PermPaymentsCharge, a.retryCharge, idempotent())
	t(post, "/api/v1/billing-runs", tenancy.PermBillingRun, a.runBilling, idempotent())

	t(get, "/api/v1/charges", tenancy.PermPaymentsRead, a.listCharges)
	t(get, "/api/v1/charges/{id}", tenancy.PermPaymentsRead, a.getCharge)
	t(get, "/api/v1/payments", tenancy.PermPaymentsRead, a.listPayments)
	t(get, "/api/v1/payment-operations", tenancy.PermPaymentsRead, a.listOperations)
	t(post, "/api/v1/payment-operations/{id}/retry", tenancy.PermPaymentsCharge, a.retryOperation)
	t(get, "/api/v1/payment-providers", tenancy.PermIntegrationsRead, a.listProviders)
	t(put, "/api/v1/payment-providers/{provider}", tenancy.PermIntegrationsWrite, a.configureProvider)
	t(get, "/api/v1/api-credentials", tenancy.PermAPICredentials, a.listAPICredentials)
	t(post, "/api/v1/api-credentials", tenancy.PermAPICredentials, a.createAPICredential)
	t(post, "/api/v1/api-credentials/{id}/rotate", tenancy.PermAPICredentials, a.rotateAPICredential)
	t(del, "/api/v1/api-credentials/{id}", tenancy.PermAPICredentials, a.revokeAPICredential)

	t(get, "/api/v1/collection-policies", tenancy.PermCollectionsRead, a.listCollectionPolicies)
	t(post, "/api/v1/collection-policies", tenancy.PermCollectionsWrite, a.createCollectionPolicy)
	t(patch, "/api/v1/collection-policies/{id}", tenancy.PermCollectionsWrite, a.updateCollectionPolicy)
	t(get, "/api/v1/notification-templates", tenancy.PermCollectionsRead, a.listNotificationTemplates)
	t(put, "/api/v1/notification-templates/{key}", tenancy.PermCollectionsWrite, a.updateNotificationTemplate)
	t(get, "/api/v1/notifications", tenancy.PermCollectionsRead, a.listNotifications)

	t(get, "/api/v1/webhook-event-types", tenancy.PermWebhooksRead, a.listWebhookEventTypes)
	t(get, "/api/v1/webhook-endpoints", tenancy.PermWebhooksRead, a.listWebhookEndpoints)
	t(post, "/api/v1/webhook-endpoints", tenancy.PermWebhooksWrite, a.createWebhookEndpoint)
	t(patch, "/api/v1/webhook-endpoints/{id}", tenancy.PermWebhooksWrite, a.updateWebhookEndpoint)
	t(del, "/api/v1/webhook-endpoints/{id}", tenancy.PermWebhooksWrite, a.deleteWebhookEndpoint)
	t(post, "/api/v1/webhook-endpoints/{id}/rotate-secret", tenancy.PermWebhooksWrite, a.rotateWebhookSecret)
	t(get, "/api/v1/webhook-endpoints/{id}/deliveries", tenancy.PermWebhooksRead, a.listWebhookDeliveries)
	t(post, "/api/v1/webhook-deliveries/{id}/retry", tenancy.PermWebhooksWrite, a.retryWebhookDelivery)

	t(get, "/api/v1/ledger/accounts", tenancy.PermLedgerRead, a.ledgerAccounts)
	t(get, "/api/v1/ledger/transactions", tenancy.PermLedgerRead, a.ledgerTransactions)
	t(get, "/api/v1/audit-events", tenancy.PermAuditRead, a.listAuditEvents)
	t(get, "/api/v1/dead-letters", tenancy.PermOperationsRead, a.listDeadLetters)
	t(post, "/api/v1/dead-letters/{id}/replay", tenancy.PermOperationsReprocess, a.resolveDeadLetter(true))
	t(post, "/api/v1/dead-letters/{id}/discard", tenancy.PermOperationsReprocess, a.resolveDeadLetter(false))
	t(get, "/api/v1/dashboard", tenancy.PermDashboardRead, a.dashboard)
}
