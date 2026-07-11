package bl3auto

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// v1 (orcicorn) fixture: top-level array, [0].codes, no "expired" field.
const v1Fixture = `[
  {
    "meta": {"version": "1"},
    "codes": [
      {"code": "AAAAA-BBBBB-CCCCC-DDDDD-EEEEE", "type": "shift", "game": "Borderlands 3", "platform": "Universal", "reward": "5 Golden Keys", "expires": "Unknown"},
      {"code": "FFFFF-GGGGG-HHHHH-IIIII-JJJJJ", "type": "shift", "game": "Borderlands 2", "platform": "Steam", "reward": "3 Golden Keys", "expires": "Unknown"}
    ]
  }
]`

// v2 (ugoogalizer) fixture: same array shape but entries carry an "expired" flag.
const v2Fixture = `[
  {
    "meta": {"version": "2"},
    "codes": [
      {"code": "BZFBT-WT9S3-WR3BC-3TJ3B-HSFBB", "type": "shift", "game": "Borderlands 4", "platform": "universal", "reward": "3 Golden Keys", "expired": false},
      {"code": "OLD11-OLD22-OLD33-OLD44-OLD55", "type": "shift", "game": "Borderlands 4", "platform": "steam", "reward": "1 Golden Key", "expired": true},
      {"code": "NEW11-NEW22-NEW33-NEW44-NEW55", "type": "shift", "game": "Borderlands 2", "platform": "epic", "reward": "10 Golden Keys", "expired": false}
    ]
  }
]`

func TestParseCodeListV1(t *testing.T) {
	codes := parseCodeList([]byte(v1Fixture), false)
	if len(codes) != 2 {
		t.Fatalf("expected 2 codes, got %d", len(codes))
	}
	if codes[0].Code != "AAAAA-BBBBB-CCCCC-DDDDD-EEEEE" || codes[0].Game != "Borderlands 3" {
		t.Errorf("unexpected first code: %+v", codes[0])
	}
}

func TestParseCodeListV2DropsExpired(t *testing.T) {
	codes := parseCodeList([]byte(v2Fixture), true)
	if len(codes) != 2 {
		t.Fatalf("expected 2 non-expired codes, got %d", len(codes))
	}
	for _, c := range codes {
		if c.Expired {
			t.Errorf("expired code leaked through: %+v", c)
		}
	}
	if codes[0].Game != "Borderlands 4" {
		t.Errorf("expected Borderlands 4 first, got %q", codes[0].Game)
	}
}

func TestDedupeCodes(t *testing.T) {
	in := []ShiftCode{
		{Code: "abcde-fghij-klmno-pqrst-uvwxy"},
		{Code: "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY"}, // same as above, different case
		{Code: "  "},
		{Code: "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ"},
	}
	out := dedupeCodes(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 unique codes, got %d (%+v)", len(out), out)
	}
	if out[0].Code != "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY" {
		t.Errorf("expected normalized uppercase code, got %q", out[0].Code)
	}
}

func TestServiceMatches(t *testing.T) {
	cases := []struct {
		filter  []string
		service string
		want    bool
	}{
		{nil, "steam", true},                      // empty filter matches all
		{[]string{"steam"}, "steam", true},        // exact
		{[]string{"psn"}, "psn_3", true},          // substring
		{[]string{"PSN"}, "psn", true},            // case-insensitive
		{[]string{"steam"}, "epic", false},        // no match
		{[]string{"epic", "steam"}, "epic", true}, // one of many
	}
	for _, tc := range cases {
		if got := ServiceMatches(tc.filter, tc.service); got != tc.want {
			t.Errorf("ServiceMatches(%v, %q) = %v, want %v", tc.filter, tc.service, got, tc.want)
		}
	}
}

func TestStatusFromText(t *testing.T) {
	if err := statusFromText("Your code was successfully redeemed", true); err != nil {
		t.Errorf("success text should be nil, got %v", err)
	}
	if err := statusFromText("This SHiFT code has already been redeemed", false); err == nil {
		t.Error("already-redeemed should be an error")
	}
	if err := statusFromText("This code has expired", false); err == nil {
		t.Error("expired should be an error")
	}
}

// trackedConn wraps a net.Conn and counts Close calls, so a test can tell
// whether the client actually closed the underlying TCP connection.
type trackedConn struct {
	net.Conn
	closed *int32
}

func (c *trackedConn) Close() error {
	atomic.AddInt32(c.closed, 1)
	return c.Conn.Close()
}

// TestResolveRedemptionClosesFinalBodyOnRedirectExhaustion proves that when all 10
// follow-redirect iterations are exhausted (every response is a 301/302/303), the
// 11th response fetched at the end of the last iteration still has its body closed
// before "too many redemption redirects" is returned. A custom Transport.DialContext
// wraps every dialed connection to count Close calls: leaving a response body open
// leaves its underlying connection open too (net/http closes the connection, rather
// than pooling it, when a body is closed without first being drained), so a leaked
// body shows up directly as dialed > closed.
func TestResolveRedemptionClosesFinalBodyOnRedirectExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/next")
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, "<html>redirecting...</html>")
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	var dialed, closed int32
	c.Client.Transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			atomic.AddInt32(&dialed, 1)
			return &trackedConn{Conn: conn, closed: &closed}, nil
		},
	}

	res, err := c.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("initial GET: %v", err)
	}

	err = c.resolveRedemption(t.Context(), res)
	if err == nil || !strings.Contains(err.Error(), "too many redemption redirects") {
		t.Fatalf("expected too-many-redirects error, got %v", err)
	}

	if dialed == 0 {
		t.Fatal("expected at least one connection to have been dialed")
	}
	if got := atomic.LoadInt32(&closed); got != dialed {
		t.Errorf("expected every dialed connection to be closed (dialed=%d, closed=%d); "+
			"a shortfall means the final redirect response's body leaked its connection",
			dialed, got)
	}
}
