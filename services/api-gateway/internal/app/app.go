// Package app wires the api-gateway process under pkg/app's ordered lifecycle.
package app

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	cpv1 "github.com/zodixdev/infrapilot/gen/go/infrapilot/controlplane/v1"
	"github.com/zodixdev/infrapilot/pkg/app"
	"github.com/zodixdev/infrapilot/pkg/grpcclient"
	"github.com/zodixdev/infrapilot/pkg/health"
	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/pkg/log"
	"github.com/zodixdev/infrapilot/pkg/telemetry"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/authz"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
	gwhttp "github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http"
)

// readOnlyRPCs are retried on UNAVAILABLE by the client (AGENTS.md §4: retries only for idempotent operations).
var readOnlyRPCs = []string{
	cpv1.OrganizationService_GetOrganization_FullMethodName, cpv1.OrganizationService_ListMyOrganizations_FullMethodName,
	cpv1.ProjectService_GetProject_FullMethodName, cpv1.ProjectService_ListProjects_FullMethodName,
	cpv1.MemberService_GetMember_FullMethodName, cpv1.MemberService_ListMembers_FullMethodName,
	cpv1.ResourceService_GetResource_FullMethodName, cpv1.ResourceService_ListResources_FullMethodName,
	cpv1.ApiKeyService_GetApiKey_FullMethodName, cpv1.ApiKeyService_ListApiKeys_FullMethodName,
	cpv1.AuditService_ListAuditEvents_FullMethodName,
	"/" + cpv1.AuthorizationService_ServiceDesc.ServiceName + "/*",
}

