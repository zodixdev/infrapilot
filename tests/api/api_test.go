//go:build integration

package api_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type orgBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type projectBody struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type resourceBody struct {
	ID     string            `json:"id"`
	Name   string            `json:"name"`
	Kind   string            `json:"kind"`
	Labels map[string]string `json:"labels"`
}

type keyBody struct {
	APIKey struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
	} `json:"api_key"`
	Secret string `json:"secret"`
}

type list[T any] struct {
	Items []T `json:"items"`
	Page  struct {
		HasMore    bool    `json:"has_more"`
		NextCursor *string `json:"next_cursor"`
	} `json:"page"`
}

// fixture creates an organization owned by u with one project; returns their ids.
func (s *stack) fixture(t *testing.T, u user, name string) (org, project string) {
	t.Helper()
	r := s.call(t, u.cred(), http.MethodPost, "/organizations", map[string]any{"name": name})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var o orgBody
	r.json(t, &o)
	r = s.call(t, u.cred(), http.MethodPost, "/organizations/"+o.ID+"/projects", map[string]any{"name": "Web", "slug": "web"})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var p projectBody
	r.json(t, &p)
	return o.ID, p.ID
}

func TestAPI_HappyPath_Integration(t *testing.T) {
	s := startStack(t, stackOptions{})
	alice := s.user(t, "alice")
	org, project := s.fixture(t, alice, "Acme")

	r := s.call(t, alice.cred(), http.MethodGet, "/organizations", nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	var mine list[struct {
		Organization orgBody `json:"organization"`
		Role         string  `json:"role"`
	}]
	r.json(t, &mine)
	require.Len(t, mine.Items, 1)
	require.Equal(t, "owner", mine.Items[0].Role)
	require.NotEmpty(t, r.header.Get("X-Request-Id"))
	require.NotEmpty(t, r.header.Get("RateLimit-Remaining"))

	r = s.call(t, alice.cred(), http.MethodPost, "/organizations/"+org+"/projects/"+project+"/resources",
		map[string]any{"kind": "host", "name": "db-1", "labels": map[string]string{"env": "prod"}})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))

	r = s.call(t, alice.cred(), http.MethodPost, "/organizations/"+org+"/api-keys",
		map[string]any{"project_id": project, "name": "ci", "scopes": []string{"read"}})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	var k keyBody
	r.json(t, &k)
	require.NotEmpty(t, k.Secret)

	r = s.call(t, key(k.Secret), http.MethodGet, "/organizations/"+org+"/projects/"+project+"/resources", nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	var res list[resourceBody]
	r.json(t, &res)
	require.Len(t, res.Items, 1)
	require.Equal(t, map[string]string{"env": "prod"}, res.Items[0].Labels)

	idem := []string{"Idempotency-Key", "create-1"}
	r1 := s.call(t, alice.cred(), http.MethodPost, "/organizations/"+org+"/projects", map[string]any{"name": "API", "slug": "api"}, idem...)
	r2 := s.call(t, alice.cred(), http.MethodPost, "/organizations/"+org+"/projects", map[string]any{"name": "API", "slug": "api"}, idem...)
	require.Equal(t, http.StatusCreated, r1.status, string(r1.body))
	require.Equal(t, http.StatusCreated, r2.status, string(r2.body))
	var p1, p2 projectBody
	r1.json(t, &p1)
	r2.json(t, &p2)
	require.Equal(t, p1.ID, p2.ID)
	r = s.call(t, alice.cred(), http.MethodPost, "/organizations/"+org+"/projects", map[string]any{"name": "Other", "slug": "other"}, idem...)
	require.Equal(t, http.StatusUnprocessableEntity, r.status, string(r.body))
	require.Equal(t, "idempotency_key_reused", r.problemCode(t))
}
