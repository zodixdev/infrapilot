//go:build integration

package grpc_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/pkg/config"
	"github.com/zodixdev/infrapilot/pkg/grpcclient"
	"github.com/zodixdev/infrapilot/pkg/grpcmeta"
	"github.com/zodixdev/infrapilot/pkg/grpcserver"
	"github.com/zodixdev/infrapilot/pkg/testutil/certtest"
	"github.com/zodixdev/infrapilot/pkg/testutil/pgtest"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/service"
	"github.com/zodixdev/infrapilot/services/control-plane/internal/testdb"
	cpgrpc "github.com/zodixdev/infrapilot/services/control-plane/internal/transport/grpc"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// testcontainers' reaper and pgx pools of other tests' containers are closed in t.Cleanup
		goleak.IgnoreTopFunction("github.com/testcontainers/testcontainers-go.(*Reaper).connect.func1"),
		goleak.IgnoreAnyFunction("github.com/testcontainers/testcontainers-go/internal/core.(*dockerClient).Close"))
}

var (
	gatewayID   = certtest.SPIFFE("infrapilot", "api-gateway")
	workerID    = certtest.SPIFFE("infrapilot", "check-runner")
	ingestionID = certtest.SPIFFE("infrapilot", "telemetry-ingestion")
	serverID    = certtest.SPIFFE("infrapilot", "control-plane")
)

type harness struct {
	db                         *testdb.DB
	gateway, worker, ingestion *grpc.ClientConn
}

func start(t *testing.T, opts ...pgtest.Option) *harness {
	t.Helper()
	d := testdb.Start(t, opts...)
	logger := slog.New(slog.DiscardHandler)
	svc, err := service.New(d.App)
	require.NoError(t, err)
	ca := certtest.NewCA(t)
	srvCert := ca.Issue("control-plane", serverID)
	srv, err := grpcserver.New(t.Context(), grpcserver.Config{
		Addr: "127.0.0.1:0", MaxRecvMsgBytes: 1 << 20, ConnectionTimeout: 5 * time.Second, KeepaliveTime: 30 * time.Second,
		KeepaliveTimeout: 10 * time.Second, MaxConcurrentStreams: 64,
		TLSCertFile: srvCert.Cert, TLSKeyFile: srvCert.Key, TLSClientCAFile: ca.File,
	}, grpcserver.WithPeerPolicy(grpcserver.PeerPolicy{Rules: cpgrpc.PeerRules([]string{gatewayID}, []string{ingestionID})}),
		grpcserver.WithUnaryInterceptors(cpgrpc.PrincipalInterceptor(svc, []string{gatewayID}, logger)))
	require.NoError(t, err)
	cpgrpc.NewServer(svc, logger).Register(srv.Registrar())
	srv.SetServing(true)
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(context.WithoutCancel(t.Context())) }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context())))
		require.NoError(t, <-runErr)
	})
	dial := func(name, id string) *grpc.ClientConn {
		f := ca.Issue(name, id)
		conn, err := grpcclient.New(config.GRPCClient{Target: srv.Addr().String(), ServerName: "localhost", ServerIdentity: serverID,
			TLSCertFile: f.Cert, TLSKeyFile: f.Key, TLSCAFile: ca.File, Timeout: 3 * time.Second}, config.EnvTest)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	return &harness{
		db: d, gateway: dial("gateway", gatewayID), worker: dial("worker", workerID),
		ingestion: dial("ingestion", ingestionID),
	}
}

func asUser(ctx context.Context, t *testing.T, subject string) context.Context {
	t.Helper()
	b, err := proto.Marshal(&cpv1.Principal{Kind: cpv1.PrincipalKind_PRINCIPAL_KIND_USER, Subject: subject, Email: subject + "@example.com"})
	require.NoError(t, err)
	return metadata.AppendToOutgoingContext(ctx, grpcmeta.Principal, string(b), grpcmeta.RequestID, "req-"+subject)
}

