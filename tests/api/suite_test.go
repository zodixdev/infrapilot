//go:build integration

package api_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type memberBody struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
	Email  string `json:"email"`
}

// world is one organization with a member of every role, an outsider and API keys, shared by the functional
// tests (each test only adds its own objects).
type world struct {
	s                                        *stack
	owner, admin, operator, viewer, outsider user
	org, project                             string
	readKey, writeKey                        string
	ids                                      map[string]string // user name → user id
}

func newWorld(t *testing.T, s *stack) *world {
	t.Helper()
	w := &world{s: s, ids: map[string]string{}}
	w.owner = s.user(t, "owner")
	w.org, w.project = s.fixture(t, w.owner, "World")
	for _, m := range []struct {
		u    *user
		name string
		role string
	}{{&w.admin, "admin", "admin"}, {&w.operator, "operator", "operator"}, {&w.viewer, "viewer", "viewer"}} {
		*m.u = s.user(t, m.name)
		// The user must have signed in once (any authenticated call) before they can be added.
		require.Equal(t, http.StatusOK, s.call(t, m.u.cred(), http.MethodGet, "/organizations", nil).status)
		r := s.call(t, w.owner.cred(), http.MethodPost, "/organizations/"+w.org+"/members", map[string]any{"email": m.name + "@example.com", "role": m.role})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		var mb memberBody
		r.json(t, &mb)
		w.ids[m.name] = mb.UserID
	}
	w.outsider = s.user(t, "outsider")
	s.fixture(t, w.outsider, "Elsewhere")
	w.readKey = w.newKey(t, w.project, "read")
	w.writeKey = w.newKey(t, w.project, "write")
	return w
}

func (w *world) newKey(t *testing.T, project string, scopes ...string) string {
	t.Helper()
	r := w.s.call(t, w.owner.cred(), http.MethodPost, "/organizations/"+w.org+"/api-keys", map[string]any{"project_id": project, "name": "k", "scopes": scopes})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var k keyBody
	r.json(t, &k)
	return k.Secret
}

func (w *world) newProject(t *testing.T, slug string) string {
	t.Helper()
	r := w.s.call(t, w.owner.cred(), http.MethodPost, "/organizations/"+w.org+"/projects", map[string]any{"name": slug, "slug": slug})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var p projectBody
	r.json(t, &p)
	return p.ID
}

func (w *world) newResource(t *testing.T, name string) string {
	t.Helper()
	r := w.s.call(t, w.owner.cred(), http.MethodPost, "/organizations/"+w.org+"/projects/"+w.project+"/resources", map[string]any{"kind": "service", "name": name})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var x resourceBody
	r.json(t, &x)
	return x.ID
}

func TestAPI_Functional_Integration(t *testing.T) {
	s := startStack(t, stackOptions{})
	w := newWorld(t, s)
	t.Run("CRUD", func(t *testing.T) { testCRUD(t, w) })
	t.Run("RBACMatrix", func(t *testing.T) { testRBACMatrix(t, w) })
	t.Run("CrossTenant", func(t *testing.T) { testCrossTenant(t, w) })
	t.Run("Contract", func(t *testing.T) { testContract(t, w) })
	t.Run("Audit", func(t *testing.T) { testAudit(t, w) })
}

