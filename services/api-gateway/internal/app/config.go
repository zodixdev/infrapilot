package app

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/zodixdev/infrapilot/pkg/config"
)

// Config is the api-gateway configuration.
type Config struct {
	config.Common
	OIDC struct {
		// Issuer must equal the "iss" claim exactly (the URL users' browsers reach Keycloak at).
		Issuer string `env:"ISSUER"`
		// JWKSURL is fetched by the gateway (an in-cluster address may differ from the issuer's host).
		JWKSURL  string `env:"JWKS_URL"`
		Audience string `env:"AUDIENCE" envDefault:"infrapilot-api"`
	} `envPrefix:"OIDC_"`
	Redis struct {
		Addr     string `env:"ADDR"`
		Username string `env:"USERNAME" envDefault:""`
		Password string `env:"PASSWORD" envDefault:""`
		DB       int    `env:"DB" envDefault:"0"`
		TLS      bool   `env:"TLS" envDefault:"false"`
	} `envPrefix:"REDIS_"`
	ControlPlane config.GRPCClient `envPrefix:"CONTROLPLANE_"`
	// CORSAllowedOrigins lists browser origins (the dashboard).
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:"," envDefault:""`
	// TrustedProxies are CIDRs whose X-Forwarded-For is believed (ingress controller pods).
	TrustedProxies []string `env:"TRUSTED_PROXIES" envSeparator:"," envDefault:""`
	RateLimit      struct {
		UserRPS           float64 `env:"USER_RPS" envDefault:"50"`
		UserBurst         int     `env:"USER_BURST" envDefault:"100"`
		APIKeyRPS         float64 `env:"API_KEY_RPS" envDefault:"100"`
		APIKeyBurst       int     `env:"API_KEY_BURST" envDefault:"200"`
		AnonymousRPS      float64 `env:"ANONYMOUS_RPS" envDefault:"10"`
		AnonymousBurst    int     `env:"ANONYMOUS_BURST" envDefault:"20"`
		MemberChangeRPS   float64 `env:"MEMBER_CHANGE_RPS" envDefault:"1"`
		MemberChangeBurst int     `env:"MEMBER_CHANGE_BURST" envDefault:"20"`
		OrgCreateRPS      float64 `env:"ORG_CREATE_RPS" envDefault:"0.01"`
		OrgCreateBurst    int     `env:"ORG_CREATE_BURST" envDefault:"5"`
		KeyCreateRPS      float64 `env:"KEY_CREATE_RPS" envDefault:"0.2"`
		KeyCreateBurst    int     `env:"KEY_CREATE_BURST" envDefault:"10"`
	} `envPrefix:"RATE_LIMIT_"`
	// UpstreamTimeout bounds each call to an internal service.
	UpstreamTimeout time.Duration `env:"UPSTREAM_TIMEOUT" envDefault:"5s"`
}

// Validate implements config.Validator.
func (c *Config) Validate() error {
	errs := []error{c.Common.Validate()}
	for name, v := range map[string]string{"OIDC_ISSUER": c.OIDC.Issuer, "OIDC_JWKS_URL": c.OIDC.JWKSURL} {
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			errs = append(errs, fmt.Errorf("%s must be an http(s) URL", name))
			continue
		}
		if (c.Environment == config.EnvProd || c.Environment == config.EnvStaging) && u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("%s must use https in %s", name, c.Environment))
		}
	}
	if strings.TrimSpace(c.OIDC.Audience) == "" {
		errs = append(errs, errors.New("OIDC_AUDIENCE must not be empty"))
	}
	if c.Redis.Addr == "" {
		errs = append(errs, errors.New("REDIS_ADDR is required"))
	}
	if (c.Environment == config.EnvProd || c.Environment == config.EnvStaging) && !c.Redis.TLS {
		errs = append(errs, fmt.Errorf("REDIS_TLS is required in %s", c.Environment))
	}
	if err := c.ControlPlane.ValidateFor(c.Environment); err != nil {
		errs = append(errs, fmt.Errorf("CONTROLPLANE_*: %w", err))
	}
	for _, o := range c.CORSAllowedOrigins {
		if u, err := url.Parse(o); err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			errs = append(errs, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must be scheme://host[:port]", o))
		}
	}
	if _, err := c.trustedProxies(); err != nil {
		errs = append(errs, err)
	}
	r := c.RateLimit
	for name, ok := range map[string]bool{
		"user": r.UserRPS > 0 && r.UserBurst > 0, "api key": r.APIKeyRPS > 0 && r.APIKeyBurst > 0,
		"anonymous": r.AnonymousRPS > 0 && r.AnonymousBurst > 0, "member change": r.MemberChangeRPS > 0 && r.MemberChangeBurst > 0,
		"org create": r.OrgCreateRPS > 0 && r.OrgCreateBurst > 0, "key create": r.KeyCreateRPS > 0 && r.KeyCreateBurst > 0,
	} {
		if !ok {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_* %s rate and burst must be positive", name))
		}
	}
	if c.UpstreamTimeout <= 0 {
		errs = append(errs, errors.New("UPSTREAM_TIMEOUT must be positive"))
	}
	return errors.Join(errs...)
}

func (c *Config) trustedProxies() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(c.TrustedProxies))
	for _, s := range c.TrustedProxies {
		p, err := netip.ParsePrefix(strings.TrimSpace(s))
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %q must be a CIDR", s)
		}
		out = append(out, p)
	}
	return out, nil
}
