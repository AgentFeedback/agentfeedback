package schema

import "testing"

func TestNormalizeDateTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"utc", "2026-09-27T09:58:12Z", "2026-09-27T09:58:12.000000Z", true},
		{"lower-case t and z", "2026-09-27t09:58:12z", "2026-09-27T09:58:12.000000Z", true},
		{"fraction kept", "2026-09-27T09:58:12.5Z", "2026-09-27T09:58:12.500000Z", true},
		{"fraction beyond six truncated", "2026-09-27T09:58:12.1234567899Z", "2026-09-27T09:58:12.123456Z", true},
		{"positive offset", "2026-09-27T09:58:12+02:00", "2026-09-27T07:58:12.000000Z", true},
		{"negative offset", "2026-09-27T23:58:12-02:30", "2026-09-28T02:28:12.000000Z", true},
		{"unknown offset -00:00 is utc", "2026-09-27T09:58:12-00:00", "2026-09-27T09:58:12.000000Z", true},
		{"offset hour 23", "2026-09-27T09:58:12+23:59", "2026-09-26T09:59:12.000000Z", true},
		{"year 0001", "0001-01-01T00:00:00Z", "0001-01-01T00:00:00.000000Z", true},
		{"year 9999", "9999-12-31T23:59:59.999999Z", "9999-12-31T23:59:59.999999Z", true},
		{"leap day", "2024-02-29T00:00:00Z", "2024-02-29T00:00:00.000000Z", true},
		{"non-leap century", "1900-02-29T00:00:00Z", "", false},
		{"leap century", "2000-02-29T00:00:00Z", "2000-02-29T00:00:00.000000Z", true},
		{"leap second", "2026-06-30T23:59:60Z", "", false},
		{"space separator", "2026-09-27 09:58:12Z", "", false},
		{"no zone", "2026-09-27T09:58:12", "", false},
		{"offset without colon", "2026-09-27T09:58:12+0200", "", false},
		{"offset hour 24", "2026-09-27T09:58:12+24:00", "", false},
		{"offset minute 60", "2026-09-27T09:58:12+01:60", "", false},
		{"empty fraction", "2026-09-27T09:58:12.Z", "", false},
		{"trailing newline", "2026-01-01T00:00:00Z\n", "", false},
		{"leading space", " 2026-01-01T00:00:00Z", "", false},
		{"month 13", "2026-13-01T00:00:00Z", "", false},
		{"day 0", "2026-01-00T00:00:00Z", "", false},
		{"day 32", "2026-01-32T00:00:00Z", "", false},
		{"hour 24", "2026-01-01T24:00:00Z", "", false},
		{"year 0000", "0000-01-01T00:00:00Z", "", false},
		{"year 0001 pushed below by offset", "0001-01-01T00:00:00+01:00", "", false},
		{"year 9999 pushed above by offset", "9999-12-31T23:30:00-01:00", "", false},
		{"non-ascii digits", "２026-01-01T00:00:00Z", "", false},
		{"two-digit year", "26-01-01T00:00:00Z", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NormalizeDateTime(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizeDateTime(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