func testCRUD(t *testing.T, w *world) {
	t.Helper()
	s, o := w.s, w.owner.cred()
	base := "/organizations/" + w.org

	// organization
	r := s.call(t, o, http.MethodPatch, base, map[string]any{"name": "World Inc"})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	var org orgBody
	r.json(t, &org)
	require.Equal(t, "World Inc", org.Name)

	// projects
	p := w.newProject(t, "crud")
	r = s.call(t, o, http.MethodPatch, base+"/projects/"+p, map[string]any{"description": "updated"})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	r = s.call(t, o, http.MethodPost, base+"/projects", map[string]any{"name": "dup", "slug": "crud"})
	require.Equal(t, http.StatusConflict, r.status)
	require.Equal(t, "conflict", r.problemCode(t))

	// resources: create, get, patch (labels replaced and cleared), list by kind, delete
	rp := base + "/projects/" + p + "/resources"
	r = s.call(t, o, http.MethodPost, rp, map[string]any{"kind": "database", "name": "pg", "labels": map[string]string{"tier": "1"}})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var res resourceBody
	r.json(t, &res)
	r = s.call(t, o, http.MethodPatch, rp+"/"+res.ID, map[string]any{"labels": map[string]string{}})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	var patched resourceBody // fresh value: Unmarshal merges into existing maps
	r.json(t, &patched)
	require.Empty(t, patched.Labels)
	r = s.call(t, o, http.MethodGet, rp+"?kind=database", nil)
	require.Equal(t, http.StatusOK, r.status)
	var rl list[resourceBody]
	r.json(t, &rl)
	require.Len(t, rl.Items, 1)
	require.Equal(t, http.StatusNoContent, s.call(t, o, http.MethodDelete, rp+"/"+res.ID, nil).status)
	require.Equal(t, http.StatusNotFound, s.call(t, o, http.MethodGet, rp+"/"+res.ID, nil).status)

	// pagination
	for i := range 5 {
		require.Equal(t, http.StatusCreated, s.call(t, o, http.MethodPost, rp, map[string]any{"kind": "host", "name": fmt.Sprintf("h%d", i)}).status)
	}
	var names []string
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 5)
		q := rp + "?limit=2"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		r = s.call(t, o, http.MethodGet, q, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		var pg list[resourceBody]
		r.json(t, &pg)
		for _, x := range pg.Items {
			names = append(names, x.Name)
		}
		if !pg.Page.HasMore {
			break
		}
		cursor = *pg.Page.NextCursor
	}
	require.Equal(t, []string{"h0", "h1", "h2", "h3", "h4"}, names)

	// members: change role, list, remove
	mp := base + "/members/" + w.ids["viewer"]
	r = s.call(t, o, http.MethodPatch, mp, map[string]any{"role": "operator"})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	r = s.call(t, o, http.MethodPatch, mp, map[string]any{"role": "viewer"})
	require.Equal(t, http.StatusOK, r.status)
	r = s.call(t, o, http.MethodGet, base+"/members", nil)
	var ml list[memberBody]
	r.json(t, &ml)
	require.Len(t, ml.Items, 4)

	// API keys: get, list, revoke; a revoked key stops working at the owning service immediately
	k := w.newKey(t, p, "read")
	r = s.call(t, o, http.MethodGet, base+"/api-keys?project_id="+p, nil)
	require.Equal(t, http.StatusOK, r.status)
	var kl list[struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
	}]
	r.json(t, &kl)
	require.Len(t, kl.Items, 1)
	require.Equal(t, kl.Items[0].Prefix, k[:12])
	require.NotContains(t, string(r.body), k, "secrets are never listed")
	require.Equal(t, http.StatusOK, s.call(t, key(k), http.MethodGet, base+"/projects/"+p, nil).status)
	require.Equal(t, http.StatusOK, s.call(t, o, http.MethodPost, base+"/api-keys/"+kl.Items[0].ID+"/revoke", nil).status)
	require.NotEqual(t, http.StatusOK, s.call(t, key(k), http.MethodGet, base+"/projects/"+p+"/resources", nil).status,
		"a revoked key is refused even while the edge still caches it")

	// project delete cascades
	require.Equal(t, http.StatusNoContent, s.call(t, o, http.MethodDelete, base+"/projects/"+p, nil).status)
	require.Equal(t, http.StatusNotFound, s.call(t, o, http.MethodGet, rp, nil).status)
}

