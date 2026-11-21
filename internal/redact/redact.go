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
	"auth", "pwd", "pass", "bearer", "jwt",
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
		// The tail of a base64 user:password pair decodes to the end of the
		// password, so Basic gets no fingerprint at all.
		if strings.EqualFold(scheme, "basic") {
			return scheme + " <redacted>"
		}
		return scheme + " " + Fingerprint(strings.TrimSpace(rest))
	}
	if IsSensitiveName(name) {
		return Fingerprint(value)
	}
	return value
}

// URL returns diagnostics with credential-bearing positions hidden by default.
// Paths are omitted, and every query value is fingerprinted, including unknown
// names such as OAuth code. Scheme, host and query names are kept. On a parse
// failure it returns "<unparseable url>", never the original string.
func URL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<unparseable url>"
	}
	// A path can contain a webhook credential. Neither the decoded path nor
	// its retained escaped form may reach a diagnostic. Opaque URLs are not
	// supported diagnostic endpoints either.
	u.Path, u.RawPath, u.Opaque = "", "", ""
	// url.URL.String() percent-escapes the fingerprint characters; the output
	// is for humans, so the readable form is restored at the end.
	var restore []string // pairs of escaped, readable
	if u.User != nil {
		name := u.User.Username()
		pw, hasPw := u.User.Password()
		// Any username may itself be a credential, even when it is short.
		nameFP := Fingerprint(name)
		restore = append(restore, url.User(nameFP).String(), nameFP)
		name = nameFP
		if hasPw {
			fp := Fingerprint(pw)
			restore = append(restore, strings.TrimPrefix(url.UserPassword("", fp).String(), ":"), fp)
			pw = fp
			u.User = url.UserPassword(name, pw)
		} else {
			u.User = url.User(name)
		}
	}
	if u.RawQuery != "" {
		parts := strings.Split(u.RawQuery, "&")
		for i, part := range parts {
			k, v, hasValue := strings.Cut(part, "=")
			if !hasValue {
				// A bare query can itself be a credential, with no field name.
				parts[i] = "query=" + Fingerprint(part)
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
	for i := 0; i+1 < len(restore); i += 2 {
		out = strings.Replace(out, restore[i], restore[i+1], 1)
	}
	return out
}
