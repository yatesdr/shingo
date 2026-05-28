package shared

import (
	"net/url"
	"strings"
)

// SafeNextPath validates a `next=` redirect target for the post-login
// bounce on Core and Edge auth flows. Returns the path unchanged when
// it's a local same-origin reference; returns "" when the value is
// empty, off-origin, or otherwise unsafe (open-redirect guard).
//
// Rejects: empty string, non-`/`-prefixed values, protocol-relative
// (`//evil.example`) and backslash variants (`/\evil`), any URL that
// parses with a scheme or host, javascript: URLs (caught by the scheme
// check).
//
// Same byte-for-byte logic lived inline in shingo-core/www/auth.go and
// shingo-edge/www/auth.go; promoted here so an open-redirect guard
// can't silently drift between the two surfaces.
func SafeNextPath(raw string) string {
	if raw == "" {
		return ""
	}
	if !strings.HasPrefix(raw, "/") {
		return ""
	}
	// Protocol-relative ("//evil.example") and backslash variants are
	// disallowed — browsers may treat them as absolute URLs.
	if strings.HasPrefix(raw, "//") || strings.HasPrefix(raw, "/\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "" || u.Host != "" {
		return ""
	}
	return raw
}