func TestGRPC_EndToEnd_Integration(t *testing.T) {
	h := start(t)
	ctx := asUser(t.Context(), t, "alice")
	orgs := cpv1.NewOrganizationServiceClient(h.gateway)
	projects := cpv1.NewProjectServiceClient(h.gateway)

	org, err := orgs.CreateOrganization(ctx, &cpv1.CreateOrganizationRequest{Name: "Acme"})
	require.NoError(t, err)
	orgID := org.GetOrganization().GetId()

	t.Run("validation errors carry field violations", func(t *testing.T) {
		_, err := projects.CreateProject(ctx, &cpv1.CreateProjectRequest{OrganizationId: orgID, Name: "", Slug: "Bad Slug"})
		st := status.Convert(err)
		require.Equal(t, codes.InvalidArgument, st.Code())
		var fields []string
		for _, d := range st.Details() {
			if br, ok := d.(*errdetails.BadRequest); ok {
				for _, v := range br.GetFieldViolations() {
					fields = append(fields, v.GetField())
				}
			}
		}
		require.ElementsMatch(t, []string{"name", "slug"}, fields)
		_, err = projects.GetProject(ctx, &cpv1.GetProjectRequest{OrganizationId: "not-a-uuid", ProjectId: orgID})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	})

	t.Run("idempotency key reuse is ALREADY_EXISTS with a reason", func(t *testing.T) {
		kctx := metadata.AppendToOutgoingContext(ctx, grpcmeta.IdempotencyKey, "k-1")
		_, err := projects.CreateProject(kctx, &cpv1.CreateProjectRequest{OrganizationId: orgID, Name: "Web", Slug: "web"})
		require.NoError(t, err)
		_, err = projects.CreateProject(kctx, &cpv1.CreateProjectRequest{OrganizationId: orgID, Name: "API", Slug: "api"})
		st := status.Convert(err)
		require.Equal(t, codes.AlreadyExists, st.Code())
		require.Len(t, st.Details(), 1)
		info, ok := st.Details()[0].(*errdetails.ErrorInfo)
		require.True(t, ok)
		require.Equal(t, cpgrpc.ErrorReasonIdempotencyKeyReused, info.GetReason())
	})

	t.Run("foreign organizations are NOT_FOUND", func(t *testing.T) {
		_, err := orgs.GetOrganization(asUser(t.Context(), t, "mallory"), &cpv1.GetOrganizationRequest{OrganizationId: orgID})
		require.Equal(t, codes.NotFound, status.Code(err))
	})

	t.Run("missing principal is UNAUTHENTICATED", func(t *testing.T) {
		_, err := orgs.GetOrganization(t.Context(), &cpv1.GetOrganizationRequest{OrganizationId: orgID})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})

	t.Run("non-edge workloads are refused", func(t *testing.T) {
		authz := cpv1.NewAuthorizationServiceClient(h.worker)
		_, err := authz.CheckPermission(ctx, &cpv1.CheckPermissionRequest{OrganizationId: orgID, Permission: cpv1.Permission_PERMISSION_ORG_READ})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "CheckPermission is edge-only until internal callers are defined")
		_, err = cpv1.NewOrganizationServiceClient(h.worker).GetOrganization(ctx, &cpv1.GetOrganizationRequest{OrganizationId: orgID})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "public-API services serve the edge only")
	})

	t.Run("CheckPermission and AuthenticateApiKey", func(t *testing.T) {
		authz := cpv1.NewAuthorizationServiceClient(h.gateway)
		d, err := authz.CheckPermission(ctx, &cpv1.CheckPermissionRequest{OrganizationId: orgID, Permission: cpv1.Permission_PERMISSION_ORG_DELETE})
		require.NoError(t, err)
		require.True(t, d.GetAllowed())
		require.Equal(t, cpv1.Role_ROLE_OWNER, d.GetRole())

		p, err := projects.CreateProject(ctx, &cpv1.CreateProjectRequest{OrganizationId: orgID, Name: "Keys", Slug: "keys"})
		require.NoError(t, err)
		k, err := cpv1.NewApiKeyServiceClient(h.gateway).CreateApiKey(ctx, &cpv1.CreateApiKeyRequest{OrganizationId: orgID,
			ProjectId: p.GetProject().GetId(), Name: "ci", Scopes: []cpv1.ApiKeyScope{cpv1.ApiKeyScope_API_KEY_SCOPE_READ}})
		require.NoError(t, err)
		sum := sha256Sum(k.GetSecret())
		got, err := authz.AuthenticateApiKey(t.Context(), &cpv1.AuthenticateApiKeyRequest{KeySha256: sum})
		require.NoError(t, err, "no principal needed to resolve a key")
		require.Equal(t, k.GetApiKey().GetId(), got.GetApiKeyId())
		_, err = authz.AuthenticateApiKey(t.Context(), &cpv1.AuthenticateApiKeyRequest{KeySha256: make([]byte, 32)})
		require.Equal(t, codes.Unauthenticated, status.Code(err))
		_, err = cpv1.NewAuthorizationServiceClient(h.worker).AuthenticateApiKey(t.Context(), &cpv1.AuthenticateApiKeyRequest{KeySha256: sum})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "only the edge resolves raw keys")
	})
}

