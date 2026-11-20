package tenancy

import (
	"slices"
	"strings"
	"testing"
)

func TestParseRole(t *testing.T) {
	for _, r := range []string{"owner", "admin", "finance", "operator", "auditor"} {
		got, err := ParseRole(r)
		if err != nil || string(got) != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, err)
		}
	}
	for _, r := range []string{"", "Owner", "root", "superadmin", " admin"} {
		if got, err := ParseRole(r); err == nil || got != "" {
			t.Errorf("ParseRole(%q) deveria falhar, obtido %q", r, got)
		}
	}
}

func TestRolesOrderedByPrivilege(t *testing.T) {
	want := []Role{RoleOwner, RoleAdmin, RoleFinance, RoleOperator, RoleAuditor}
	if got := Roles(); !slices.Equal(got, want) {
		t.Fatalf("Roles = %v, esperado %v", got, want)
	}
	// Alterar o slice devolvido não afeta chamadas seguintes.
	r := Roles()
	r[0] = "hacker"
	if Roles()[0] != RoleOwner {
		t.Fatal("Roles deve devolver uma cópia")
	}
}

func TestParsePermission(t *testing.T) {
	for _, p := range AllPermissions() {
		got, err := ParsePermission(string(p))
		if err != nil || got != p {
			t.Errorf("ParsePermission(%q) = %q, %v", p, got, err)
		}
	}
	for _, p := range []string{"", "customers", "customers.delete", "CUSTOMERS.READ", "*"} {
		if got, err := ParsePermission(p); err == nil || got != "" {
			t.Errorf("ParsePermission(%q) deveria falhar, obtido %q", p, got)
		}
	}
}

func TestAllPermissionsSortedAndUnique(t *testing.T) {
	all := AllPermissions()
	if !slices.IsSorted(all) {
		t.Fatal("AllPermissions deve estar ordenado")
	}
	if len(slices.Compact(slices.Clone(all))) != len(all) {
		t.Fatal("AllPermissions não deve ter repetições")
	}
	if !slices.Contains(all, PermPlatformManage) || !slices.Contains(all, PermAuditRead) {
		t.Fatal("permissões essenciais ausentes")
	}
}

func TestOwnerHasEveryPermission(t *testing.T) {
	for _, p := range AllPermissions() {
		if !RoleOwner.Can(p) {
			t.Errorf("owner deveria ter %s", p)
		}
	}
	if !slices.Equal(RoleOwner.Permissions(), AllPermissions()) {
		t.Fatal("permissões do owner devem ser todas as permissões")
	}
	if RoleOwner.Can("permissao.inexistente") {
		t.Fatal("owner não ganha permissões desconhecidas")
	}
}

func isRead(p Permission) bool { return strings.HasSuffix(string(p), ".read") }

func TestAuditorIsReadOnly(t *testing.T) {
	for _, p := range AllPermissions() {
		if got := RoleAuditor.Can(p); got != isRead(p) {
			t.Errorf("auditor.Can(%s) = %v, esperado %v", p, got, isRead(p))
		}
	}
	if !RoleAuditor.Can(PermAuditRead) {
		t.Fatal("auditor deve ler a trilha de auditoria")
	}
}

func TestRoleMatrix(t *testing.T) {
	cases := []struct {
		role Role
		perm Permission
		want bool
	}{
		// admin administra a organização, mas não o plano da plataforma.
		{RoleAdmin, PermPlatformManage, false},
		{RoleAdmin, PermPlatformRead, true},
		{RoleAdmin, PermMembersManage, true},
		{RoleAdmin, PermOrganizationUpdate, true},
		{RoleAdmin, PermAPICredentials, true},
		{RoleAdmin, PermPaymentsRefund, true},
		{RoleAdmin, PermAuditRead, true},
		// finance opera cobrança, mas não membros nem catálogo.
		{RoleFinance, PermPaymentsRefund, true},
		{RoleFinance, PermInvoicesVoid, true},
		{RoleFinance, PermBillingRun, true},
		{RoleFinance, PermSubscriptionsCancel, true},
		{RoleFinance, PermMembersManage, false},
		{RoleFinance, PermCatalogWrite, false},
		{RoleFinance, PermPlatformManage, false},
		{RoleFinance, PermAuditRead, false},
		// operator mantém clientes, assinaturas e consumo, sem operações financeiras.
		{RoleOperator, PermCustomersWrite, true},
		{RoleOperator, PermSubscriptionsWrite, true},
		{RoleOperator, PermUsageWrite, true},
		{RoleOperator, PermPaymentsRefund, false},
		{RoleOperator, PermPaymentsCharge, false},
		{RoleOperator, PermInvoicesVoid, false},
		{RoleOperator, PermSubscriptionsCancel, false},
		{RoleOperator, PermMembersManage, false},
		{RoleOperator, PermLedgerRead, false},
		{RoleOperator, PermAuditRead, false},
		// auditor apenas lê.
		{RoleAuditor, PermCustomersWrite, false},
		{RoleAuditor, PermOperationsReprocess, false},
		// papel desconhecido não tem permissão alguma.
		{Role("visitante"), PermOrganizationRead, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"/"+string(tc.perm), func(t *testing.T) {
			if got := tc.role.Can(tc.perm); got != tc.want {
				t.Fatalf("Can = %v, esperado %v", got, tc.want)
			}
		})
	}
}

func TestOnlyOwnerManagesPlatform(t *testing.T) {
	for _, r := range Roles() {
		if got := r.Can(PermPlatformManage); got != (r == RoleOwner) {
			t.Errorf("%s.Can(platform_billing.manage) = %v", r, got)
		}
	}
}

func TestPermissionsListConsistentWithCan(t *testing.T) {
	for _, r := range Roles() {
		perms := r.Permissions()
		if !slices.IsSorted(perms) {
			t.Errorf("%s: permissões devem estar ordenadas", r)
		}
		for _, p := range AllPermissions() {
			if slices.Contains(perms, p) != r.Can(p) {
				t.Errorf("%s: Permissions e Can divergem em %s", r, p)
			}
		}
	}
	if perms := Role("visitante").Permissions(); len(perms) != 0 {
		t.Fatalf("papel desconhecido não deve listar permissões: %v", perms)
	}
}
