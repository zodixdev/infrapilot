package postgres_test

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// testcontainers keeps a reaper/docker client goroutine for the process lifetime (integration runs).
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
	)
}