func TestIngestionAuthorization_Integration(t *testing.T) {
	h := start(t)
	ctx := t.Context()
	owner := asUser(ctx, t, "ingest-owner")
	otherOwner := asUser(ctx, t, "other-owner")
	orgs := cpv1.NewOrganizationServiceClient(h.gateway)
	projects := cpv1.NewProjectServiceClient(h.gateway)
	resources := cpv1.NewResourceServiceClient(h.gateway)
	keys := cpv1.NewApiKeyServiceClient(h.gateway)
	ingestion := cpv1.NewIngestionAuthorizationServiceClient(h.ingestion)

	createOrg := func(owner context.Context, name string) string {
		t.Helper()
		org, err := orgs.CreateOrganization(owner, &cpv1.CreateOrganizationRequest{Name: name})
		require.NoError(t, err)
		return org.GetOrganization().GetId()
	}
	createProject := func(owner context.Context, org, slug string) string {
		t.Helper()
		project, err := projects.CreateProject(owner, &cpv1.CreateProjectRequest{OrganizationId: org, Name: slug, Slug: slug})
		require.NoError(t, err)
		return project.GetProject().GetId()
	}
	createResource := func(owner context.Context, org, project, name string) string {
		t.Helper()
		resource, err := resources.CreateResource(owner, &cpv1.CreateResourceRequest{
			OrganizationId: org, ProjectId: project, Kind: cpv1.ResourceKind_RESOURCE_KIND_HOST, Name: name,
		})
		require.NoError(t, err)
		return resource.GetResource().GetId()
	}

	org := createOrg(owner, "Telemetry")
	project := createProject(owner, org, "ingest")
	resource := createResource(owner, org, project, "host")
	otherProject := createProject(owner, org, "other")
	otherProjectResource := createResource(owner, org, otherProject, "host")
	foreignOrg := createOrg(otherOwner, "Foreign")
	foreignProject := createProject(otherOwner, foreignOrg, "foreign")
	foreignResource := createResource(otherOwner, foreignOrg, foreignProject, "host")

	createKey := func(project, name string, scope cpv1.ApiKeyScope) *cpv1.CreateApiKeyResponse {
		t.Helper()
		key, err := keys.CreateApiKey(owner, &cpv1.CreateApiKeyRequest{
			OrganizationId: org, ProjectId: project, Name: name, Scopes: []cpv1.ApiKeyScope{scope},
		})
		require.NoError(t, err)
		return key
	}
	ingestKey := createKey(project, "Ingest", cpv1.ApiKeyScope_API_KEY_SCOPE_INGEST)
	boundRequest := &cpv1.ValidateIngestResourceRequest{KeySha256: sha256Sum(ingestKey.GetSecret()), ResourceId: resource}
	got, err := ingestion.ValidateIngestResource(ctx, boundRequest)
	require.NoError(t, err)
	require.Equal(t, org, got.GetOrganizationId())
	require.Equal(t, project, got.GetProjectId())

	readKey := createKey(project, "Read", cpv1.ApiKeyScope_API_KEY_SCOPE_READ)
	deletedResource := createResource(owner, org, project, "retired")
	_, err = resources.DeleteResource(owner, &cpv1.DeleteResourceRequest{
		OrganizationId: org, ProjectId: project, ResourceId: deletedResource,
	})
	require.NoError(t, err)
	deletedProject := createProject(owner, org, "retired")
	deletedProjectResource := createResource(owner, org, deletedProject, "host")
	deletedProjectKey := createKey(deletedProject, "Old project", cpv1.ApiKeyScope_API_KEY_SCOPE_INGEST)
	_, err = projects.DeleteProject(owner, &cpv1.DeleteProjectRequest{OrganizationId: org, ProjectId: deletedProject})
	require.NoError(t, err)

	for _, tt := range []struct {
		name     string
		key      []byte
		resource string
		want     codes.Code
	}{
		{"read scope", sha256Sum(readKey.GetSecret()), resource, codes.PermissionDenied},
		{"foreign project", boundRequest.GetKeySha256(), otherProjectResource, codes.PermissionDenied},
		{"foreign tenant", boundRequest.GetKeySha256(), foreignResource, codes.PermissionDenied},
		{"deleted resource", boundRequest.GetKeySha256(), deletedResource, codes.PermissionDenied},
		{"deleted project", sha256Sum(deletedProjectKey.GetSecret()), deletedProjectResource, codes.Unauthenticated},
		{"unknown key", make([]byte, sha256.Size), resource, codes.Unauthenticated},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response, err := ingestion.ValidateIngestResource(t.Context(), &cpv1.ValidateIngestResourceRequest{
				KeySha256: tt.key, ResourceId: tt.resource,
			})
			require.Equal(t, tt.want, status.Code(err))
			require.Nil(t, response)
		})
	}

	t.Run("only the ingestion workload can bind credentials", func(t *testing.T) {
		for _, peer := range []*grpc.ClientConn{h.gateway, h.worker} {
			_, err := cpv1.NewIngestionAuthorizationServiceClient(peer).ValidateIngestResource(t.Context(), boundRequest)
			require.Equal(t, codes.PermissionDenied, status.Code(err))
		}
	})
	t.Run("ingestion cannot act as the edge", func(t *testing.T) {
		authz := cpv1.NewAuthorizationServiceClient(h.ingestion)
		_, err := authz.AuthenticateApiKey(t.Context(), &cpv1.AuthenticateApiKeyRequest{KeySha256: boundRequest.GetKeySha256()})
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = authz.CheckPermission(owner, &cpv1.CheckPermissionRequest{
			OrganizationId: org, Permission: cpv1.Permission_PERMISSION_ORG_READ,
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err))
	})
	t.Run("revoked key", func(t *testing.T) {
		_, err := keys.RevokeApiKey(owner, &cpv1.RevokeApiKeyRequest{OrganizationId: org, ApiKeyId: ingestKey.GetApiKey().GetId()})
		require.NoError(t, err)
		_, err = ingestion.ValidateIngestResource(t.Context(), boundRequest)
		require.Equal(t, codes.Unauthenticated, status.Code(err))
	})
}

