package redisx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"
)

// ErrTampered is returned for a cache value whose MAC does not verify (written by someone without the secret, or
// moved between keys). Callers treat it as a miss.
var ErrTampered = errors.New("redisx: cache value failed integrity check")

// SignedCache stores values with an HMAC-SHA256 over (key, value) so that anyone able to write to Redis cannot
// plant a principal or an authorization decision the gateway would trust. Rotating the secret simply turns every
// entry into a miss (the cache is rebuildable, data.md §1).
type SignedCache struct {
	store  *Store
	secret []byte
}

// NewSignedCache returns a signing wrapper; the secret must have at least 32 bytes.
func NewSignedCache(store *Store, secret []byte) (*SignedCache, error) {
	if len(secret) < 32 {
		return nil, errors.New("redisx: cache signing secret must have at least 32 bytes")
	}
	return &SignedCache{store: store, secret: append([]byte(nil), secret...)}, nil
}

func (c *SignedCache) mac(key string, val []byte) []byte {
	m := hmac.New(sha256.New, c.secret)
	// hash.Hash.Write never returns an error.
	_, _ = m.Write([]byte(key))
	_, _ = m.Write([]byte{0})
	_, _ = m.Write(val)
	return m.Sum(nil)
}

// Get returns a verified value; found is false and err is ErrTampered when verification fails.
func (c *SignedCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, found, err := c.store.Get(ctx, key)
	if err != nil || !found {
		return nil, false, err
	}
	if len(b) < sha256.Size {
		return nil, false, ErrTampered
	}
	val, sum := b[:len(b)-sha256.Size], b[len(b)-sha256.Size:]
	if !hmac.Equal(sum, c.mac(key, val)) {
		return nil, false, ErrTampered
	}
	return val, true, nil
}

// Set stores val with its MAC.
func (c *SignedCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	return c.store.Set(ctx, key, append(append(make([]byte, 0, len(val)+sha256.Size), val...), c.mac(key, val)...), ttl)
}
