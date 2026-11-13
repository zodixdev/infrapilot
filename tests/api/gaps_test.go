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

// TestAPI_Boundaries_Integration covers the edges of the functional suite: the operations not in its RBAC matrix,
// mutating calls with foreign ids, foreign page tokens, audit of keys and projects through the API, the edge
// permission cache never outliving a demotion, and Idempotency-Key replay across a gateway restart.
func TestAPI_Boundaries_Integration(t *testing.T) {
	s := startStack(t, stackOptions{})
	w := newWorld(t, s)
	t.Run("RBACMatrixRemainingOperations", func(t *testing.T) { testRBACRemaining(t, w) })
	t.Run("CrossTenantMutations", func(t *testing.T) { testCrossTenantMutations(t, w) })
	t.Run("ForeignPageToken", func(t *testing.T) { testForeignPageToken(t, w) })
	t.Run("AuditOfKeysAndProjects", func(t *testing.T) { testAuditKeysProjects(t, w) })
	t.Run("EdgeCacheNeverOutlivesDemotion", func(t *testing.T) { testEdgeCacheDemotion(t, w) })
	t.Run("IdempotencyAcrossGatewayRestart", func(t *testing.T) { testIdempotencyRestart(t, w) }) // last: restarts the gateway
}

func (w *world) callers() map[string]credential {
	return map[string]credential{
		"owner": w.owner.cred(), "admin": w.admin.cred(), "operator": w.operator.cred(), "viewer": w.viewer.cred(),
		"outsider": w.outsider.cred(), "key-read": key(w.readKey), "key-write": key(w.writeKey),
	}
}

// expectAccess asserts the ADR-0022 outcome: allowed → 2xx; in-org caller without the right → 403; outsider → 404.
func expectAccess(t *testing.T, who string, allowed bool, r response, what string) {
	t.Helper()
	switch {
	case allowed:
		require.True(t, r.status >= 200 && r.status < 300, "%s as %s: %d %s", what, who, r.status, r.body)
	case who == "outsider":
		require.Equal(t, http.StatusNotFound, r.status, "%s as outsider: %s", what, r.body)
	default:
		require.Equal(t, http.StatusForbidden, r.status, "%s as %s: %s", what, who, r.body)
	}
}

