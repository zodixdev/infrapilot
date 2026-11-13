package domain

import "slices"

// Role is an organization role (ADR-0022).
type Role string

// Roles, highest first.
const (
	RoleOwner    Role = "owner"
	RoleAdmin    Role = "admin"
	RoleOperator Role = "operator"
	RoleViewer   Role = "viewer"
)

// Roles lists every role, highest first.
var Roles = []Role{RoleOwner, RoleAdmin, RoleOperator, RoleViewer}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return slices.Contains(Roles, r) }

// Permission is a named capability checked on every operation (ADR-0022).
type Permission string

// Permissions.
const (
	PermOrgRead       Permission = "org.read"
	PermOrgUpdate     Permission = "org.update"
	PermOrgDelete     Permission = "org.delete"
	PermMemberRead    Permission = "member.read"
	PermMemberManage  Permission = "member.manage"
	PermProjectRead   Permission = "project.read"
	PermProjectWrite  Permission = "project.write"
	PermResourceRead  Permission = "resource.read"
	PermResourceWrite Permission = "resource.write"
	PermAPIKeyRead    Permission = "apikey.read"
	PermAPIKeyManage  Permission = "apikey.manage"
	PermAuditRead     Permission = "audit.read"
)

// Permissions lists every permission.
var Permissions = []Permission{
	PermOrgRead, PermOrgUpdate, PermOrgDelete, PermMemberRead, PermMemberManage, PermProjectRead, PermProjectWrite,
	PermResourceRead, PermResourceWrite, PermAPIKeyRead, PermAPIKeyManage, PermAuditRead,
}

// rolePermissions is the matrix of ADR-0022. Each role lists exactly what it may do.
var rolePermissions = map[Role][]Permission{
	RoleViewer:   {PermOrgRead, PermMemberRead, PermProjectRead, PermResourceRead},
	RoleOperator: {PermOrgRead, PermMemberRead, PermProjectRead, PermResourceRead, PermResourceWrite},
	RoleAdmin: {PermOrgRead, PermMemberRead, PermProjectRead, PermResourceRead, PermResourceWrite, PermProjectWrite,
		PermMemberManage, PermAPIKeyRead, PermAPIKeyManage, PermAuditRead, PermOrgUpdate},
	RoleOwner: {PermOrgRead, PermMemberRead, PermProjectRead, PermResourceRead, PermResourceWrite, PermProjectWrite,
		PermMemberManage, PermAPIKeyRead, PermAPIKeyManage, PermAuditRead, PermOrgUpdate, PermOrgDelete},
}

// RoleAllows reports whether role grants perm.
func RoleAllows(role Role, perm Permission) bool {
	return slices.Contains(rolePermissions[role], perm)
}

// Scope is an API key scope (ADR-0022).
type Scope string

// Scopes.
const (
	ScopeRead   Scope = "read"
	ScopeWrite  Scope = "write"
	ScopeIngest Scope = "ingest"
)

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s == ScopeRead || s == ScopeWrite || s == ScopeIngest }

// scopePermissions: what each scope grants inside the key's own project. Keys never manage members, keys,
// projects or the organization.
var scopePermissions = map[Scope][]Permission{
	ScopeRead:   {PermOrgRead, PermProjectRead, PermResourceRead},
	ScopeWrite:  {PermOrgRead, PermProjectRead, PermResourceRead, PermResourceWrite},
	ScopeIngest: nil,
}

// ProjectScoped reports whether perm concerns objects inside a project, so an API key must be bound to it.
func ProjectScoped(perm Permission) bool {
	return perm == PermProjectRead || perm == PermResourceRead || perm == PermResourceWrite
}

// KeyAllows reports whether a key with scopes, bound to keyProject, may use perm on project (uuid strings;
// project may be empty for organization-level permissions).
func KeyAllows(scopes []Scope, keyProject, project string, perm Permission) bool {
	if ProjectScoped(perm) && project != keyProject {
		return false
	}
	if !ProjectScoped(perm) && perm != PermOrgRead {
		return false
	}
	for _, s := range scopes {
		if slices.Contains(scopePermissions[s], perm) {
			return true
		}
	}
	return false
}

// MemberChange is a requested membership mutation, checked by CheckMemberChange.
type MemberChange struct {
	ActorRole   Role // role of the acting user
	ActorIsSelf bool // the actor changes their own membership
	Current     Role // target's current role ("" when adding)
	New         Role // target's new role ("" when removing)
	Owners      int  // number of owners before the change
}

// CheckMemberChange enforces the membership invariants of ADR-0022:
//   - member.manage is required, except that members may remove themselves;
//   - only an owner can grant, change or revoke the owner role, and admins cannot modify owners;
//   - the last owner can be neither removed nor demoted.
func CheckMemberChange(c MemberChange) error {
	removingSelf := c.ActorIsSelf && c.New == ""
	if !removingSelf && !RoleAllows(c.ActorRole, PermMemberManage) {
		return Deniedf("changing members requires the %s permission", PermMemberManage)
	}
	if (c.Current == RoleOwner || c.New == RoleOwner) && c.ActorRole != RoleOwner && !removingSelf {
		return Deniedf("only owners can grant, change or revoke the owner role")
	}
	if c.Current == RoleOwner && c.New != RoleOwner && c.Owners <= 1 {
		return Preconditionf("an organization must keep at least one owner")
	}
	return nil
}