// testRBACMatrix checks every endpoint with every kind of caller (ADR-0022). Expected outcome per caller:
// allowed → 2xx; member or key of the org without the right → 403; outsider → 404.
func testRBACMatrix(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	callers := map[string]credential{
		"owner": w.owner.cred(), "admin": w.admin.cred(), "operator": w.operator.cred(), "viewer": w.viewer.cred(),
		"outsider": w.outsider.cred(), "key-read": key(w.readKey), "key-write": key(w.writeKey),
	}
	members := []string{"owner", "admin", "operator", "viewer"}
	managers := []string{"owner", "admin"}
	readers := append(slices.Clone(members), "key-read", "key-write")
	writers := []string{"owner", "admin", "operator", "key-write"}
	base := "/organizations/" + w.org
	type op struct {
		name    string
		allowed []string
		req     func() (method, path string, body any)
	}
	n := 0
	uniq := func() string { n++; return fmt.Sprintf("m%d", n) }
	ops := []op{
		{"getOrganization", readers, func() (string, string, any) { return http.MethodGet, base, nil }},
		{"updateOrganization", managers, func() (string, string, any) { return http.MethodPatch, base, map[string]any{"name": "World"} }},
		{"listMembers", members, func() (string, string, any) { return http.MethodGet, base + "/members", nil }},
		{"getMember", members, func() (string, string, any) { return http.MethodGet, base + "/members/" + w.ids["viewer"], nil }},
		{"updateMemberRole", managers, func() (string, string, any) {
			return http.MethodPatch, base + "/members/" + w.ids["viewer"], map[string]any{"role": "viewer"}
		}},
		{"listProjects", readers, func() (string, string, any) { return http.MethodGet, base + "/projects", nil }},
		{"getProject", readers, func() (string, string, any) { return http.MethodGet, base + "/projects/" + w.project, nil }},
		{"createProject", managers, func() (string, string, any) {
			id := uniq()
			return http.MethodPost, base + "/projects", map[string]any{"name": id, "slug": "p-" + id}
		}},
		{"updateProject", managers, func() (string, string, any) {
			return http.MethodPatch, base + "/projects/" + w.project, map[string]any{"description": "x"}
		}},
		{"deleteProject", managers, func() (string, string, any) {
			return http.MethodDelete, base + "/projects/" + w.newProject(t, "del-"+uniq()), nil
		}},
		{"listResources", readers, func() (string, string, any) {
			return http.MethodGet, base + "/projects/" + w.project + "/resources", nil
		}},
		{"getResource", readers, func() (string, string, any) {
			return http.MethodGet, base + "/projects/" + w.project + "/resources/" + w.newResource(t, "g-"+uniq()), nil
		}},
		{"createResource", writers, func() (string, string, any) {
			return http.MethodPost, base + "/projects/" + w.project + "/resources", map[string]any{"kind": "host", "name": "c-" + uniq()}
		}},
		{"updateResource", writers, func() (string, string, any) {
			return http.MethodPatch, base + "/projects/" + w.project + "/resources/" + w.newResource(t, "u-"+uniq()), map[string]any{"name": "u-" + uniq()}
		}},
		{"deleteResource", writers, func() (string, string, any) {
			return http.MethodDelete, base + "/projects/" + w.project + "/resources/" + w.newResource(t, "d-"+uniq()), nil
		}},
		{"listApiKeys", managers, func() (string, string, any) { return http.MethodGet, base + "/api-keys", nil }},
		{"createApiKey", managers, func() (string, string, any) {
			return http.MethodPost, base + "/api-keys", map[string]any{"project_id": w.project, "name": "x", "scopes": []string{"read"}}
		}},
		{"listAuditEvents", managers, func() (string, string, any) { return http.MethodGet, base + "/audit-events", nil }},
	}
	for _, o := range ops {
		t.Run(o.name, func(t *testing.T) {
			for _, who := range []string{"owner", "admin", "operator", "viewer", "outsider", "key-read", "key-write"} {
				method, path, body := o.req()
				r := s.call(t, callers[who], method, path, body)
				switch {
				case slices.Contains(o.allowed, who):
					require.True(t, r.status >= 200 && r.status < 300, "%s %s as %s: %d %s", method, path, who, r.status, r.body)
				case who == "outsider":
					require.Equal(t, http.StatusNotFound, r.status, "outsider on %s: %s", o.name, r.body)
				default:
					require.Equal(t, http.StatusForbidden, r.status, "%s on %s: %s", who, o.name, r.body)
				}
			}
		})
	}
}

// testCrossTenant: every endpoint of the outsider's own organization is unreachable with the world's
// credentials and vice versa; foreign ids yield 404 and list endpoints never show foreign rows.
func testCrossTenant(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	r := s.call(t, w.outsider.cred(), http.MethodGet, "/organizations", nil)
	var mine list[struct {
		Organization orgBody `json:"organization"`
	}]
	r.json(t, &mine)
	require.Len(t, mine.Items, 1)
	foreignOrg := mine.Items[0].Organization.ID
	r = s.call(t, w.outsider.cred(), http.MethodGet, "/organizations/"+foreignOrg+"/projects", nil)
	var fp list[projectBody]
	r.json(t, &fp)
	foreignProject := fp.Items[0].ID

	for _, c := range []credential{w.owner.cred(), key(w.readKey), key(w.writeKey)} {
		for _, path := range []string{
			"/organizations/" + foreignOrg, "/organizations/" + foreignOrg + "/members", "/organizations/" + foreignOrg + "/projects",
			"/organizations/" + foreignOrg + "/projects/" + foreignProject, "/organizations/" + foreignOrg + "/projects/" + foreignProject + "/resources",
			"/organizations/" + foreignOrg + "/api-keys", "/organizations/" + foreignOrg + "/audit-events",
			// Foreign project addressed through the caller's own organization.
			"/organizations/" + w.org + "/projects/" + foreignProject, "/organizations/" + w.org + "/projects/" + foreignProject + "/resources",
		} {
			got := s.call(t, c, http.MethodGet, path, nil)
			require.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, got.status, "GET %s: %d %s", path, got.status, got.body)
			if strings.HasPrefix(path, "/organizations/"+foreignOrg) && c.bearer != "" {
				require.Equal(t, http.StatusNotFound, got.status, "foreign organizations are indistinguishable from missing ones: %s", path)
			}
		}
		smuggle := s.call(t, c, http.MethodPost, "/organizations/"+foreignOrg+"/projects/"+foreignProject+"/resources", map[string]any{"kind": "host", "name": "smuggled"})
		require.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, smuggle.status)
	}
	r = s.call(t, w.owner.cred(), http.MethodGet, "/organizations", nil)
	require.NotContains(t, string(r.body), foreignOrg, "my organizations never include foreign ones")
}