// testRBACRemaining: addMember, removeMember, getApiKey, revokeApiKey and deleteOrganization for every caller.
// Denied callers go first so a wrongly allowed call would be visible as a missing target for the allowed ones.
func testRBACRemaining(t *testing.T, w *world) {
	t.Helper()
	s, base := w.s, "/organizations/"+w.org
	callers := w.callers()
	all := []string{"owner", "admin", "operator", "viewer", "outsider", "key-read", "key-write"}
	managers := []string{"owner", "admin"}
	ordered := func(allowed []string) []string { // denied callers first
		out := slices.DeleteFunc(slices.Clone(all), func(c string) bool { return slices.Contains(allowed, c) })
		return append(out, allowed...)
	}

	t.Run("addMember+removeMember", func(t *testing.T) {
		for _, who := range ordered(managers) {
			name := "cand-" + who
			cand := s.user(t, name)
			require.Equal(t, http.StatusOK, s.call(t, cand.cred(), http.MethodGet, "/organizations", nil).status)
			r := s.call(t, callers[who], http.MethodPost, base+"/members", map[string]any{"email": name + "@example.com", "role": "viewer"})
			allowed := slices.Contains(managers, who)
			expectAccess(t, who, allowed, r, "addMember")
			if !allowed { // added by the owner so the removal can be attempted by this caller
				r = s.call(t, w.owner.cred(), http.MethodPost, base+"/members", map[string]any{"email": name + "@example.com", "role": "viewer"})
				require.Equal(t, http.StatusCreated, r.status, string(r.body))
			}
			var m memberBody
			r.json(t, &m)
			r = s.call(t, callers[who], http.MethodDelete, base+"/members/"+m.UserID, nil)
			expectAccess(t, who, allowed, r, "removeMember")
			got := s.call(t, w.owner.cred(), http.MethodGet, base+"/members/"+m.UserID, nil).status
			if allowed {
				require.Equal(t, http.StatusNotFound, got)
			} else {
				require.Equal(t, http.StatusOK, got, "a denied removal leaves the membership intact")
			}
		}
	})

	t.Run("getApiKey+revokeApiKey", func(t *testing.T) {
		for _, who := range ordered(managers) {
			r := s.call(t, w.owner.cred(), http.MethodPost, base+"/api-keys", map[string]any{"project_id": w.project, "name": "t-" + who, "scopes": []string{"read"}})
			require.Equal(t, http.StatusCreated, r.status, string(r.body))
			var k keyBody
			r.json(t, &k)
			allowed := slices.Contains(managers, who)
			g := s.call(t, callers[who], http.MethodGet, base+"/api-keys/"+k.APIKey.ID, nil)
			expectAccess(t, who, allowed, g, "getApiKey")
			require.NotContains(t, string(g.body), k.Secret, "the secret is shown only once")
			expectAccess(t, who, allowed, s.call(t, callers[who], http.MethodPost, base+"/api-keys/"+k.APIKey.ID+"/revoke", nil), "revokeApiKey")
			works := s.call(t, key(k.Secret), http.MethodGet, base+"/projects/"+w.project, nil).status == http.StatusOK
			require.Equal(t, !allowed, works, "after %s's revoke attempt the key works=%v", who, works)
		}
	})

	t.Run("deleteOrganization", func(t *testing.T) {
		org, project := s.fixture(t, w.owner, "Doomed")
		ob := "/organizations/" + org
		local := map[string]credential{"owner": w.owner.cred(), "outsider": w.outsider.cred()}
		for _, m := range []struct {
			who  string
			u    user
			role string
		}{{"admin", w.admin, "admin"}, {"operator", w.operator, "operator"}, {"viewer", w.viewer, "viewer"}} {
			r := s.call(t, w.owner.cred(), http.MethodPost, ob+"/members", map[string]any{"email": m.who + "@example.com", "role": m.role})
			require.Equal(t, http.StatusCreated, r.status, string(r.body))
			local[m.who] = m.u.cred()
		}
		for _, sc := range []string{"read", "write"} {
			r := s.call(t, w.owner.cred(), http.MethodPost, ob+"/api-keys", map[string]any{"project_id": project, "name": sc, "scopes": []string{sc}})
			require.Equal(t, http.StatusCreated, r.status, string(r.body))
			var k keyBody
			r.json(t, &k)
			local["key-"+sc] = key(k.Secret)
		}
		for _, who := range ordered([]string{"owner"}) {
			expectAccess(t, who, who == "owner", s.call(t, local[who], http.MethodDelete, ob, nil), "deleteOrganization")
			if who != "owner" {
				require.Equal(t, http.StatusOK, s.call(t, w.owner.cred(), http.MethodGet, ob, nil).status, "denied delete by %s left the org", who)
			}
		}
		require.Equal(t, http.StatusNotFound, s.call(t, w.owner.cred(), http.MethodGet, ob, nil).status)
		require.NotEqual(t, http.StatusOK, s.call(t, local["key-read"], http.MethodGet, ob+"/projects/"+project, nil).status,
			"keys of a deleted organization stop working")
	})
}

// foreign is the outsider's organization with one object of every kind.
type foreign struct{ org, project, resource, keyID, keySecret, owner string }

func (w *world) foreignWorld(t *testing.T) foreign {
	t.Helper()
	s, o := w.s, w.outsider.cred()
	r := s.call(t, o, http.MethodGet, "/organizations", nil)
	var mine list[struct {
		Organization orgBody `json:"organization"`
	}]
	r.json(t, &mine)
	require.Len(t, mine.Items, 1)
	f := foreign{org: mine.Items[0].Organization.ID}
	fb := "/organizations/" + f.org
	r = s.call(t, o, http.MethodGet, fb+"/projects", nil)
	var ps list[projectBody]
	r.json(t, &ps)
	f.project = ps.Items[0].ID
	r = s.call(t, o, http.MethodPost, fb+"/projects/"+f.project+"/resources", map[string]any{"kind": "host", "name": "foreign-" + randomHex(t, 4)})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var res resourceBody
	r.json(t, &res)
	f.resource = res.ID
	r = s.call(t, o, http.MethodPost, fb+"/api-keys", map[string]any{"project_id": f.project, "name": "f", "scopes": []string{"read"}})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var k keyBody
	r.json(t, &k)
	f.keyID, f.keySecret = k.APIKey.ID, k.Secret
	r = s.call(t, o, http.MethodGet, fb+"/members", nil)
	var ms list[memberBody]
	r.json(t, &ms)
	require.Len(t, ms.Items, 1)
	f.owner = ms.Items[0].UserID
	return f
}

