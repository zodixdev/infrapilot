package log

import (
	"encoding"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Redacted replaces every secret-looking value.
const Redacted = "[REDACTED]"

// maxDepth bounds the recursive conversion of structured values.
const maxDepth = 6

// secretKeyFragments are matched case-insensitively against attribute keys and group names (with '-' and
// '.' folded to '_'). A match masks the whole value, including nested structures.
var secretKeyFragments = []string{
	"password", "passwd", "passphrase", "pwd", "secret", "token", "authorization", "auth_header",
	"api_key", "apikey", "private_key", "privatekey", "signing_key", "secret_key", "access_key",
	"encryption_key", "credential", "cookie", "session", "jwt", "dsn", "connection_string",
}

// exactSecretKeys are only masked on an exact match (as fragments they would hit harmless keys).
var exactSecretKeys = map[string]bool{"pass": true, "key": true, "auth": true}

var (
	// scheme://user:password@host — user may be empty (redis://:pw@host); the password runs to the last '@'
	// before whitespace, so passwords containing '/', ':' or '@' are fully covered.
	urlUserinfo = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)([^\s/@:]*):(\S*)@`)
	// key=value / key: value / "key":"value" / key='value' for any key containing a secret word, with any
	// prefix (PGPASSWORD, access_token, aws_secret_access_key, private-key).
	keyValue = regexp.MustCompile(`(?i)([a-z0-9_.\-]*(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|private[_-]?key|credential|jwt)[a-z0-9_.\-]*["']?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s&;,"'}\]<]+)`)
	// <password>value</password>
	xmlElement = regexp.MustCompile(`(?i)(<([a-z0-9_.\-]*(?:password|secret|token|api[_-]?key)[a-z0-9_.\-]*)>)[^<]*(</)`)
	// Authorization headers in any scheme, and bare bearer/basic credentials.
	authHeader = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?\s*(?:[a-z]+\s+)?)[^\s"',}]+`)
	authScheme = regexp.MustCompile(`(?i)\b(bearer|basic|digest)(\s+)[A-Za-z0-9\-._~+/]+=*`)
	// JSON Web Tokens and InfraPilot API keys anywhere in text.
	jwt    = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]*`)
	apiKey = regexp.MustCompile(`\bipk_[A-Za-z0-9]{8,}`)
)

// Scrub masks secrets embedded in free-form text. It is exported for other packages that surface error
// strings (e.g. readiness reports).
func Scrub(s string) string {
	if s == "" {
		return s
	}
	s = urlUserinfo.ReplaceAllString(s, "${1}${2}:"+Redacted+"@")
	s = xmlElement.ReplaceAllString(s, "${1}"+Redacted+"${3}")
	s = authHeader.ReplaceAllString(s, "${1}"+Redacted)
	s = authScheme.ReplaceAllString(s, "${1}${2}"+Redacted)
	s = keyValue.ReplaceAllStringFunc(s, func(m string) string {
		sub := keyValue.FindStringSubmatch(m)
		if strings.Contains(sub[2], Redacted) {
			return m
		}
		// Keep the quotes of quoted values so embedded JSON/DSNs stay well-formed.
		if q := sub[2][0]; q == '"' || q == '\'' {
			return sub[1] + string(q) + Redacted + string(q)
		}
		return sub[1] + Redacted
	})
	s = jwt.ReplaceAllString(s, Redacted)
	s = apiKey.ReplaceAllString(s, Redacted)
	return s
}

func isSecretKey(key string) bool {
	k := strings.NewReplacer("-", "_", ".", "_").Replace(strings.ToLower(key))
	if exactSecretKeys[k] {
		return true
	}
	for _, frag := range secretKeyFragments {
		if strings.Contains(k, frag) {
			return true
		}
	}
	return false
}

// redact returns a scrubbed copy of a. Secret keys (or anything under a secret group) are masked;
// structured values are converted to groups and walked; strings, errors and Stringers are scrubbed.
func redact(a slog.Attr, masked bool, depth int) slog.Attr {
	if masked || isSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Scrub(v.String()))
	case slog.KindGroup:
		members := v.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = redact(m, false, depth+1)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: anyValue(v.Any(), depth)}
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindTime, slog.KindUint64, slog.KindLogValuer:
		return slog.Attr{Key: a.Key, Value: v}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// anyValue converts an arbitrary value into a slog.Value that the handlers can render without ever
// calling json.Marshal on unknown types (which would bypass redaction).
func anyValue(x any, depth int) slog.Value {
	if x == nil {
		return slog.StringValue("<nil>")
	}
	if depth >= maxDepth {
		return slog.StringValue(fmt.Sprintf("<%T>", x))
	}
	switch t := x.(type) {
	case error:
		return slog.StringValue(Scrub(t.Error()))
	case []byte:
		return slog.StringValue(fmt.Sprintf("[REDACTED bytes len=%d]", len(t)))
	case time.Time:
		return slog.TimeValue(t)
	case time.Duration:
		return slog.DurationValue(t)
	case fmt.Stringer:
		return slog.StringValue(Scrub(t.String()))
	case encoding.TextMarshaler:
		b, err := t.MarshalText()
		if err != nil {
			return slog.StringValue(fmt.Sprintf("<%T>", x))
		}
		return slog.StringValue(Scrub(string(b)))
	}
	rv := reflect.ValueOf(x)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return slog.StringValue("<nil>")
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.String:
		return slog.StringValue(Scrub(rv.String()))
	case reflect.Bool:
		return slog.BoolValue(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return slog.Int64Value(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return slog.Uint64Value(rv.Uint())
	case reflect.Float32, reflect.Float64:
		return slog.Float64Value(rv.Float())
	case reflect.Map:
		attrs := make([]slog.Attr, 0, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			k := fmt.Sprint(iter.Key().Interface())
			attrs = append(attrs, redact(slog.Attr{Key: k, Value: anyValue(iter.Value().Interface(), depth+1)}, false, depth+1))
		}
		return slog.GroupValue(attrs...)
	case reflect.Struct:
		rt := rv.Type()
		attrs := make([]slog.Attr, 0, rt.NumField())
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			attrs = append(attrs, redact(slog.Attr{Key: f.Name, Value: anyValue(rv.Field(i).Interface(), depth+1)}, false, depth+1))
		}
		return slog.GroupValue(attrs...)
	case reflect.Slice, reflect.Array:
		attrs := make([]slog.Attr, 0, rv.Len())
		for i := range rv.Len() {
			attrs = append(attrs, slog.Attr{Key: strconv.Itoa(i), Value: anyValue(rv.Index(i).Interface(), depth+1)})
		}
		return slog.GroupValue(attrs...)
	default:
		return slog.StringValue(fmt.Sprintf("<%T>", x))
	}
}
