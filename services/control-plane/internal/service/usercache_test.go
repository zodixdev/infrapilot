package service

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestUserCache(t *testing.T) {
	t.Parallel()
	c := newUserCache(10, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	p := Principal{Subject: "s1", Email: "a@example.com", DisplayName: "A", EmailVerified: true}
	id := uuid.New()
	c.put(p, id, now)

	got, ok := c.get(p, now.Add(30*time.Second))
	require.True(t, ok)
	require.Equal(t, id, got)

	_, ok = c.get(p, now.Add(2*time.Minute))
	require.False(t, ok, "entries expire so last-seen and claims reach the database")

	for name, changed := range map[string]Principal{
		"email":    {Subject: "s1", Email: "b@example.com", DisplayName: "A", EmailVerified: true},
		"name":     {Subject: "s1", Email: "a@example.com", DisplayName: "B", EmailVerified: true},
		"verified": {Subject: "s1", Email: "a@example.com", DisplayName: "A", EmailVerified: false},
	} {
		_, ok = c.get(changed, now)
		require.False(t, ok, "a changed %s claim forces an upsert", name)
	}

	for i := range 20 {
		c.put(Principal{Subject: "u" + strconv.Itoa(i)}, uuid.New(), now)
	}
	require.LessOrEqual(t, len(c.entries), 10, "bounded")
	require.GreaterOrEqual(t, len(c.entries), 5, "eviction drops a fraction, not everything")
}
