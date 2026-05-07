package harness

import "testing"

// TestExtractTokenByPrefix exercises every documented edge of the helper:
// empty input, no match, single-line / multi-line matches, first-of-many
// preference, the strict length boundary (len > minLength, not >=), and
// surrounding whitespace handling.
func TestExtractTokenByPrefix(t *testing.T) {
	cases := []struct {
		name      string
		output    string
		prefix    string
		minLength int
		want      string
	}{
		{
			name:      "empty input returns empty",
			output:    "",
			prefix:    "rsk_",
			minLength: 4,
			want:      "",
		},
		{
			name:      "no match returns empty",
			output:    "hello world\nthis line has no token\n",
			prefix:    "rsk_",
			minLength: 4,
			want:      "",
		},
		{
			name:      "single match on a single line",
			output:    "Created key: rsk_live_abc123",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_live_abc123",
		},
		{
			name:      "multi-line input matches in second line",
			output:    "first line has nothing\nsecond line has rsk_live_xyz789 inside",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_live_xyz789",
		},
		{
			name:      "multiple matches returns the first",
			output:    "rsk_live_first second\nrsk_live_third",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_live_first",
		},
		{
			name:      "match at minLength boundary returns empty (strict >)",
			output:    "token: rsk_",
			prefix:    "rsk_",
			minLength: 4,
			want:      "",
		},
		{
			name:      "match longer than minLength returns the token",
			output:    "token: rsk_x",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_x",
		},
		{
			name:      "leading whitespace before token still matches",
			output:    "    \trsk_live_indent",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_live_indent",
		},
		{
			name:      "trailing whitespace after token still matches",
			output:    "rsk_live_trailing    \t  \n",
			prefix:    "rsk_",
			minLength: 4,
			want:      "rsk_live_trailing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractTokenByPrefix(tc.output, tc.prefix, tc.minLength)
			if got != tc.want {
				t.Errorf("ExtractTokenByPrefix(%q, %q, %d) = %q, want %q",
					tc.output, tc.prefix, tc.minLength, got, tc.want)
			}
		})
	}
}
