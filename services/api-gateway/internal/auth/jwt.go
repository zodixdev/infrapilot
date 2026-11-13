package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ErrInvalidToken covers every reason a bearer token is rejected (malformed, bad signature, expired, wrong
// issuer or audience). Callers answer 401 without detail.
var ErrInvalidToken = errors.New("auth: invalid token")

var allowedAlgs = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

func allowedAlg(a jose.SignatureAlgorithm) bool { return slices.Contains(allowedAlgs, a) }

// Identity is the user an access token was issued to.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// VerifierConfig configures access-token checks.
type VerifierConfig struct {
	Issuer   string
	Audience string
	// AuthorizedParties are the OAuth clients ("azp") whose tokens the API accepts; tokens issued to any other
	// realm client are refused even if they carry the audience.
	AuthorizedParties []string
	// MaxLifetime bounds exp - iat (default 1 h; Keycloak issues 5-minute access tokens).
	MaxLifetime time.Duration
	// Leeway tolerated on exp/nbf/iat (default 30 s).
	Leeway time.Duration
}

// Verifier validates Keycloak access tokens locally (ADR-0026).
type Verifier struct {
	cfg  VerifierConfig
	keys *KeySet
	now  func() time.Time
}

// NewVerifier returns a verifier over keys.
func NewVerifier(cfg VerifierConfig, keys *KeySet) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Audience == "" || len(cfg.AuthorizedParties) == 0 {
		return nil, errors.New("auth: issuer, audience and authorized parties are required")
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = time.Hour
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 30 * time.Second
	}
	return &Verifier{cfg: cfg, keys: keys, now: time.Now}, nil
}

type accessClaims struct {
	Type              string `json:"typ"`
	AuthorizedParty   string `json:"azp"`
	Email             string `json:"email"`
	EmailVerified     bool   `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

// Verify checks signature (RS256/ES256 only), issuer, audience, expiry and token type and returns the identity.
// ErrKeysUnavailable is returned (and must become 503) when no signing keys could ever be loaded.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	tok, err := jwt.ParseSigned(raw, allowedAlgs)
	if err != nil || len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return Identity{}, ErrInvalidToken
	}
	key, err := v.keys.Key(ctx, tok.Headers[0].KeyID)
	if errors.Is(err, ErrKeysUnavailable) {
		return Identity{}, err
	}
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if key.Algorithm != "" && key.Algorithm != tok.Headers[0].Algorithm {
		return Identity{}, ErrInvalidToken // the key is bound to another algorithm
	}
	var std jwt.Claims
	var extra accessClaims
	if err := tok.Claims(key.Key, &std, &extra); err != nil {
		return Identity{}, ErrInvalidToken
	}
	if std.Expiry == nil || std.IssuedAt == nil || std.Subject == "" ||
		std.Expiry.Time().Sub(std.IssuedAt.Time()) > v.cfg.MaxLifetime {
		return Identity{}, ErrInvalidToken
	}
	if !slices.Contains(v.cfg.AuthorizedParties, extra.AuthorizedParty) {
		return Identity{}, ErrInvalidToken
	}
	if err := std.ValidateWithLeeway(jwt.Expected{Issuer: v.cfg.Issuer, AnyAudience: jwt.Audience{v.cfg.Audience}, Time: v.now()}, v.cfg.Leeway); err != nil {
		return Identity{}, ErrInvalidToken
	}
	// Keycloak marks access tokens "Bearer"; ID and refresh tokens carry other types and must not authenticate.
	if extra.Type != "Bearer" {
		return Identity{}, ErrInvalidToken
	}
	name := extra.Name
	if name == "" {
		name = extra.PreferredUsername
	}
	return Identity{Subject: std.Subject, Email: extra.Email, EmailVerified: extra.EmailVerified, Name: name}, nil
}
