// Package pgtest starts a disposable PostgreSQL container for integration tests and provisions the
// three roles of ADR-0018 so tests exercise the real privilege model.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the pinned PostgreSQL image used by all integration tests (matches the compose core profile).
const Image = "postgres:17.6-alpine"

// Roles created in the test database (passwords are random per container).
const (
	MigratorRole = "svc_migrator"
	AppRole      = "svc_app"
	BatchRole    = "svc_batch"
	Database     = "svc"
)

// DB describes the running container.
type DB struct {
	container *tcpostgres.PostgresContainer
	// SuperuserURL connects as the bootstrap superuser.
	SuperuserURL string
	// MigratorURL connects as the owner role; AppURL as the runtime role; BatchURL as the cross-tenant role.
	MigratorURL, AppURL, BatchURL string
}

// Start launches the container and provisions roles. The container is terminated when the test ends.
func Start(t *testing.T) *DB {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	pw := "pw_" + t.Name()
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(Database),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err, "start postgres container (is Docker running?)")
	t.Cleanup(func() {
		require.NoError(t, testcontainers.TerminateContainer(c))
	})
	superURL, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	conn, err := pgx.Connect(ctx, superURL)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()
	stmts := []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`, MigratorRole, pw),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`, AppRole, pw),
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOCREATEDB NOCREATEROLE BYPASSRLS`, BatchRole, pw),
		`GRANT ALL ON SCHEMA public TO ` + MigratorRole,
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s, %s`, AppRole, BatchRole),
		`ALTER DATABASE ` + Database + ` OWNER TO ` + MigratorRole,
	}
	for _, s := range stmts {
		_, err := conn.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	return &DB{
		container:    c,
		SuperuserURL: superURL,
		MigratorURL:  withUser(t, superURL, MigratorRole, pw),
		AppURL:       withUser(t, superURL, AppRole, pw),
		BatchURL:     withUser(t, superURL, BatchRole, pw),
	}
}

func withUser(t *testing.T, base, user, pw string) string {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	u.User = url.UserPassword(user, pw)
	return u.String()
}

// Pause freezes the container (connections hang until they time out) — simulates dependency loss
// without changing the published port. Unpause resumes it.
func (d *DB) Pause(t *testing.T) {
	t.Helper()
	d.docker(t, "pause")
}

// Unpause resumes a paused container.
func (d *DB) Unpause(t *testing.T) {
	t.Helper()
	d.docker(t, "unpause")
}

func (d *DB) docker(t *testing.T, verb string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", verb, d.container.GetContainerID()).CombinedOutput()
	require.NoError(t, err, "docker %s: %s", verb, out)
}
