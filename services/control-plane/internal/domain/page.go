package domain

import (
	"encoding/base64"

	"github.com/google/uuid"
)

// Page size bounds for every list operation (OpenAPI: limit 1–200, default 50).
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Page is a validated keyset page request: rows strictly after (or before, for descending lists) Cursor.
type Page struct {
	Size   int32
	Cursor uuid.UUID // uuid.Nil when starting from the beginning
}

// ParsePage validates size and token. Tokens are opaque to clients; they encode the last id of the previous
// page. A forged token only moves the start position: row-level security still bounds what can be read.
func ParsePage(v *Validator, size int32, token string) Page {
	p := Page{Size: size}
	switch {
	case size == 0:
		p.Size = DefaultPageSize
	case size < 0 || size > MaxPageSize:
		v.Add("page_size", "must be between 1 and %d", MaxPageSize)
	}
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(b) != 16 {
			v.Add("page_token", "is not a valid page token")
		} else {
			copy(p.Cursor[:], b)
		}
	}
	return p
}

// Limit is the row count to query: one more than the page, so the presence of a next page is known exactly.
func (p Page) Limit() int32 { return p.Size + 1 }

// Trim cuts rows fetched with Limit() to the page and reports whether more rows exist.
func Trim[T any](p Page, rows []T) (page []T, more bool) {
	if len(rows) > int(p.Size) {
		return rows[:p.Size], true
	}
	return rows, false
}

// NextToken returns the token for the page after last, or "" when there are no more rows.
func NextToken(more bool, last uuid.UUID) string {
	if !more {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(last[:])
}
