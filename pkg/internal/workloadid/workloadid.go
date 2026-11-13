// Package workloadid extracts and validates InfraPilot workload identities (ADR-0025): spiffe:// URIs carried
// as the single URI SAN of an X.509 certificate. Parsing is strict so that two different certificates can
// never map to the same identity string through encoding tricks or SAN ordering.
package workloadid

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Validate reports whether id is a canonical spiffe:// identity: lowercase trust domain, no userinfo, port,
// query, fragment or percent-encoding, and a non-empty path without empty, "." or ".." segments.
func Validate(id string) error {
	u, err := url.Parse(id)
	if err != nil {
		return fmt.Errorf("workload identity %q: %w", id, err)
	}
	return validateURL(id, u)
}

func validateURL(raw string, u *url.URL) error {
	switch {
	case u.Scheme != "spiffe":
		return fmt.Errorf("workload identity %q: scheme must be spiffe", raw)
	case u.Host == "" || u.Host != strings.ToLower(u.Host) || u.Port() != "" || strings.Contains(u.Host, ":"):
		return fmt.Errorf("workload identity %q: trust domain must be a lowercase host without port", raw)
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "":
		return fmt.Errorf("workload identity %q: userinfo, query and fragment are not allowed", raw)
	case u.RawPath != "" || strings.Contains(raw, "%"):
		return fmt.Errorf("workload identity %q: percent-encoding is not allowed", raw)
	case u.Path == "" || u.Path == "/":
		return fmt.Errorf("workload identity %q: path is required", raw)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(u.Path, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("workload identity %q: empty, . and .. path segments are not allowed", raw)
		}
	}
	if u.String() != raw {
		return fmt.Errorf("workload identity %q: not in canonical form", raw)
	}
	return nil
}

// ErrNoIdentity is returned for certificates without exactly one valid spiffe URI SAN.
var ErrNoIdentity = errors.New("certificate carries no valid workload identity")

// FromCertificate returns the identity of cert, which must carry exactly one URI SAN.
func FromCertificate(cert *x509.Certificate) (string, error) {
	if cert == nil || len(cert.URIs) != 1 {
		return "", ErrNoIdentity
	}
	u := cert.URIs[0]
	raw := u.String()
	if err := validateURL(raw, u); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoIdentity, err)
	}
	return raw, nil
}