// Serve runs the gateway until SIGTERM/SIGINT, then drains (AGENTS.md §5). Startup depends on none of Keycloak,
// Redis or control-plane being up: each degrades per request (F02, F03, 503).
func Serve(ctx context.Context, cfg *Config, logCfg log.Config, logger *slog.Logger) (err error) {
	c := cfg.Common
	var cleanup []func(context.Context) error
	defer func() {
		if err == nil {
			return
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for i := len(cleanup) - 1; i >= 0; i-- {
			_ = cleanup[i](closeCtx) // best effort: the startup error is what matters
		}
	}()

	tel, err := telemetry.Setup(ctx, telemetry.Config{
		OTLPEndpoint: c.Telemetry.ExporterOTLPEndpoint, OTLPInsecure: c.Telemetry.ExporterOTLPInsecure,
		TraceSampleRatio: c.Telemetry.TracesSamplerArg, MetricsPush: c.Telemetry.MetricsPush, LogsPush: c.Telemetry.LogsPush,
		ExportInterval: c.Telemetry.ExportInterval, ExportTimeout: c.Telemetry.ExportTimeout,
	}, telemetry.Service{Name: c.ServiceName, Version: c.Version, Environment: string(c.Environment)}, logger)
	if err != nil {
		return err
	}
	cleanup = append(cleanup, tel.Shutdown)
	if h := tel.LogHandler(); h != nil {
		if logger, err = log.New(logCfg, os.Stdout, c.ServiceName, c.Version, string(c.Environment), h); err != nil {
			return err
		}
	}
	meter := tel.Meter("github.com/zodixdev/infrapilot/services/api-gateway")

	ropts := &redis.Options{Addr: cfg.Redis.Addr, Username: cfg.Redis.Username, Password: cfg.Redis.Password, DB: cfg.Redis.DB}
	if cfg.Redis.TLS {
		ropts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	rdb := redisx.NewClient(ropts)
	cleanup = append(cleanup, func(context.Context) error { return rdb.Close() })
	if tErr := redisotel.InstrumentTracing(rdb, redisotel.WithTracerProvider(tel.TracerProvider())); tErr != nil {
		return tErr
	}
	store, err := redisx.New(rdb, redisx.Config{}, meter)
	if err != nil {
		return err
	}

	cpConn, err := grpcclient.New(cfg.ControlPlane, c.Environment, grpcclient.WithLogger(logger), grpcclient.WithUpstream("control-plane"),
		grpcclient.WithTracerProvider(tel.TracerProvider()), grpcclient.WithMeterProvider(tel.MeterProvider()),
		grpcclient.WithIdempotent(readOnlyRPCs...))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, func(context.Context) error { return cpConn.Close() })

	jwksClient := &http.Client{Timeout: 10 * time.Second, Transport: otelhttp.NewTransport(http.DefaultTransport, otelhttp.WithTracerProvider(tel.TracerProvider()))}
	keys, err := auth.NewKeySet(auth.KeySetConfig{URL: cfg.OIDC.JWKSURL}, jwksClient, logger, meter)
	if err != nil {
		return err
	}
	verifier, err := auth.NewVerifier(auth.VerifierConfig{Issuer: cfg.OIDC.Issuer, Audience: cfg.OIDC.Audience}, keys)
	if err != nil {
		return err
	}
	authzClient := cpv1.NewAuthorizationServiceClient(cpConn)
	keyResolver, err := auth.NewKeyResolver(store, authzClient, meter)
	if err != nil {
		return err
	}
	authorizer, err := authz.New(store, authzClient, meter)
	if err != nil {
		return err
	}
	proxies, err := cfg.trustedProxies()
	if err != nil {
		return err
	}
	rl := cfg.RateLimit
	apiHandler, err := gwhttp.NewHandler(gwhttp.Deps{
		Verifier: verifier, Keys: keyResolver, Authorizer: authorizer, Store: store, CORSOrigins: cfg.CORSAllowedOrigins,
		Logger: logger, Meter: meter,
		Limits: gwhttp.Limits{
			User:      gwhttp.RateRule{Rate: rl.UserRPS, Burst: rl.UserBurst},
			APIKey:    gwhttp.RateRule{Rate: rl.APIKeyRPS, Burst: rl.APIKeyBurst},
			Anonymous: gwhttp.RateRule{Rate: rl.AnonymousRPS, Burst: rl.AnonymousBurst},
			Operations: map[string]gwhttp.RateRule{
				"addMember":          {Rate: rl.MemberChangeRPS, Burst: rl.MemberChangeBurst},
				"updateMemberRole":   {Rate: rl.MemberChangeRPS, Burst: rl.MemberChangeBurst},
				"removeMember":       {Rate: rl.MemberChangeRPS, Burst: rl.MemberChangeBurst},
				"createOrganization": {Rate: rl.OrgCreateRPS, Burst: rl.OrgCreateBurst},
				"createApiKey":       {Rate: rl.KeyCreateRPS, Burst: rl.KeyCreateBurst},
			},
			TrustedProxies: proxies,
		},
		Upstreams: gwhttp.Upstreams{
			Organizations: cpv1.NewOrganizationServiceClient(cpConn), Projects: cpv1.NewProjectServiceClient(cpConn),
			Members: cpv1.NewMemberServiceClient(cpConn), Resources: cpv1.NewResourceServiceClient(cpConn),
			APIKeys: cpv1.NewApiKeyServiceClient(cpConn), Audit: cpv1.NewAuditServiceClient(cpConn),
		},
	})
	if err != nil {
		return err
	}

	// Readiness is process-level only: dependency outages degrade per request instead of pulling every replica
	// out of rotation (which would turn a partial outage into a total one). Dependencies are watched by metrics.
	reg := health.NewRegistry(health.WithLogger(logger), health.WithMeter(tel.Meter("health")))
	mws, err := httpserver.Standard(httpserver.Options{Logger: logger, TracerProvider: tel.TracerProvider(),
		MeterProvider: tel.MeterProvider(), ServerName: c.ServiceName, MaxBodyBytes: c.HTTP.MaxBodyBytes, Edge: true})
	if err != nil {
		return err
	}
	httpCfg := httpserver.Config{
		Addr: c.HTTP.Addr, ReadHeaderTimeout: c.HTTP.ReadHeaderTimeout, ReadTimeout: c.HTTP.ReadTimeout,
		WriteTimeout: c.HTTP.WriteTimeout, IdleTimeout: c.HTTP.IdleTimeout, MaxHeaderBytes: c.HTTP.MaxHeaderBytes,
	}
	public, err := httpserver.New(ctx, httpCfg, httpserver.Chain(apiHandler, mws...), httpserver.WithName("http"), httpserver.WithLogger(logger))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, public.Shutdown)
	adminCfg := httpCfg
	adminCfg.Addr, adminCfg.WriteTimeout = c.Admin.Addr, c.Admin.WriteTimeout
	admin, err := httpserver.New(ctx, adminCfg,
		httpserver.AdminHandler(reg.LivenessHandler(), reg.ReadinessHandler(), tel.MetricsHandler(), c.Admin.Pprof),
		httpserver.WithName("admin"), httpserver.WithLogger(logger))
	if err != nil {
		return err
	}
	cleanup = append(cleanup, admin.Shutdown)

	a, err := app.New(app.Config{ShutdownDelay: c.Shutdown.Delay, ShutdownTimeout: c.Shutdown.Timeout},
		app.WithLogger(logger), app.WithReadinessGate(reg))
	if err != nil {
		return err
	}
	// Shutdown runs in reverse: public → admin → health → jwks → control-plane conn → redis → telemetry.
	a.Add(app.Closer("telemetry", tel.Shutdown),
		app.Closer("redis", func(context.Context) error { return rdb.Close() }),
		app.Closer("control-plane", func(context.Context) error { return cpConn.Close() }),
		app.Func("jwks", keys.Run, nil),
		app.Func("health", reg.Run, nil), admin, public)
	cleanup = nil // app.Run owns every resource from here on
	return a.Run(ctx)
}
