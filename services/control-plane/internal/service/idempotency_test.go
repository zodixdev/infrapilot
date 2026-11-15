package service

import (
	"crypto/sha256"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/zodixdev/infrapilot/services/control-plane/internal/domain"
)

// Live records can be replayed across a deployment only if the digest of the same input stays stable.
func TestIdempotencyFingerprintCompatibility(t *testing.T) {
	t.Parallel()
	const projectID = "0192f7a0-3c1e-7c9a-9d41-9a1f0f3b2c22"
	for _, tc := range []struct {
		operation string
		input     any
		wire      string
	}{
		{"CreateOrganization", CreateOrganizationInput{Name: "Example"}, `{"name":"Example"}`},
		{"CreateProject", CreateProjectInput{Name: "Web", Slug: "web"}, `{"name":"Web","slug":"web","description":""}`},
		{
			"CreateResource:" + projectID,
			CreateResourceInput{Kind: domain.KindHost, Name: "edge", Labels: map[string]string{"zone": "east", "env": "test"}},
			`{"kind":"host","name":"edge","labels":{"env":"test","zone":"east"}}`,
		},
		{
			"CreateAPIKey",
			CreateAPIKeyInput{ProjectID: uuid.MustParse(projectID), Name: "automation", Scopes: []domain.Scope{domain.ScopeRead}},
			`{"project_id":"` + projectID + `","name":"automation","scopes":["read"],"expires_at":null}`,
		},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			t.Parallel()
			ctx := WithRequestMeta(t.Context(), RequestMeta{IdempotencyKey: "test-key"})
			previous := sha256.Sum256([]byte(tc.operation + "\x00" + tc.wire))
			current, err := newIdempotency(ctx, tc.operation, tc.input)
			require.NoError(t, err)
			require.Equal(t, previous[:], current.hash)
		})
	}
}
