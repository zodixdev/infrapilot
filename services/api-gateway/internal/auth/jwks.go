// Package auth authenticates public API callers at the edge (ADR-0026): Keycloak access tokens verified locally
// against a cached JWKS, and project API keys resolved through control-plane with a cache in front.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"
)

// Errors of the key set.
var (
	// ErrUnknownKey: the token names a key id the key set does not contain (after any permitted refresh).
	ErrUnknownKey = errors.New("auth: unknown signing key")
	// ErrKeysUnavailable: no key set has ever been loaded (identity provider unreachable since startup).
	ErrKeysUnavailable = errors.New("auth: signing keys unavailable")
)

// KeySetConfig configures the JWKS cache.
type KeySetConfig struct {
	URL string
	// RefreshInterval between background refreshes (default 10 min).
	RefreshInterval time.Duration
	// MinRefreshGap rate-limits refreshes triggered by unknown key ids (default 30 s), so a stream of tokens
	// with random kids cannot turn into load on the identity provider.
	MinRefreshGap time.Duration
	// FetchTimeout bounds one JWKS request (default 5 s).
	FetchTimeout time.Duration
}

// KeySet caches the identity provider's public signing keys. When refreshes fail it keeps serving the last
// good set indefinitely (F02); the key-set age metric and alert report how stale it is.
type KeySet struct {
	cfg    KeySetConfig
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu          sync.RWMutex
	keys        map[string]jose.JSONWebKey
	fetchedAt   time.Time
	lastAttempt time.Time

	sf        singleflight.Group
	refreshes metric.Int64Counter
}

// NewKeySet returns an empty key set; Run loads it and keeps it fresh.
func NewKeySet(cfg KeySetConfig, client *http.Client, logger *slog.Logger, meter metric.Meter) (*KeySet, error) {
	if cfg.URL == "" {
		return nil, errors.New("auth: JWKS URL is required")
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 10 * time.Minute
	}
	if cfg.MinRefreshGap <= 0 {
		cfg.MinRefreshGap = 30 * time.Second
	}
	if cfg.FetchTimeout <= 0 {
		cfg.FetchTimeout = 5 * time.Second
	}
	k := &KeySet{cfg: cfg, client: client, log: logger, now: time.Now, keys: map[string]jose.JSONWebKey{}}
	var err error
	if k.refreshes, err = meter.Int64Counter("gateway_jwks_refresh_total", metric.WithDescription("JWKS fetches by result and trigger")); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	_, err = meter.Float64ObservableGauge("gateway_jwks_age_seconds",
		metric.WithDescription("Seconds since the signing keys were last fetched successfully"), metric.WithUnit("s"),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			k.mu.RLock()
			at := k.fetchedAt
			k.mu.RUnlock()
			if !at.IsZero() {
				o.Observe(k.now().Sub(at).Seconds())
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	_, err = meter.Int64ObservableGauge("gateway_jwks_keys", metric.WithDescription("Signing keys in the cached set"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			k.mu.RLock()
			n := len(k.keys)
			k.mu.RUnlock()
			o.Observe(int64(n))
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return k, nil
}

// Run loads the key set and refreshes it until ctx is done. Startup does not wait for the identity provider:
// API-key requests are served meanwhile and token requests get 503 until the first load succeeds.
func (k *KeySet) Run(ctx context.Context) error {
	_ = k.refresh(ctx, "startup")
	t := time.NewTicker(k.cfg.RefreshInterval)
	defer t.Stop()
	retry := time.NewTimer(0)
	defer retry.Stop()
	<-retry.C
	for {
		k.mu.RLock()
		loaded := !k.fetchedAt.IsZero()
		k.mu.RUnlock()
		if !loaded {
			// Not loaded yet: retry soon (bounded by MinRefreshGap) instead of waiting a full interval.
			retry.Reset(k.cfg.MinRefreshGap)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_ = k.refresh(ctx, "interval")
		case <-retry.C:
			_ = k.refresh(ctx, "startup")
		}
	}
}

// Loaded reports whether a key set has ever been fetched.
func (k *KeySet) Loaded() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return !k.fetchedAt.IsZero()
}

// Key returns the key for kid, refreshing once (rate-limited, deduplicated) when it is unknown.
func (k *KeySet) Key(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	k.mu.RLock()
	allowed := k.now().Sub(k.lastAttempt) >= k.cfg.MinRefreshGap
	k.mu.RUnlock()
	if allowed {
		// Shared by concurrent callers; detached from any one request's cancellation.
		_, _, _ = k.sf.Do("refresh", func() (any, error) {
			return nil, k.refresh(context.WithoutCancel(ctx), "unknown_kid")
		})
		if key, ok := k.lookup(kid); ok {
			return key, nil
		}
	}
	if !k.Loaded() {
		return jose.JSONWebKey{}, ErrKeysUnavailable
	}
	return jose.JSONWebKey{}, ErrUnknownKey
}

func (k *KeySet) lookup(kid string) (jose.JSONWebKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[kid]
	return key, ok
}

func (k *KeySet) refresh(ctx context.Context, trigger string) error {
	k.mu.Lock()
	k.lastAttempt = k.now()
	k.mu.Unlock()
	keys, err := k.fetch(ctx)
	result := "ok"
	if err != nil {
		result = "error"
		k.log.WarnContext(ctx, "JWKS refresh failed; keeping the cached keys", slog.String("trigger", trigger), slog.Any("error", err))
	} else {
		k.mu.Lock()
		k.keys, k.fetchedAt = keys, k.now()
		k.mu.Unlock()
	}
	k.refreshes.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result), attribute.String("trigger", trigger)))
	return err
}

func (k *KeySet) fetch(ctx context.Context) (map[string]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(ctx, k.cfg.FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.cfg.URL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jwks fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks decode: %w", err)
	}
	keys := make(map[string]jose.JSONWebKey, len(set.Keys))
	for i := range set.Keys {
		key := &set.Keys[i]
		// Only public signing keys of the allowed algorithms; encryption keys and symmetric keys are ignored.
		if key.KeyID == "" || !key.IsPublic() || (key.Use != "" && key.Use != "sig") ||
			(key.Algorithm != "" && !allowedAlg(jose.SignatureAlgorithm(key.Algorithm))) {
			continue
		}
		keys[key.KeyID] = *key
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable signing keys")
	}
	return keys, nil
}
