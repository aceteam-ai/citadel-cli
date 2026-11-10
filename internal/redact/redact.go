// Package redact owns the rule for keeping credentials out of logs: a secret
// is never printed in full, but a short fingerprint (its last four characters)
// is kept so an operator can tell which credential was used or rejected.
package redact

import (
	"net/url"
	"strings"
)

const (
	fingerprintMinLen = 16
	fingerprintTail   = 4
)

// Fingerprint returns "…" plus the last four characters of secret when it is
// at least 16 characters long, "<redacted>" for shorter non-empty secrets
// (a tail would leak too much of them), and "<empty>" for an empty string.
// It counts runes, not bytes.
func Fingerprint(secret string) string {
	if secret == "" {
		return "<empty>"
	}
	r := []rune(secret)
	if len(r) < fingerprintMinLen {
		return "<redacted>"
	}
	return "…" + string(r[len(r)-fingerprintTail:])
}

var sensitiveExact = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

var sensitiveParts = []string{
	"token", "secret", "key", "password", "passwd",
	"sig", "signature", "credential", "session",
}

// IsSensitiveName reports whether a header or query parameter name probably
// carries a credential. The match is case-insensitive and deliberately broad
// (any name containing "key" counts): it fails toward redaction.
func IsSensitiveName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if sensitiveExact[n] {
		return true
	}
	for _, p := range sensitiveParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
}

// Header returns value in a form safe to log. Authorization and
// Proxy-Authorization keep their scheme word ("Bearer …a1b2"); other
// sensitive names are fingerprinted whole; the rest pass through unchanged.
func Header(name, value string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "authorization" || n == "proxy-authorization" {
		scheme, rest, found := strings.Cut(strings.TrimSpace(value), " ")
		if !found {
			return Fingerprint(value)
		}
		return scheme + " " + Fingerprint(strings.TrimSpace(rest))
	}
	if IsSensitiveName(name) {
		return Fingerprint(value)
	}
	return value
}

// URL returns raw with a userinfo password and the values of sensitive query
// parameters replaced by fingerprints. Scheme, host and path are kept. On a
// parse failure it returns "<unparseable url>", never the original string.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	// url.URL.String() percent-escapes the fingerprint characters; the output
	// is for humans, so the readable form is restored at the end.
	var restore []string
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			fp := Fingerprint(pw)
			u.User = url.UserPassword(u.User.Username(), fp)
			if esc := (&url.URL{User: url.UserPassword("", fp)}).String(); strings.HasPrefix(esc, "//:") {
				restore = append(restore, strings.TrimSuffix(strings.TrimPrefix(esc, "//:"), "@"), fp)
			}
		}
	}
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			k, v, hasValue := strings.Cut(part, "=")
			name, err := url.QueryUnescape(k)
			if err != nil {
				name = k
			}
			if !IsSensitiveName(name) || !hasValue {
				continue
			}
			val, err := url.QueryUnescape(v)
			if err != nil {
				val = v
			}
			// Written raw for readability; the output is for logs, not for re-parsing.
			parts[i] = k + "=" + Fingerprint(val)
		}
		u.RawQuery = strings.Join(parts, "&")
	}
	// Fragments can carry tokens; drop them.
	if u.Fragment != "" {
		u.Fragment = "redacted"
		u.RawFragment = ""
	}
	out := u.String()
	if len(restore) == 2 {
		out = strings.Replace(out, restore[0], restore[1], 1)
	}
	return out
}
