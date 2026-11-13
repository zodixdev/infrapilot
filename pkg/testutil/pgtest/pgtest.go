// Package pgtest starts a disposable PostgreSQL container for integration tests and provisions the
// three roles of ADR-0018 exactly as production provisioning does, so tests exercise the real
// privilege model.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Roles created in the test database.
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
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase(Database),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword(randomSecret(t)),
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

	pw := map[string]string{MigratorRole: randomSecret(t), AppRole: randomSecret(t), BatchRole: randomSecret(t)}
	_, err = conn.Exec(ctx, `SET log_min_error_statement = panic`) // never log provisioning statements
	require.NoError(t, err)
	for _, role := range []struct {
		name   string
		bypass bool
	}{{MigratorRole, false}, {AppRole, false}, {BatchRole, true}} {
		attrs := "NOBYPASSRLS"
		if role.bypass {
			attrs = "BYPASSRLS"
		}
		ident := pgx.Identifier{role.name}.Sanitize()
		_, err = conn.Exec(ctx, `CREATE ROLE `+ident+` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE `+attrs)
		require.NoError(t, err)
		// PASSWORD does not accept bind parameters; the value is random hex, quoted defensively.
		_, err = conn.Exec(ctx, `ALTER ROLE `+ident+` PASSWORD `+quoteLiteral(pw[role.name]))
		require.NoError(t, err)
	}
	// The maintenance database is not a service database: nobody but the superuser connects to it.
	_, err = conn.Exec(ctx, `REVOKE CONNECT ON DATABASE postgres FROM PUBLIC`)
	require.NoError(t, err)
	for _, s := range append(ProvisioningSQL(Database, MigratorRole, AppRole, BatchRole), SchemaSQL(MigratorRole, AppRole, BatchRole)...) {
		_, err := conn.Exec(ctx, s)
		require.NoError(t, err, s)
	}
	return &DB{
		container:    c,
		SuperuserURL: superURL,
		MigratorURL:  withUser(t, superURL, MigratorRole, pw[MigratorRole]),
		AppURL:       withUser(t, superURL, AppRole, pw[AppRole]),
		BatchURL:     withUser(t, superURL, BatchRole, pw[BatchRole]),
	}
}

// ProvisioningSQL is the per-database privilege setup of ADR-0018, shared with the compose init
// script's intent: database-level PUBLIC privileges revoked, schema owned by the migrator, server-side
// timeouts as role defaults. Default privileges are NOT granted here: every migration grants its own
// tables explicitly, so all environments behave the same.
func ProvisioningSQL(db, migrator, app, batch string) []string {
	d, m, a, b := pgx.Identifier{db}.Sanitize(), pgx.Identifier{migrator}.Sanitize(), pgx.Identifier{app}.Sanitize(), pgx.Identifier{batch}.Sanitize()
	return []string{
		`REVOKE ALL ON DATABASE ` + d + ` FROM PUBLIC`,
		`ALTER DATABASE ` + d + ` OWNER TO ` + m,
		`GRANT CONNECT ON DATABASE ` + d + ` TO ` + m + `, ` + a + `, ` + b,
		`ALTER ROLE ` + a + ` SET statement_timeout = '30s'`,
		`ALTER ROLE ` + a + ` SET lock_timeout = '5s'`,
		`ALTER ROLE ` + a + ` SET idle_in_transaction_session_timeout = '60s'`,
		`ALTER ROLE ` + b + ` SET statement_timeout = '15min'`,
		`ALTER ROLE ` + b + ` SET idle_in_transaction_session_timeout = '5min'`,
		`ALTER ROLE ` + m + ` SET lock_timeout = '3s'`,
	}
}

// SchemaSQL is run inside the service database (as superuser) after ProvisioningSQL.
func SchemaSQL(migrator, app, batch string) []string {
	m, a, b := pgx.Identifier{migrator}.Sanitize(), pgx.Identifier{app}.Sanitize(), pgx.Identifier{batch}.Sanitize()
	return []string{
		`REVOKE ALL ON SCHEMA public FROM PUBLIC`,
		`ALTER SCHEMA public OWNER TO ` + m,
		`GRANT USAGE ON SCHEMA public TO ` + a + `, ` + b,
	}
}

func randomSecret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 18)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func quoteLiteral(s string) string {
	out := []byte{'\''}
	for i := range len(s) {
		if s[i] == '\'' {
			out = append(out, '\'')
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
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
