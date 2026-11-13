package platform

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrCobrowseURLNotAllowed is returned when a co-browse start or navigate URL
// is not an absolute http(s) URL or about:blank.
var ErrCobrowseURLNotAllowed = errors.New("co-browse URL must be an absolute http or https URL, or about:blank")

// ValidateCobrowseURL decides which URLs a co-browse session may start on or
// navigate to. The empty string means "no URL" and is allowed. Everything else
// must be about:blank or an absolute http/https URL with a host. Other schemes
// (file, data, javascript, chrome, ...) and anything that is not a URL at all,
// such as a value beginning with "-" or "/", are refused.
func ValidateCobrowseURL(raw string) error {
	if raw == "" {
		return nil
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("%w: contains whitespace or control characters", ErrCobrowseURLNotAllowed)
	}
	if raw == "about:blank" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCobrowseURLNotAllowed, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ErrCobrowseURLNotAllowed
	}
	if u.Host == "" || u.Opaque != "" {
		return ErrCobrowseURLNotAllowed
	}
	return nil
}