// testCrossTenantMutations: every mutating endpoint, addressed with foreign ids either under the foreign
// organization or smuggled under the caller's own one, is refused and changes nothing.
func testCrossTenantMutations(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	f := w.foreignWorld(t)
	fb, mb := "/organizations/"+f.org, "/organizations/"+w.org
	type req struct {
		method, path string
		body         any
	}
	reqs := []req{
		{http.MethodPatch, fb, map[string]any{"name": "pwned"}},
		{http.MethodPost, fb + "/members", map[string]any{"email": "owner@example.com", "role": "owner"}},
		{http.MethodPatch, fb + "/members/" + f.owner, map[string]any{"role": "viewer"}},
		{http.MethodDelete, fb + "/members/" + f.owner, nil},
		{http.MethodPost, fb + "/projects", map[string]any{"name": "x", "slug": "smuggled"}},
		{http.MethodPatch, fb + "/projects/" + f.project, map[string]any{"name": "pwned"}},
		{http.MethodPatch, fb + "/projects/" + f.project + "/resources/" + f.resource, map[string]any{"name": "pwned"}},
		{http.MethodGet, fb + "/projects/" + f.project + "/resources/" + f.resource, nil},
		{http.MethodGet, fb + "/api-keys/" + f.keyID, nil},
		{http.MethodPost, fb + "/api-keys/" + f.keyID + "/revoke", nil},
		{http.MethodPost, fb + "/api-keys", map[string]any{"project_id": f.project, "name": "x", "scopes": []string{"write"}}},
		// Foreign children addressed through the caller's own organization or project.
		{http.MethodPatch, mb + "/projects/" + f.project, map[string]any{"name": "pwned"}},
		{http.MethodPatch, mb + "/projects/" + f.project + "/resources/" + f.resource, map[string]any{"name": "pwned"}},
		{http.MethodPatch, mb + "/projects/" + w.project + "/resources/" + f.resource, map[string]any{"name": "pwned"}},
		{http.MethodGet, mb + "/projects/" + w.project + "/resources/" + f.resource, nil},
		{http.MethodPatch, mb + "/members/" + f.owner, map[string]any{"role": "viewer"}},
		{http.MethodGet, mb + "/members/" + f.owner, nil},
		{http.MethodGet, mb + "/api-keys/" + f.keyID, nil},
		{http.MethodPost, mb + "/api-keys/" + f.keyID + "/revoke", nil},
		{http.MethodPost, mb + "/api-keys", map[string]any{"project_id": f.project, "name": "x", "scopes": []string{"write"}}},
		{http.MethodPost, mb + "/projects/" + f.project + "/resources", map[string]any{"kind": "host", "name": "smuggled"}},
		// Deletes last: if one wrongly succeeded, the verification below shows it.
		{http.MethodDelete, mb + "/projects/" + w.project + "/resources/" + f.resource, nil},
		{http.MethodDelete, mb + "/projects/" + f.project + "/resources/" + f.resource, nil},
		{http.MethodDelete, mb + "/members/" + f.owner, nil},
		{http.MethodDelete, mb + "/projects/" + f.project, nil},
		{http.MethodDelete, fb + "/projects/" + f.project + "/resources/" + f.resource, nil},
		{http.MethodDelete, fb + "/projects/" + f.project, nil},
		{http.MethodDelete, fb, nil},
	}
	for name, c := range map[string]credential{"owner": w.owner.cred(), "admin": w.admin.cred(), "key-write": key(w.writeKey)} {
		for _, q := range reqs {
			r := s.call(t, c, q.method, q.path, q.body)
			require.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, r.status, "%s %s as %s: %d %s", q.method, q.path, name, r.status, r.body)
			require.NotContains(t, string(r.body), "pwned")
			if c.bearer != "" && strings.HasPrefix(q.path, fb) {
				require.Equal(t, http.StatusNotFound, r.status, "foreign organizations look missing to users: %s %s", q.method, q.path)
			}
		}
	}

	// Nothing of the foreign organization changed.
	o := w.outsider.cred()
	r := s.call(t, o, http.MethodGet, fb, nil)
	require.Equal(t, http.StatusOK, r.status)
	var org orgBody
	r.json(t, &org)
	require.Equal(t, "Elsewhere", org.Name)
	r = s.call(t, o, http.MethodGet, fb+"/projects/"+f.project+"/resources/"+f.resource, nil)
	require.Equal(t, http.StatusOK, r.status, "foreign resource survives")
	var res resourceBody
	r.json(t, &res)
	require.True(t, strings.HasPrefix(res.Name, "foreign-"))
	r = s.call(t, o, http.MethodGet, fb+"/members", nil)
	var ms list[memberBody]
	r.json(t, &ms)
	require.Len(t, ms.Items, 1, "no one joined the foreign organization")
	require.Equal(t, "owner", ms.Items[0].Role)
	require.Equal(t, http.StatusOK, s.call(t, key(f.keySecret), http.MethodGet, fb+"/projects/"+f.project, nil).status, "foreign key not revoked")
	r = s.call(t, o, http.MethodGet, fb+"/projects", nil)
	var ps list[projectBody]
	r.json(t, &ps)
	require.Len(t, ps.Items, 1, "no project was smuggled into the foreign organization")
	r = s.call(t, o, http.MethodGet, fb+"/api-keys", nil)
	var ks list[struct{ ID string }]
	r.json(t, &ks)
	require.Len(t, ks.Items, 1, "no key was minted for the foreign project")
}