// TestF01_PostgresRestart: while Postgres restarts, requests fail fast with UNAVAILABLE (never hang or 500), the
// pool reconnects without restarting the service, and every acknowledged write is present afterwards.
func TestF01_PostgresRestart_Integration(t *testing.T) {
	h := start(t, pgtest.WithFixedHostPort())
	ctx := asUser(t.Context(), t, "alice")
	org, err := cpv1.NewOrganizationServiceClient(h.gateway).CreateOrganization(ctx, &cpv1.CreateOrganizationRequest{Name: "Acme"})
	require.NoError(t, err)
	orgID := org.GetOrganization().GetId()
	projects := cpv1.NewProjectServiceClient(h.gateway)

	type result struct {
		id   string
		code codes.Code
		took time.Duration
	}
	var (
		mu      sync.Mutex
		results []result
		stop    = make(chan struct{})
		wg      sync.WaitGroup
	)
	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				start := time.Now()
				p, err := projects.CreateProject(cctx, &cpv1.CreateProjectRequest{OrganizationId: orgID, Name: "p", Slug: fmt.Sprintf("p-%d-%d", w, i)})
				cancel()
				mu.Lock()
				results = append(results, result{id: p.GetProject().GetId(), code: status.Code(err), took: time.Since(start)})
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
	countOK := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, r := range results {
			if r.code == codes.OK {
				n++
			}
		}
		return n
	}
	require.Eventually(t, func() bool { return countOK() >= 10 }, 10*time.Second, 10*time.Millisecond, "warm-up traffic")
	h.db.Restart(t)
	// After the restart the service recovers on its own: no process restart, the pool reconnects.
	require.Eventually(t, func() bool {
		_, err := projects.ListProjects(ctx, &cpv1.ListProjectsRequest{OrganizationId: orgID})
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "service must recover after Postgres is back")
	recovered := countOK()
	require.Eventually(t, func() bool { return countOK() >= recovered+10 }, 10*time.Second, 10*time.Millisecond,
		"writers succeed again after recovery")
	close(stop)
	wg.Wait()

	var ok, unavailable int
	for _, r := range results {
		switch r.code {
		case codes.OK:
			ok++
		case codes.Unavailable:
			unavailable++
			require.Less(t, r.took, 2*time.Second, "failures must be fast, not timeouts")
		default:
			t.Fatalf("unexpected code %s after %s (only OK or UNAVAILABLE are acceptable)", r.code, r.took)
		}
	}
	require.Positive(t, unavailable, "the restart must have been observed")
	require.Positive(t, ok)
	// No confirmed transaction lost: every acknowledged create is readable.
	for _, r := range results {
		if r.code != codes.OK {
			continue
		}
		_, err := projects.GetProject(ctx, &cpv1.GetProjectRequest{OrganizationId: orgID, ProjectId: r.id})
		require.NoError(t, err, "acknowledged project %s must survive the restart", r.id)
	}
	t.Logf("F01: %d acknowledged, %d fast UNAVAILABLE, 0 other", ok, unavailable)
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
