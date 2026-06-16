package bl3auto

import "testing"

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