// testForeignPageToken: a page token taken from another organization's list never yields its rows.
func testForeignPageToken(t *testing.T, w *world) {
	t.Helper()
	s := w.s
	f := w.foreignWorld(t)
	fb := "/organizations/" + f.org + "/projects/" + f.project + "/resources"
	foreignIDs := map[string]bool{}
	for i := range 3 {
		r := s.call(t, w.outsider.cred(), http.MethodPost, fb, map[string]any{"kind": "host", "name": fmt.Sprintf("tok-%d", i)})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
	}
	r := s.call(t, w.outsider.cred(), http.MethodGet, fb+"?limit=1", nil)
	var fl list[resourceBody]
	r.json(t, &fl)
	require.True(t, fl.Page.HasMore)
	r = s.call(t, w.outsider.cred(), http.MethodGet, fb+"?limit=100", nil)
	var all list[resourceBody]
	r.json(t, &all)
	for _, x := range all.Items {
		foreignIDs[x.ID] = true
	}

	mine := "/organizations/" + w.org + "/projects/" + w.project + "/resources?limit=1&cursor=" + *fl.Page.NextCursor
	for _, c := range []credential{w.owner.cred(), key(w.readKey)} {
		r = s.call(t, c, http.MethodGet, mine, nil)
		require.Contains(t, []int{http.StatusOK, http.StatusBadRequest}, r.status, string(r.body))
		var got list[resourceBody]
		r.json(t, &got)
		for _, x := range got.Items {
			require.False(t, foreignIDs[x.ID], "foreign resource %s leaked through a foreign page token", x.ID)
		}
	}
	// The token does not open the foreign list to a non-member either.
	r = s.call(t, w.owner.cred(), http.MethodGet, fb+"?limit=1&cursor="+*fl.Page.NextCursor, nil)
	require.Equal(t, http.StatusNotFound, r.status)
}

