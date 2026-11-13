package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	nethttpmw "github.com/oapi-codegen/nethttp-middleware"
	"go.opentelemetry.io/otel/metric"

	"github.com/zodixdev/infrapilot/pkg/httpserver"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/auth"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/authz"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/redisx"
	"github.com/zodixdev/infrapilot/services/api-gateway/internal/transport/http/api"
)

// Deps are the collaborators of the public API handler.
type Deps struct {
	Verifier    *auth.Verifier
	Keys        *auth.KeyResolver
	Authorizer  *authz.Authorizer
	Store       *redisx.Store
	Limits      Limits
	CORSOrigins []string
	Upstreams   Upstreams
	Logger      *slog.Logger
	Meter       metric.Meter
}

// BasePath of every public route (ADR-0003: versioned by path).
const BasePath = "/v1"

// NewHandler assembles the public API, outermost first: CORS → authentication → rate limit → contract
// validation (types, sizes, required credentials per operation) → generated strict server, whose operation
// middleware applies per-operation limits and the edge pre-authorization.
func NewHandler(d Deps) (http.Handler, error) {
	spec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("gateway: load OpenAPI document: %w", err)
	}
	// Validate paths relative to the version prefix; the document's absolute server URLs are for clients.
	spec.Servers = openapi3.Servers{&openapi3.Server{URL: BasePath}}
	validator, err := newValidator(spec)
	if err != nil {
		return nil, err
	}
	lim, err := newLimiter(d.Store, d.Limits, d.Meter, d.Logger)
	if err != nil {
		return nil, err
	}
	onUpstream := upstreamError(d.Logger)
	strict := api.NewStrictHandlerWithOptions(NewHandlers(d.Upstreams),
		[]api.StrictMiddlewareFunc{operationPolicy(d.Authorizer, lim)},
		api.StrictHTTPServerOptions{
			RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
				httpserver.WriteProblemBody(w, r, httpserver.Problem{Status: http.StatusBadRequest, Code: codeValidation, Title: "malformed request body"})
			},
			ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
				if errors.Is(err, errResponded) {
					return
				}
				onUpstream(w, r, err)
			},
		})
	mux := http.NewServeMux()
	api.HandlerWithOptions(strict, api.StdHTTPServerOptions{
		BaseURL:    BasePath,
		BaseRouter: mux,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, _ error) {
			httpserver.WriteProblemBody(w, r, httpserver.Problem{Status: http.StatusBadRequest, Code: codeValidation, Title: "invalid parameter"})
		},
	})
	return httpserver.Chain(mux, cors(d.CORSOrigins), authenticate(d.Verifier, d.Keys, d.Logger), lim.middleware, validator), nil
}

// newValidator builds the OpenAPI request validator; the library panics on an unroutable document, which is
// turned into a startup error here.
func newValidator(spec *openapi3.T) (mw httpserver.Middleware, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("gateway: OpenAPI router: %v", r)
		}
	}()
	v := nethttpmw.OapiRequestValidatorWithOptions(spec, &nethttpmw.Options{
		Options: openapi3filter.Options{AuthenticationFunc: securityCheck},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, _ nethttpmw.ErrorHandlerOpts) {
			validationError(w, r, err)
		},
		SilenceServersWarning: true,
	})
	return v, nil
}
