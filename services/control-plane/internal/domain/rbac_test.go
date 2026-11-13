package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

// TestPermissions_Matrix is the golden copy of the ADR-0022 matrix. Changing a role's rights must change this
// table and the ADR together.
func TestPermissions_Matrix(t *testing.T) {
	t.Parallel()
	const o, a, op, v = "owner", "admin", "operator", "viewer"
	want := map[domain.Permission][]string{
		domain.PermOrgRead:       {o, a, op, v},
		domain.PermMemberRead:    {o, a, op, v},
		domain.PermProjectRead:   {o, a, op, v},
		domain.PermResourceRead:  {o, a, op, v},
		domain.PermResourceWrite: {o, a, op},
		domain.PermProjectWrite:  {o, a},
		domain.PermMemberManage:  {o, a},
		domain.PermAPIKeyRead:    {o, a},
		domain.PermAPIKeyManage:  {o, a},
		domain.PermAuditRead:     {o, a},
		domain.PermOrgUpdate:     {o, a},
		domain.PermOrgDelete:     {o},
	}
	require.Len(t, want, len(domain.Permissions), "every permission is in the golden table")
	for _, perm := range domain.Permissions {
		for _, role := range domain.Roles {
			expected := false
			for _, r := range want[perm] {
				expected = expected || r == string(role)
			}
			require.Equal(t, expected, domain.RoleAllows(role, perm), "%s × %s", role, perm)
		}
	}
	require.False(t, domain.RoleAllows("superuser", domain.PermOrgRead), "unknown roles grant nothing")
}

func TestKeyAllows(t *testing.T) {
	t.Parallel()
	const own, other = "11111111-1111-7111-8111-111111111111", "22222222-2222-7222-8222-222222222222"
	read, write, ingest := []domain.Scope{domain.ScopeRead}, []domain.Scope{domain.ScopeWrite}, []domain.Scope{domain.ScopeIngest}
	tests := []struct {
		name    string
		scopes  []domain.Scope
		project string
		perm    domain.Permission
		want    bool
	}{
		{"read key reads own project resources", read, own, domain.PermResourceRead, true},
		{"read key cannot write", read, own, domain.PermResourceWrite, false},
		{"write key writes own project", write, own, domain.PermResourceWrite, true},
		{"write key cannot touch another project", write, other, domain.PermResourceWrite, false},
		{"read key cannot read another project", read, other, domain.PermResourceRead, false},
		{"project-scoped permission needs a project", read, "", domain.PermProjectRead, false},
		{"keys read org metadata", read, "", domain.PermOrgRead, true},
		{"ingest key has no control-plane rights", ingest, own, domain.PermResourceRead, false},
		{"keys never manage members", write, "", domain.PermMemberManage, false},
		{"keys never read members", write, "", domain.PermMemberRead, false},
		{"keys never manage keys", write, "", domain.PermAPIKeyManage, false},
		{"keys never write projects", write, own, domain.PermProjectWrite, false},
		{"keys never read audit", write, "", domain.PermAuditRead, false},
		{"keys never delete orgs", write, "", domain.PermOrgDelete, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, domain.KeyAllows(tc.scopes, own, tc.project, tc.perm))
		})
	}
}

func TestCheckMemberChange(t *testing.T) {
	t.Parallel()
	const (
		owner, admin, operator, viewer, none = domain.RoleOwner, domain.RoleAdmin, domain.RoleOperator, domain.RoleViewer, domain.Role("")
	)
	tests := []struct {
		name string
		c    domain.MemberChange
		want error
	}{
		{"admin adds viewer", domain.MemberChange{ActorRole: admin, Current: none, New: viewer, Owners: 1}, nil},
		{"admin adds admin", domain.MemberChange{ActorRole: admin, Current: none, New: admin, Owners: 1}, nil},
		{"admin cannot add owner", domain.MemberChange{ActorRole: admin, Current: none, New: owner, Owners: 1}, domain.ErrPermissionDenied},
		{"admin cannot demote owner", domain.MemberChange{ActorRole: admin, Current: owner, New: viewer, Owners: 2}, domain.ErrPermissionDenied},
		{"admin cannot remove owner", domain.MemberChange{ActorRole: admin, Current: owner, New: none, Owners: 2}, domain.ErrPermissionDenied},
		{"owner grants owner", domain.MemberChange{ActorRole: owner, Current: admin, New: owner, Owners: 1}, nil},
		{"owner demotes another owner", domain.MemberChange{ActorRole: owner, Current: owner, New: admin, Owners: 2}, nil},
		{"last owner cannot be demoted", domain.MemberChange{ActorRole: owner, ActorIsSelf: true, Current: owner, New: admin, Owners: 1}, domain.ErrFailedPrecondition},
		{"last owner cannot leave", domain.MemberChange{ActorRole: owner, ActorIsSelf: true, Current: owner, New: none, Owners: 1}, domain.ErrFailedPrecondition},
		{"one of two owners can leave", domain.MemberChange{ActorRole: owner, ActorIsSelf: true, Current: owner, New: none, Owners: 2}, nil},
		{"operator cannot manage", domain.MemberChange{ActorRole: operator, Current: viewer, New: operator, Owners: 1}, domain.ErrPermissionDenied},
		{"viewer can leave", domain.MemberChange{ActorRole: viewer, ActorIsSelf: true, Current: viewer, New: none, Owners: 1}, nil},
		{"viewer cannot promote self", domain.MemberChange{ActorRole: viewer, ActorIsSelf: true, Current: viewer, New: admin, Owners: 1}, domain.ErrPermissionDenied},
		{"admin cannot promote self to owner", domain.MemberChange{ActorRole: admin, ActorIsSelf: true, Current: admin, New: owner, Owners: 1}, domain.ErrPermissionDenied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := domain.CheckMemberChange(tc.c)
			if tc.want == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}