// testAuditKeysProjects: key and project lifecycle events appear in the API's audit trail with actor and request id.
func testAuditKeysProjects(t *testing.T, w *world) {
	t.Helper()
	s, base := w.s, "/organizations/"+w.org
	r := s.call(t, w.admin.cred(), http.MethodPost, base+"/projects", map[string]any{"name": "Audited", "slug": "audited"})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var p projectBody
	r.json(t, &p)
	r = s.call(t, w.admin.cred(), http.MethodPost, base+"/api-keys", map[string]any{"project_id": p.ID, "name": "a", "scopes": []string{"read"}})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var k keyBody
	r.json(t, &k)
	require.Equal(t, http.StatusOK, s.call(t, w.admin.cred(), http.MethodPost, base+"/api-keys/"+k.APIKey.ID+"/revoke", nil).status)
	require.Equal(t, http.StatusNoContent, s.call(t, w.admin.cred(), http.MethodDelete, base+"/projects/"+p.ID, nil).status)

	r = s.call(t, w.owner.cred(), http.MethodGet, base+"/audit-events?limit=200", nil)
	require.Equal(t, http.StatusOK, r.status)
	require.NotContains(t, string(r.body), k.Secret, "the audit trail never carries key secrets")
	var events list[struct {
		Action    string `json:"action"`
		TargetID  string `json:"target_id"`
		ActorID   string `json:"actor_id"`
		RequestID string `json:"request_id"`
	}]
	r.json(t, &events)
	got := map[string]int{}
	for _, e := range events.Items {
		if e.TargetID != p.ID && e.TargetID != k.APIKey.ID {
			continue
		}
		got[e.Action]++
		require.Equal(t, w.ids["admin"], e.ActorID, e.Action)
		require.NotEmpty(t, e.RequestID, e.Action)
	}
	require.Equal(t, map[string]int{"project.created": 1, "apikey.created": 1, "apikey.revoked": 1, "project.deleted": 1}, got)
}

// testEdgeCacheDemotion: the edge caches "allowed" decisions, but a demoted member loses the right on the very
// next call because the owning service re-authorizes every request.
func testEdgeCacheDemotion(t *testing.T, w *world) {
	t.Helper()
	s, base := w.s, "/organizations/"+w.org
	d := s.user(t, "demoted")
	require.Equal(t, http.StatusOK, s.call(t, d.cred(), http.MethodGet, "/organizations", nil).status)
	r := s.call(t, w.owner.cred(), http.MethodPost, base+"/members", map[string]any{"email": "demoted@example.com", "role": "admin"})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var m memberBody
	r.json(t, &m)
	upd := map[string]any{"description": "by demoted"}
	path := base + "/projects/" + w.project
	require.Equal(t, http.StatusOK, s.call(t, d.cred(), http.MethodPatch, path, upd).status, "warms the edge decision cache")

	require.Equal(t, http.StatusOK, s.call(t, w.owner.cred(), http.MethodPatch, base+"/members/"+m.UserID, map[string]any{"role": "viewer"}).status)
	require.Equal(t, http.StatusForbidden, s.call(t, d.cred(), http.MethodPatch, path, upd).status, "demotion is effective immediately")

	require.Equal(t, http.StatusNoContent, s.call(t, w.owner.cred(), http.MethodDelete, base+"/members/"+m.UserID, nil).status)
	got := s.call(t, d.cred(), http.MethodGet, path, nil).status
	require.Contains(t, []int{http.StatusNotFound, http.StatusForbidden}, got, "removal is effective immediately")
}

// testIdempotencyRestart: the idempotency record lives with the owning service, so a replay after the gateway
// restarts still returns the original resource and a mismatched replay is still refused.
func testIdempotencyRestart(t *testing.T, w *world) {
	t.Helper()
	s, path := w.s, "/organizations/"+w.org+"/projects"
	h := []string{"Idempotency-Key", "restart-" + randomHex(t, 4)}
	body := map[string]any{"name": "Durable", "slug": "durable"}
	r1 := s.call(t, w.owner.cred(), http.MethodPost, path, body, h...)
	require.Equal(t, http.StatusCreated, r1.status, string(r1.body))
	s.restartGateway(t)
	r2 := s.call(t, w.owner.cred(), http.MethodPost, path, body, h...)
	require.Equal(t, http.StatusCreated, r2.status, string(r2.body))
	var p1, p2 projectBody
	r1.json(t, &p1)
	r2.json(t, &p2)
	require.Equal(t, p1.ID, p2.ID, "replay after a gateway restart returns the original project")
	r3 := s.call(t, w.owner.cred(), http.MethodPost, path, map[string]any{"name": "Other", "slug": "other-durable"}, h...)
	require.Equal(t, http.StatusUnprocessableEntity, r3.status, string(r3.body))
	require.Equal(t, "idempotency_key_reused", r3.problemCode(t))
}
