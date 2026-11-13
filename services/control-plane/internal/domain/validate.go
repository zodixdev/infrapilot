package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits shared with the database CHECK constraints (migrations/00001_init.sql) and the OpenAPI document.
const (
	MaxNameLen        = 200
	MaxDescriptionLen = 2000
	MaxLabels         = 32
	MaxLabelValueLen  = 255
	MaxEmailLen       = 320
	MaxAPIKeyLifetime = 400 * 24 * time.Hour
)

var (
	slugRe     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)
	labelKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,62}$`)
)

// CheckName validates a display name: 1–200 characters, valid UTF-8, no control characters, not only spaces.
func CheckName(v *Validator, field, name string) {
	switch {
	case !utf8.ValidString(name):
		v.Add(field, "must be valid UTF-8")
	case strings.TrimSpace(name) == "":
		v.Add(field, "must not be empty")
	case utf8.RuneCountInString(name) > MaxNameLen:
		v.Add(field, "must be at most %d characters", MaxNameLen)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		v.Add(field, "must not contain control characters")
	}
}

// CheckSlug validates a URL handle: lowercase letters, digits and hyphens, 3–63 characters, starting with a
// letter and not ending with a hyphen.
func CheckSlug(v *Validator, field, slug string) {
	v.Check(slugRe.MatchString(slug), field, "must be 3-63 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen")
}

// CheckDescription validates free text up to 2000 characters.
func CheckDescription(v *Validator, field, s string) {
	switch {
	case !utf8.ValidString(s):
		v.Add(field, "must be valid UTF-8")
	case utf8.RuneCountInString(s) > MaxDescriptionLen:
		v.Add(field, "must be at most %d characters", MaxDescriptionLen)
	}
}

// CheckLabels validates resource labels.
func CheckLabels(v *Validator, field string, labels map[string]string) {
	if len(labels) > MaxLabels {
		v.Add(field, "must have at most %d entries", MaxLabels)
		return
	}
	for k, val := range labels {
		if !labelKeyRe.MatchString(k) {
			v.Add(field+"."+k, "key must match ^[a-z][a-z0-9_.-]{0,62}$")
		}
		if !utf8.ValidString(val) || utf8.RuneCountInString(val) > MaxLabelValueLen || strings.IndexFunc(val, unicode.IsControl) >= 0 {
			v.Add(field+"."+k, "value must be at most %d printable characters", MaxLabelValueLen)
		}
	}
}

// NormalizeEmail validates an e-mail address and returns it trimmed (comparison is case-insensitive in SQL).
func NormalizeEmail(v *Validator, field, email string) string {
	email = strings.TrimSpace(email)
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > MaxEmailLen {
		v.Add(field, "must be a valid e-mail address")
	}
	return email
}

// CheckScopes validates API key scopes: 1–3 distinct known scopes.
func CheckScopes(v *Validator, field string, scopes []Scope) {
	if len(scopes) == 0 {
		v.Add(field, "must contain at least one scope")
		return
	}
	seen := map[Scope]bool{}
	for _, s := range scopes {
		if !s.Valid() {
			v.Add(field, "unknown scope %q", s)
		} else if seen[s] {
			v.Add(field, "duplicate scope %q", s)
		}
		seen[s] = true
	}
}

// CheckExpiry validates an optional API key expiry: in the future and at most 400 days ahead.
func CheckExpiry(v *Validator, field string, expires *time.Time, now time.Time) {
	if expires == nil {
		return
	}
	switch {
	case !expires.After(now):
		v.Add(field, "must be in the future")
	case expires.Sub(now) > MaxAPIKeyLifetime:
		v.Add(field, "must be at most 400 days ahead")
	}
}
