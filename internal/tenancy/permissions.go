package tenancy

import (
	"fmt"
	"slices"
)

// Role é o papel de um membro dentro de uma organização.
type Role string

const (
	// RoleOwner controla a organização, incluindo o plano da plataforma.
	RoleOwner Role = "owner"
	// RoleAdmin administra membros, integrações e configurações.
	RoleAdmin Role = "admin"
	// RoleFinance opera faturamento, cobranças, reembolsos e conciliação.
	RoleFinance Role = "finance"
	// RoleOperator mantém clientes, assinaturas e consumo.
	RoleOperator Role = "operator"
	// RoleAuditor tem acesso somente de leitura, incluindo auditoria.
	RoleAuditor Role = "auditor"
)

// Roles lista os papéis em ordem decrescente de privilégio.
func Roles() []Role { return []Role{RoleOwner, RoleAdmin, RoleFinance, RoleOperator, RoleAuditor} }

// ParseRole valida um papel.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if slices.Contains(Roles(), r) {
		return r, nil
	}
	return "", fmt.Errorf("papel desconhecido: %q", s)
}

// Permission é uma ação autorizável.
type Permission string

// Permissões explícitas por ação.
const (
	PermOrganizationRead   Permission = "organization.read"
	PermOrganizationUpdate Permission = "organization.update"
	PermMembersRead        Permission = "members.read"
	PermMembersManage      Permission = "members.manage"
	PermPlatformRead       Permission = "platform_billing.read"
	PermPlatformManage     Permission = "platform_billing.manage"

	PermCustomersRead  Permission = "customers.read"
	PermCustomersWrite Permission = "customers.write"
	PermPortalLinks    Permission = "customers.portal_links"

	PermCatalogRead  Permission = "catalog.read"
	PermCatalogWrite Permission = "catalog.write"

	PermSubscriptionsRead   Permission = "subscriptions.read"
	PermSubscriptionsWrite  Permission = "subscriptions.write"
	PermSubscriptionsCancel Permission = "subscriptions.cancel"

	PermUsageRead  Permission = "usage.read"
	PermUsageWrite Permission = "usage.write"

	PermInvoicesRead   Permission = "invoices.read"
	PermInvoicesWrite  Permission = "invoices.write"
	PermInvoicesVoid   Permission = "invoices.void"
	PermInvoicesExport Permission = "invoices.export"
	PermBillingRun     Permission = "billing.run"
	PermCreditsWrite   Permission = "credits.write"

	PermPaymentsRead   Permission = "payments.read"
	PermPaymentsCharge Permission = "payments.charge"
	PermPaymentsRefund Permission = "payments.refund"
	PermPaymentsManual Permission = "payments.record_manual"

	PermLedgerRead Permission = "ledger.read"

	PermReconciliationRead    Permission = "reconciliation.read"
	PermReconciliationRun     Permission = "reconciliation.run"
	PermReconciliationResolve Permission = "reconciliation.resolve"

	PermCollectionsRead  Permission = "collections.read"
	PermCollectionsWrite Permission = "collections.write"

	PermIntegrationsRead  Permission = "integrations.read"
	PermIntegrationsWrite Permission = "integrations.write"
	PermWebhooksRead      Permission = "webhooks.read"
	PermWebhooksWrite     Permission = "webhooks.write"
	PermAPICredentials    Permission = "api_credentials.manage"

	PermAuditRead            Permission = "audit.read"
	PermOperationsRead       Permission = "operations.read"
	PermOperationsReprocess  Permission = "operations.reprocess"
	PermDashboardRead        Permission = "dashboard.read"
)

var readAll = []Permission{
	PermOrganizationRead, PermMembersRead, PermPlatformRead, PermCustomersRead, PermCatalogRead,
	PermSubscriptionsRead, PermUsageRead, PermInvoicesRead, PermPaymentsRead, PermLedgerRead,
	PermReconciliationRead, PermCollectionsRead, PermIntegrationsRead, PermWebhooksRead,
	PermOperationsRead, PermDashboardRead,
}

var rolePermissions = map[Role][]Permission{
	RoleOwner: nil, // todas
	RoleAdmin: append(slices.Clone(readAll),
		PermOrganizationUpdate, PermMembersManage,
		PermCustomersWrite, PermPortalLinks, PermCatalogWrite, PermSubscriptionsWrite, PermSubscriptionsCancel,
		PermUsageWrite, PermInvoicesWrite, PermInvoicesVoid, PermInvoicesExport, PermBillingRun, PermCreditsWrite,
		PermPaymentsCharge, PermPaymentsRefund, PermPaymentsManual,
		PermReconciliationRun, PermReconciliationResolve, PermCollectionsWrite,
		PermIntegrationsWrite, PermWebhooksWrite, PermAPICredentials, PermAuditRead, PermOperationsReprocess),
	RoleFinance: append(slices.Clone(readAll),
		PermPortalLinks, PermInvoicesWrite, PermInvoicesVoid, PermInvoicesExport, PermBillingRun, PermCreditsWrite,
		PermPaymentsCharge, PermPaymentsRefund, PermPaymentsManual,
		PermReconciliationRun, PermReconciliationResolve, PermCollectionsWrite, PermSubscriptionsCancel),
	RoleOperator: {
		PermOrganizationRead, PermMembersRead, PermCustomersRead, PermCustomersWrite, PermPortalLinks,
		PermCatalogRead, PermSubscriptionsRead, PermSubscriptionsWrite, PermUsageRead, PermUsageWrite,
		PermInvoicesRead, PermPaymentsRead, PermCollectionsRead, PermDashboardRead,
	},
	RoleAuditor: append(slices.Clone(readAll), PermAuditRead),
}

// AllPermissions lista todas as permissões em ordem estável.
func AllPermissions() []Permission {
	all := append(slices.Clone(readAll),
		PermOrganizationUpdate, PermMembersManage, PermPlatformManage,
		PermCustomersWrite, PermPortalLinks, PermCatalogWrite, PermSubscriptionsWrite, PermSubscriptionsCancel,
		PermUsageWrite, PermInvoicesWrite, PermInvoicesVoid, PermInvoicesExport, PermBillingRun, PermCreditsWrite,
		PermPaymentsCharge, PermPaymentsRefund, PermPaymentsManual,
		PermReconciliationRun, PermReconciliationResolve, PermCollectionsWrite,
		PermIntegrationsWrite, PermWebhooksWrite, PermAPICredentials, PermAuditRead, PermOperationsReprocess)
	slices.Sort(all)
	return slices.Compact(all)
}

// Can informa se o papel concede a permissão.
func (r Role) Can(p Permission) bool {
	perms, ok := rolePermissions[r]
	if !ok {
		return false
	}
	if r == RoleOwner {
		return slices.Contains(AllPermissions(), p)
	}
	return slices.Contains(perms, p)
}

// Permissions lista as permissões do papel em ordem estável.
func (r Role) Permissions() []Permission {
	var out []Permission
	for _, p := range AllPermissions() {
		if r.Can(p) {
			out = append(out, p)
		}
	}
	return out
}

// ParsePermission valida uma permissão (usada em escopos de credenciais).
func ParsePermission(s string) (Permission, error) {
	p := Permission(s)
	if slices.Contains(AllPermissions(), p) {
		return p, nil
	}
	return "", fmt.Errorf("permissão desconhecida: %q", s)
}
