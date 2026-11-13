package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// APIKeyPrefix starts every InfraPilot API key (secret scanners match on it).
const APIKeyPrefix = "ipk_"

// apiKeySecretBytes of randomness: 256 bits, so an unsalted SHA-256 is safe to store and index (ADR-0022).
const apiKeySecretBytes = 32

// NewAPIKey returns a fresh key, its SHA-256 and its non-secret display prefix ("ipk_" + 8 characters).
func NewAPIKey() (secret string, hash [32]byte, display string, err error) {
	b := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", hash, "", fmt.Errorf("api key: %w", err)
	}
	secret = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return secret, sha256.Sum256([]byte(secret)), secret[:len(APIKeyPrefix)+8], nil
}

// LooksLikeAPIKey reports whether s has the shape of a key (prefix + 43 base64url characters); gateways use it
// to reject garbage before hashing and looking it up.
func LooksLikeAPIKey(s string) bool {
	rest, ok := strings.CutPrefix(s, APIKeyPrefix)
	if !ok || len(rest) != base64.RawURLEncoding.EncodedLen(apiKeySecretBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil
}
