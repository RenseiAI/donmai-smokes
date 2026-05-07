package harness

import "testing"

// ── ContainsAny ───────────────────────────────────────────────────────────────

func TestContainsAny(t *testing.T) {
	if !ContainsAny("hello world", "world", "missing") {
		t.Error("ContainsAny should find 'world'")
	}
	if ContainsAny("hello", "xyz", "abc") {
		t.Error("ContainsAny should return false when no needle matches")
	}
	if ContainsAny("hello", "") {
		t.Error("ContainsAny with empty needle should return false")
	}
}

// ── SafePrefix ────────────────────────────────────────────────────────────────

func TestSafePrefix(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"hello", 3, "hel…"},
		{"hi", 10, "hi"},
		{"", 5, ""},
		{"abcdef", 6, "abcdef"},
	}
	for _, tc := range cases {
		got := SafePrefix(tc.s, tc.n)
		if got != tc.want {
			t.Errorf("SafePrefix(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

// ── StripANSI ─────────────────────────────────────────────────────────────────

func TestStripANSI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{"prefix\x1b[1;33mbold-yellow\x1b[mtail", "prefixbold-yellowtail"},
	}
	for _, tc := range cases {
		got := StripANSI(tc.in)
		if got != tc.want {
			t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