func testContract(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	base := "/organizations/" + w.org
	r := s.call(t, w.owner.cred(), http.MethodPost, base+"/projects", map[string]any{"name": "", "slug": "Bad Slug"})
	require.Equal(t, http.StatusBadRequest, r.status)
	require.Equal(t, "validation_failed", r.problemCode(t))
	r = s.call(t, w.owner.cred(), http.MethodPost, base+"/projects", map[string]any{"name": "x", "slug": "okay", "admin": true})
	require.Equal(t, http.StatusBadRequest, r.status, "unknown properties are rejected")
	r = s.call(t, w.owner.cred(), http.MethodGet, "/organizations/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, r.status)
	r = s.call(t, w.owner.cred(), http.MethodGet, "/nope", nil)
	require.Equal(t, http.StatusNotFound, r.status)
	require.Equal(t, "not_found", r.problemCode(t))
	r = s.call(t, anonymous, http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, r.status)
	require.NotEmpty(t, r.header.Get("WWW-Authenticate"))
	r = s.call(t, credential{bearer: w.owner.token + "x"}, http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, r.status, "tampered tokens are rejected")
	r = s.call(t, credential{bearer: w.owner.token, apiKey: w.readKey}, http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, r.status, "ambiguous credentials are rejected")
	r = s.call(t, key(w.writeKey), http.MethodPost, "/organizations", map[string]any{"name": "via key"})
	require.Equal(t, http.StatusForbidden, r.status, "operations declared OIDC-only refuse API keys")
	r = s.call(t, key("ipk_"+"A"+fmt.Sprintf("%042d", 0)), http.MethodGet, base, nil)
	require.Equal(t, http.StatusUnauthorized, r.status)
	r = s.call(t, anonymous, http.MethodGet, "/health", nil)
	require.Equal(t, http.StatusOK, r.status)
	// CORS preflight for the dashboard origin only.
	r = s.call(t, anonymous, http.MethodOptions, base, nil, "Origin", "http://localhost:3000", "Access-Control-Request-Method", "PATCH")
	require.Equal(t, http.StatusNoContent, r.status)
	require.Equal(t, "http://localhost:3000", r.header.Get("Access-Control-Allow-Origin"))
	r = s.call(t, anonymous, http.MethodOptions, base, nil, "Origin", "https://evil.example", "Access-Control-Request-Method", "PATCH")
	require.Empty(t, r.header.Get("Access-Control-Allow-Origin"))
	// The edge mints its own request id.
	r = s.call(t, w.owner.cred(), http.MethodGet, base, nil, "X-Request-Id", "client-chosen")
	require.NotEqual(t, "client-chosen", r.header.Get("X-Request-Id"))
}

func testAudit(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	base := "/organizations/" + w.org
	extra := s.user(t, "audited")
	require.Equal(t, http.StatusOK, s.call(t, extra.cred(), http.MethodGet, "/organizations", nil).status)
	r := s.call(t, w.admin.cred(), http.MethodPost, base+"/members", map[string]any{"email": "audited@example.com", "role": "viewer"})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var m memberBody
	r.json(t, &m)
	require.Equal(t, http.StatusOK, s.call(t, w.admin.cred(), http.MethodPatch, base+"/members/"+m.UserID, map[string]any{"role": "operator"}).status)
	require.Equal(t, http.StatusNoContent, s.call(t, w.admin.cred(), http.MethodDelete, base+"/members/"+m.UserID, nil).status)

	r = s.call(t, w.owner.cred(), http.MethodGet, base+"/audit-events?limit=200", nil)
	require.Equal(t, http.StatusOK, r.status)
	var events list[struct {
		Action    string            `json:"action"`
		TargetID  string            `json:"target_id"`
		ActorID   string            `json:"actor_id"`
		Details   map[string]string `json:"details"`
		RequestID string            `json:"request_id"`
	}]
	r.json(t, &events)
	var actions []string
	for _, e := range events.Items {
		if e.TargetID == m.UserID {
			actions = append(actions, e.Action)
			require.Equal(t, w.ids["admin"], e.ActorID)
			require.NotEmpty(t, e.RequestID, "audit events carry the edge request id")
			if e.Action == "member.role_changed" {
				require.Equal(t, map[string]string{"old_role": "viewer", "new_role": "operator"}, e.Details)
			}
		}
	}
	require.Equal(t, []string{"member.removed", "member.role_changed", "member.added"}, actions)
}
