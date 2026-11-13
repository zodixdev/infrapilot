package log

import (
	"encoding"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Redacted replaces every secret-looking value.
const Redacted = "[REDACTED]"

const (
	// maxDepth bounds the recursive conversion of structured values.
	maxDepth = 6
	// maxElems bounds how many elements of one map/slice/struct are rendered.
	maxElems = 100
)

// secretKeyFragments are matched case-insensitively against attribute keys, map keys, struct field names,
// json tag names and group names (with '-' and '.' folded to '_'). A match masks the whole value.
var secretKeyFragments = []string{
	"password", "passwd", "passphrase", "pwd", "secret", "token", "authorization", "auth_header",
	"api_key", "apikey", "private_key", "privatekey", "signing_key", "secret_key", "access_key",
	"encryption_key", "credential", "cookie", "session", "jwt", "dsn", "connection_string",
}

// exactSecretKeys are only masked on an exact match (as fragments they would hit harmless keys).
var exactSecretKeys = map[string]bool{"pass": true, "key": true, "auth": true}

var keyFolder = strings.NewReplacer("-", "_", ".", "_")

const quoted = `"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`

var (
	// scheme://…@ candidates; userinfo vs host:port/path is decided in scrubURLs.
	urlCandidate = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)(\S*)@`)
	hostPort     = regexp.MustCompile(`^[^:@]+:\d+$`)
	// key=value / key: value / "key":"value" / key='va\'lue' for any key containing a secret word, with any
	// prefix (PGPASSWORD, access_token, aws_secret_access_key, private-key, session_id, x-auth).
	keyValue = regexp.MustCompile(`(?i)([a-z0-9_.\-]*(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|private[_-]?key|credential|jwt|session|cookie|auth)[a-z0-9_.\-]*["']?\s*[=:]\s*)(` + quoted + `|[^\s&;,"'}\]<]+)`)
	// <password>value</password>, also with attributes on the opening tag.
	xmlElement = regexp.MustCompile(`(?i)(<([a-z0-9_.\-]*(?:password|secret|token|api[_-]?key)[a-z0-9_.\-]*)(?:\s[^>]*)?>)[^<]*(</)`)
	// Authorization headers in any scheme, cookies, and bare credentials after well-known schemes.
	authHeader = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?\s*(?:[a-z]+\s+)?)[^\s"',}]+`)
	cookieLine = regexp.MustCompile(`(?i)\b((?:set-)?cookie["']?\s*:\s*)[^\r\n"]+`)
	authScheme = regexp.MustCompile(`(?i)\b(bearer|basic|digest)(\s+)[A-Za-z0-9\-._~+/]+=*`)
	// "token <value>" only for credential-shaped values, so prose like "token validation failed" survives.
	tokenScheme = regexp.MustCompile(`(?i)\b(token)(\s+)[A-Za-z0-9\-._~+/]{20,}=*`)
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
	// Prefilter (profiled: regex backtracking dominated every log record): each pattern runs only when its
	// literal trigger occurs, which is rare in ordinary attribute values (routes, ids, user agents).
	lower := strings.ToLower(s)
	if strings.Contains(lower, "://") && strings.Contains(lower, "@") {
		s = scrubURLs(s)
	}
	if strings.Contains(lower, "<") {
		s = xmlElement.ReplaceAllString(s, "${1}"+Redacted+"${3}")
	}
	if strings.Contains(lower, "authoriz") {
		s = authHeader.ReplaceAllString(s, "${1}"+Redacted)
	}
	if strings.Contains(lower, "cookie") {
		s = cookieLine.ReplaceAllString(s, "${1}"+Redacted)
	}
	if strings.Contains(lower, "bearer") || strings.Contains(lower, "basic") || strings.Contains(lower, "digest") {
		s = authScheme.ReplaceAllString(s, "${1}${2}"+Redacted)
	}
	if strings.Contains(lower, "token") {
		s = tokenScheme.ReplaceAllString(s, "${1}${2}"+Redacted)
	}
	if strings.ContainsAny(lower, "=:") && containsAny(lower, keyWords) {
		s = scrubKeyValues(s)
	}
	if strings.Contains(s, "eyJ") {
		s = jwt.ReplaceAllString(s, Redacted)
	}
	if strings.Contains(s, "ipk_") {
		s = apiKey.ReplaceAllString(s, Redacted)
	}
	return s
}

// keyWords are the literal triggers of keyValue (any match makes the regex worth running).
var keyWords = []string{"pass", "pwd", "secret", "token", "key", "credential", "jwt", "session", "cookie", "auth"}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func scrubKeyValues(s string) string {
	return keyValue.ReplaceAllStringFunc(s, func(m string) string {
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
}

// scrubURLs masks URL userinfo. The candidate runs to the last '@' of the token so passwords containing
// '/', ':' or '@' are covered; "host:port/path…@…" (an '@' in the path or query) is left alone.
func scrubURLs(s string) string {
	return urlCandidate.ReplaceAllStringFunc(s, func(m string) string {
		sub := urlCandidate.FindStringSubmatch(m)
		scheme, rest := sub[1], sub[2]
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			head := rest[:i]
			if !strings.Contains(head, ":") || hostPort.MatchString(head) {
				return m // the '@' belongs to the path/query, not to userinfo
			}
		}
		if user, _, ok := strings.Cut(rest, ":"); ok {
			return scheme + user + ":" + Redacted + "@"
		}
		if rest == "" {
			return m
		}
		return scheme + Redacted + "@" // token-only userinfo (https://<token>@host)
	})
}

func isSecretKey(key string) bool {
	k := keyFolder.Replace(strings.ToLower(key))
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

// redact returns a scrubbed copy of a. Secret keys (or anything under a secret group) are masked; structured
// values are converted to groups and walked; strings, errors and Stringers are scrubbed.
func redact(a slog.Attr, masked bool, depth int) slog.Attr {
	key := Scrub(a.Key)
	if masked || isSecretKey(a.Key) {
		return slog.String(key, Redacted)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(key, Scrub(v.String()))
	case slog.KindGroup:
		members := v.Group()
		out := make([]slog.Attr, len(members))
		for i, m := range members {
			out[i] = redact(m, false, depth+1)
		}
		return slog.Attr{Key: key, Value: slog.GroupValue(out...)}
	case slog.KindAny:
		return slog.Attr{Key: key, Value: anyValue(v.Any(), depth)}
	case slog.KindBool, slog.KindDuration, slog.KindFloat64, slog.KindInt64, slog.KindTime, slog.KindUint64, slog.KindLogValuer:
	}
	return slog.Attr{Key: key, Value: v}
}

// anyValue converts an arbitrary value into a slog.Value that the handlers can render without ever calling
// json.Marshal on unknown types (which would bypass redaction). It never panics: nil receivers and panicking
// methods render as placeholders, like fmt does.
func anyValue(x any, depth int) (out slog.Value) {
	defer func() {
		if r := recover(); r != nil {
			out = slog.StringValue(fmt.Sprintf("<%T panic>", x))
		}
	}()
	if isNil(x) {
		return slog.StringValue("<nil>")
	}
	if depth >= maxDepth {
		return slog.StringValue(fmt.Sprintf("<%T>", x))
	}
	switch t := x.(type) {
	case slog.LogValuer:
		return anyValue(t.LogValue().Resolve().Any(), depth+1)
	case *url.Userinfo, url.Userinfo:
		return slog.StringValue(Redacted)
	case *url.URL:
		return slog.StringValue(Scrub(t.Redacted()))
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
		keys := rv.MapKeys()
		names := make([]string, len(keys))
		byName := make(map[string]reflect.Value, len(keys))
		for i, k := range keys {
			names[i] = fmt.Sprint(k.Interface())
			byName[names[i]] = k
		}
		sort.Strings(names) // deterministic output
		attrs := make([]slog.Attr, 0, min(len(names), maxElems)+1)
		for i, n := range names {
			if i == maxElems {
				attrs = append(attrs, slog.Int("…truncated", len(names)-maxElems))
				break
			}
			attrs = append(attrs, field(n, rv.MapIndex(byName[n]).Interface(), depth))
		}
		return slog.GroupValue(attrs...)
	case reflect.Struct:
		rt := rv.Type()
		attrs := make([]slog.Attr, 0, min(rt.NumField(), maxElems))
		for i := range rt.NumField() {
			f := rt.Field(i)
			if !f.IsExported() || len(attrs) == maxElems {
				continue
			}
			name := f.Name
			if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag != "" && tag != "-" && isSecretKey(tag) {
				name = tag // the serialized name is what tells us it is a secret
			}
			attrs = append(attrs, field(name, rv.Field(i).Interface(), depth))
		}
		return slog.GroupValue(attrs...)
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 { // byte arrays are key material as often as not
			return slog.StringValue(fmt.Sprintf("[REDACTED bytes len=%d]", rv.Len()))
		}
		n := rv.Len()
		attrs := make([]slog.Attr, 0, min(n, maxElems)+1)
		for i := range min(n, maxElems) {
			attrs = append(attrs, slog.Attr{Key: strconv.Itoa(i), Value: anyValue(rv.Index(i).Interface(), depth+1)})
		}
		if n > maxElems {
			attrs = append(attrs, slog.Int("…truncated", n-maxElems))
		}
		return slog.GroupValue(attrs...)
	default:
		return slog.StringValue(fmt.Sprintf("<%T>", x))
	}
}

// field renders one named member: secret names are masked without converting the value.
func field(name string, v any, depth int) slog.Attr {
	if isSecretKey(name) {
		return slog.String(Scrub(name), Redacted)
	}
	return slog.Attr{Key: Scrub(name), Value: anyValue(v, depth+1)}
}

func isNil(x any) bool {
	if x == nil {
		return true
	}
	rv := reflect.ValueOf(x)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}
