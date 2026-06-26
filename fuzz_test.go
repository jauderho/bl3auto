package bl3auto

import (
	"net/http"
	"testing"
)

// FuzzParseCodeList exercises the SHiFT code-list JSON parser against arbitrary
// bytes. parseCodeList consumes untrusted remote input (the published code
// lists), so it must never panic regardless of how malformed the body is.
func FuzzParseCodeList(f *testing.F) {
	f.Add([]byte(`[{"codes":[{"code":"ABC-123","game":"bl3","expired":false}]}]`), true)
	f.Add([]byte(`[{"codes":[{"code":"X","expired":true},{"code":"Y"}]}]`), false)
	f.Add([]byte(`[]`), true)
	f.Add([]byte(`{}`), false)
	f.Add([]byte(`null`), true)
	f.Add([]byte(``), false)
	f.Add([]byte(`[{"codes":"notanarray"}]`), true)

	f.Fuzz(func(t *testing.T, body []byte, dropExpired bool) {
		// The result is intentionally ignored; we only assert it does not panic.
		_ = parseCodeList(body, dropExpired)
	})
}

// FuzzStatusFromText exercises the redemption-status text classifier, which maps
// arbitrary SHiFT alert/status strings to outcomes.
func FuzzStatusFromText(f *testing.F) {
	f.Add("Success! Your code was redeemed", true)
	f.Add("This code has already been redeemed", false)
	f.Add("This SHiFT code has expired", true)
	f.Add("", false)
	f.Add("launch a SHiFT-enabled title", true)

	f.Fuzz(func(t *testing.T, text string, visited bool) {
		_ = statusFromText(text, visited)
	})
}

// FuzzParseRetryAfter exercises the Retry-After header parser (delta-seconds or
// HTTP-date) against arbitrary header values.
func FuzzParseRetryAfter(f *testing.F) {
	f.Add("120")
	f.Add("-5")
	f.Add("Wed, 21 Oct 2099 07:28:00 GMT")
	f.Add("garbage")
	f.Add("")

	f.Fuzz(func(t *testing.T, value string) {
		h := http.Header{}
		h.Set("Retry-After", value)
		_ = parseRetryAfter(h)
	})
}
