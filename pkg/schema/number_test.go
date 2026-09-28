package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseDecimalSpellings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in    string
		ok    bool
		isInt bool
	}{
		{"0", true, true}, {"-0", true, true}, {"0.0", true, true}, {"0e5", true, true},
		{"1", true, true}, {"2.0", true, true}, {"1e2", true, true}, {"1E+2", true, true},
		{"10", true, true}, {"1.5", true, false}, {"1e-1", true, false}, {"100e-2", true, true},
		{"-1.50", true, false}, {"1.23e2", true, true}, {"1.234e2", true, false},
		{strings.Repeat("9", 5000), true, true}, {"1e" + strings.Repeat("9", 5000), true, true},
		{"1e-" + strings.Repeat("9", 5000), true, false},
		{"01", false, false}, {"1.", false, false}, {".5", false, false}, {"+1", false, false},
		{"1e", false, false}, {"--1", false, false}, {"", false, false}, {"abc", false, false},
		{"NaN", false, false}, {"1 ", false, false},
	}
	for _, tc := range tests {
		d, ok := parseDecimal(tc.in)
		if ok != tc.ok {
			t.Errorf("parseDecimal(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && d.isInt() != tc.isInt {
			t.Errorf("parseDecimal(%q).isInt() = %v, want %v", tc.in, d.isInt(), tc.isInt)
		}
	}
}

func TestDecimalCompare(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want int
	}{
		{"0", "0", 0}, {"-0", "0", 0}, {"0.0", "-0e9", 0},
		{"2.0", "2", 0}, {"1e2", "100", 0}, {"0.10", "0.1", 0}, {"-1.0", "-1", 0},
		{"1", "2", -1}, {"2", "1", 1}, {"-1", "1", -1}, {"1", "-1", 1},
		{"-2", "-1", -1}, {"-1", "-2", 1},
		{"0", "1", -1}, {"0", "-1", 1}, {"1", "0", 1}, {"-1", "0", -1},
		{"9", "10", -1}, {"10", "9", 1}, {"123", "13", 1}, {"12", "123", -1}, {"1.5", "1.25", 1},
		{"0.5", "1", -1}, {"1e-1", "0.2", -1}, {"1.2e1", "12", 0},
		{"1e" + strings.Repeat("9", 30), "1e18", 1}, {"1e-" + strings.Repeat("9", 30), "1e-18", -1},
		{"1e" + strings.Repeat("9", 30), "1e" + strings.Repeat("9", 40), -1},
		{"1e100000000000000000000", "1e100000000000000000001", -1},
		{"10e99999999999999999999", "1e100000000000000000000", 0},
		{"9e99999999999999999999", "1e100000000000000000000", -1},
		{"1e-100000000000000000000", "1e-99999999999999999999", -1},
		{"-1e100000000000000000000", "-1e100000000000000000001", 1},
		{"1.5e100000000000000000000", "15e99999999999999999999", 0},
		{"1e" + strings.Repeat("9", 5000), "2e" + strings.Repeat("9", 5000), -1},
		{"5", "4.999999999999999999999999", 1}, {"1", "1.000000000000000000000001", -1},
	}
	for _, tc := range tests {
		a, ok1 := parseDecimal(tc.a)
		b, ok2 := parseDecimal(tc.b)
		if !ok1 || !ok2 {
			t.Fatalf("parse %q %q", tc.a, tc.b)
		}
		if got := a.cmp(b); got != tc.want {
			t.Errorf("cmp(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := b.cmp(a); got != -tc.want {
			t.Errorf("cmp(%q, %q) = %d, want %d", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestTypeOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   any
		want string
	}{
		{nil, "null"}, {true, "boolean"}, {"s", "string"}, {[]any{}, "array"}, {map[string]any{}, "object"},
		{json.Number("1"), "integer"}, {json.Number("2.0"), "integer"}, {json.Number("1e2"), "integer"},
		{json.Number("1.5"), "number"}, {json.Number("1e-2"), "number"}, {json.Number("-0"), "integer"},
		{json.Number("garbage"), "number"},
		{float64(3), "integer"}, {2.5, "number"}, {7, "integer"}, {int64(-1), "integer"}, {uint64(1), "integer"},
	}
	for _, tc := range tests {
		if got := typeOf(tc.in); got != tc.want {
			t.Errorf("typeOf(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTypeOfPanicsOnForeignValues(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a struct value")
		}
	}()
	typeOf(struct{}{})
}

func TestJSONEqual(t *testing.T) {
	t.Parallel()
	n := func(s string) json.Number { return json.Number(s) }
	tests := []struct {
		a, b any
		want bool
	}{
		{n("2.0"), n("2"), true}, {n("1e2"), n("100"), true}, {n("1"), n("1.5"), false},
		{n("1"), "1", false}, {"1", n("1"), false}, {"a", "a", true}, {"a", "b", false},
		{true, true, true}, {true, false, false}, {nil, nil, true}, {nil, false, false},
		{[]any{n("1"), "x"}, []any{n("1.0"), "x"}, true}, {[]any{n("1")}, []any{n("1"), n("2")}, false},
		{map[string]any{"a": n("1")}, map[string]any{"a": n("1.0")}, true},
		{map[string]any{"a": n("1")}, map[string]any{"b": n("1")}, false},
		{n("bad"), n("1"), false}, {2, n("2.0"), true},
	}
	for _, tc := range tests {
		if got := jsonEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("jsonEqual(%#v, %#v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestBigIntArithmetic(t *testing.T) {
	t.Parallel()
	tests := []struct{ a, b, sum string }{
		{"0", "0", "0"}, {"1", "2", "3"}, {"999", "1", "1000"}, {"1", "999", "1000"},
		{"-1", "1", "0"}, {"1", "-1", "0"}, {"-5", "3", "-2"}, {"5", "-3", "2"}, {"-5", "-3", "-8"},
		{"1000", "-1", "999"}, {"-1000", "1", "-999"}, {"10", "-100", "-90"},
		{"99999999999999999999", "1", "100000000000000000000"},
		{"100000000000000000000", "-1", "99999999999999999999"},
	}
	parse := func(s string) bigInt {
		neg := strings.HasPrefix(s, "-")
		return bigFromDigits(strings.TrimPrefix(s, "-"), neg)
	}
	render := func(b bigInt) string {
		if b.isZero() {
			return "0"
		}
		if b.neg {
			return "-" + b.mag
		}
		return b.mag
	}
	for _, tc := range tests {
		if got := render(parse(tc.a).add(parse(tc.b))); got != tc.sum {
			t.Errorf("%s + %s = %s, want %s", tc.a, tc.b, got, tc.sum)
		}
		if got := render(parse(tc.sum).sub(parse(tc.b))); got != render(parse(tc.a)) {
			t.Errorf("%s - %s = %s, want %s", tc.sum, tc.b, got, tc.a)
		}
	}
	if n, ok := parse("-123").small(); !ok || n != -123 {
		t.Fatalf("small = %d, %v", n, ok)
	}
	if _, ok := parse(strings.Repeat("9", 19)).small(); ok {
		t.Fatal("19 digits should not be small")
	}
	if bigFromInt(0).neg || bigFromInt(-7).mag != "7" || !bigFromInt(-7).neg {
		t.Fatal("bigFromInt")
	}
}
