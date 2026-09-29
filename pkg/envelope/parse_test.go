package envelope

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

func parseString(t *testing.T, body string) (any, []schema.Detail, error) {
	t.Helper()
	det := newDetails()
	v, err := parse([]byte(body), det, MaxDepth)
	return v, det.flatten(), err
}

func TestParseRejectsNonJSON(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"", " ", "{", "}", "[", "{]", `{"a"}`, `{"a":}`, `{"a":1,}`, `{,}`, `[1,]`, `[,1]`,
		`{'a':1}`, `{a:1}`, `{"a":1}x`, `{"a":1} {"b":2}`, "{\"a\":1}\x00",
		"-", "01", "1.", ".5", "1e", "1e+", "+1", "0x10", "NaN", "Infinity", "-Infinity",
		"tru", "True", "nul", "truex",
		`"unterminated`, "\"raw\ttab\"", "\"raw\nnewline\"", "\"\x01\"",
		`"\x"`, `"\u12"`, `"\u12G4"`, `"\ud800\u12"`, `"\ud800\uZZZZ"`, "\"\\\xc3\xa9\"",
		"\ufeff{}", "{}\ufeff", "\v{}", "\f{}", " {}",
		"[[[[]]]]]",
	} {
		if _, _, err := parseString(t, body); err == nil {
			t.Errorf("%q parsed", body)
		}
	}
}

func TestParseValues(t *testing.T) {
	t.Parallel()
	got, warnings, err := parseString(t, ` {"n":[1,-0,0.10,1e2,1E+2,-1.5e-3,123456789012345678901234567890],"s":"a\"b\\c\/d\b\f\n\r\tAé😀","t":true,"f":false,"z":null,"o":{},"a":[]} `)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"n": []any{json.Number("1"), json.Number("-0"), json.Number("0.10"), json.Number("1e2"), json.Number("1E+2"), json.Number("-1.5e-3"), json.Number("123456789012345678901234567890")},
		"s": "a\"b\\c/d\b\f\n\r\tAé😀",
		"t": true, "f": false, "z": nil,
		"o": map[string]any{}, "a": []any{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %#v, want %#v", got, want)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings on a clean body: %v", warnings)
	}
	for _, body := range []string{"1", `"s"`, "true", "null", "[1]", "1.5e3", " 0 "} {
		if _, _, err := parseString(t, body); err != nil {
			t.Errorf("%q: %v", body, err)
		}
	}
}

func TestParseDepth(t *testing.T) {
	t.Parallel()
	deep := func(levels int) string {
		return strings.Repeat("[", levels) + strings.Repeat("]", levels)
	}
	if _, _, err := parseString(t, deep(MaxDepth)); err != nil {
		t.Errorf("depth %d rejected: %v", MaxDepth, err)
	}
	if _, _, err := parseString(t, deep(MaxDepth+1)); err == nil {
		t.Errorf("depth %d accepted", MaxDepth+1)
	}
	mixed := strings.Repeat(`{"a":[`, MaxDepth/2) + strings.Repeat("]}", MaxDepth/2)
	if _, _, err := parseString(t, mixed); err != nil {
		t.Errorf("mixed depth %d rejected: %v", MaxDepth, err)
	}
	if _, _, err := parseString(t, `{"a":`+mixed+"}"); err == nil {
		t.Errorf("mixed depth %d accepted", MaxDepth+1)
	}
	// The depth is reset between siblings: many shallow containers in a row
	// are not deep.
	if _, _, err := parseString(t, "["+strings.Repeat("[],", 2000)+"[]]"); err != nil {
		t.Errorf("siblings rejected: %v", err)
	}
}

func TestParseDuplicates(t *testing.T) {
	t.Parallel()
	got, warnings, err := parseString(t, `{"a":1,"a":2,"a":3,"b":{"x":"\ud800"},"b":"clean","c":"\ud800","c":"\udc00","a":4}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"a": json.Number("4"), "b": "clean", "c": "�"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %#v, want %#v", got, want)
	}
	// One duplicate_key per member however often it repeats; the discarded
	// value's warnings go; the kept value's stay.
	if codes := keys(warnings); !reflect.DeepEqual(codes, []string{"duplicate_key /a", "duplicate_key /b", "duplicate_key /c", "invalid_utf8 /c"}) {
		t.Errorf("warnings %v", codes)
	}
}

func TestParseInvalidNameKeepsWarningOnlyWhenLastSpellingIsInvalid(t *testing.T) {
	t.Parallel()
	// The name's warning is deferred until the pointer is known and dropped
	// with the discarded value when the member repeats.
	_, warnings, err := parseString(t, "{\"a\xffb\":1,\"a�b\":2,\"c�\":1,\"c\xff\":2}")
	if err != nil {
		t.Fatal(err)
	}
	if codes := keys(warnings); !reflect.DeepEqual(codes, []string{"duplicate_key /a�b", "duplicate_key /c�", "invalid_utf8 /c�"}) {
		t.Errorf("warnings %v", codes)
	}
}

func TestParseArrayPointers(t *testing.T) {
	t.Parallel()
	_, warnings, err := parseString(t, `{"a":["x","\udc00",{"k":"\ud800"}],"b/c":"\ud800","~":"\ud800"}`)
	if err != nil {
		t.Fatal(err)
	}
	if codes := keys(warnings); !reflect.DeepEqual(codes, []string{"invalid_utf8 /a/1", "invalid_utf8 /a/2/k", "invalid_utf8 /b~1c", "invalid_utf8 /~0"}) {
		t.Errorf("warnings %v", codes)
	}
}

func keys(warnings []schema.Detail) []string {
	var out []string
	for _, d := range warnings {
		out = append(out, d.Code+" "+d.Pointer)
	}
	return out
}

func TestUnescapeSurrogates(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want string
		lone bool
	}{
		{`😀`, "😀", false},
		{`\ud800`, "�", true},
		{`\udc00`, "�", true},
		{`\ud800\ud800`, "��", true},
		{`\ud800A`, "�A", true},
		{`\ud800x`, "�x", true},
		{`􏿿`, "\U0010FFFF", false},
		{`�`, "�", false},
		{`\u0000`, "\x00", false},
		{`a\/b`, "a/b", false},
	} {
		got, lone, err := unescape(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want || lone != c.lone {
			t.Errorf("%q: got %q lone=%v, want %q lone=%v", c.in, got, lone, c.want, c.lone)
		}
	}
	if _, _, err := unescape(`\ud800\u12`); err == nil {
		t.Error("truncated low-surrogate escape accepted")
	}
}

// Vectors from Unicode ch. 3, table 3-8 and the WHATWG decoder, with the
// exact number of replacements.
func TestRepairUTF8Vectors(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"é😀", "é😀"},
		{"\x80", "�"},
		{"\xc0\x80", "��"},      // overlong: C0 is never a lead byte
		{"\xc1\xbf", "��"},      // overlong
		{"\xe0\x80\x80", "���"}, // overlong 3-byte: second byte below A0
		{"\xed\xa0\x80", "���"}, // encoded surrogate
		{"\xed\x9f\xbf", "퟿"},   // U+D7FF, the last code point before the surrogates
		{"\xf0\x80\x80\x80", "����"},
		{"\xf4\x90\x80\x80", "����"}, // above U+10FFFF
		{"\xf5\x80", "��"},
		{"\xf8\x88\x80\x80\x80", "�����"},
		{"\xff", "�"},
		{"\xc2", "�"},         // truncated at end
		{"\xe1\x80", "�"},     // truncated at end: one maximal subpart
		{"\xf0\x9f", "�"},     // truncated at end
		{"\xf0\x9f\x98", "�"}, // truncated at end
		{"\xe1\x80x", "�x"},   // truncated before ASCII
		{"\xf1\x80\x80\xe1\x80\xc2b\x80c\x80\xbfd", "���b�c��d"}, // table 3-8
		{"\x61\xf1\x80\x80\xe1\x80\xc2\x62\x80\x63\x80\xbf\x64", "a���b�c��d"},
		{"\xc2\xa0\xc2", " �"},
		{"\xe2\x82", "�"},
		{"\xe2\x82\xac", "€"},
		{"\xe2\x82\xe2\x82\xac", "�€"},
	} {
		got, replaced := repairUTF8([]byte(c.in))
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
		if replaced != (c.in != c.want) {
			t.Errorf("%q: replaced=%v", c.in, replaced)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%q: repaired text is not valid UTF-8", c.in)
		}
	}
}

// oracleRepair is a second, independent formulation of the same rule: at
// each position, the longest prefix of a well-formed sequence (table 3-7's
// byte ranges) is one unit; a complete one decodes, an incomplete one is one
// U+FFFD, and a byte that starts nothing is one U+FFFD.
func oracleRepair(b []byte) string {
	type rng struct{ lo, hi byte }
	shapes := func(lead byte) []rng {
		switch {
		case lead >= 0xC2 && lead <= 0xDF:
			return []rng{{0x80, 0xBF}}
		case lead == 0xE0:
			return []rng{{0xA0, 0xBF}, {0x80, 0xBF}}
		case lead >= 0xE1 && lead <= 0xEC, lead >= 0xEE && lead <= 0xEF:
			return []rng{{0x80, 0xBF}, {0x80, 0xBF}}
		case lead == 0xED:
			return []rng{{0x80, 0x9F}, {0x80, 0xBF}}
		case lead == 0xF0:
			return []rng{{0x90, 0xBF}, {0x80, 0xBF}, {0x80, 0xBF}}
		case lead >= 0xF1 && lead <= 0xF3:
			return []rng{{0x80, 0xBF}, {0x80, 0xBF}, {0x80, 0xBF}}
		case lead == 0xF4:
			return []rng{{0x80, 0x8F}, {0x80, 0xBF}, {0x80, 0xBF}}
		}
		return nil
	}
	var out []byte
	for i := 0; i < len(b); {
		if b[i] < 0x80 {
			out = append(out, b[i])
			i++
			continue
		}
		trail := shapes(b[i])
		if trail == nil {
			out = append(out, "�"...)
			i++
			continue
		}
		n := 1
		for n <= len(trail) && i+n < len(b) && b[i+n] >= trail[n-1].lo && b[i+n] <= trail[n-1].hi {
			n++
		}
		if n == len(trail)+1 {
			r, _ := utf8.DecodeRune(b[i : i+n])
			out = utf8.AppendRune(out, r)
		} else {
			out = append(out, "�"...)
		}
		i += n
	}
	return string(out)
}

func TestRepairUTF8AgainstOracle(t *testing.T) {
	t.Parallel()
	check := func(b []byte) {
		if got, want := oracleRepair(b), func() string { s, _ := repairUTF8(b); return s }(); got != want {
			t.Errorf("%q: repair %q, oracle %q", b, want, got)
		}
	}
	// Every one- and two-byte sequence.
	for a := 0; a < 256; a++ {
		check([]byte{byte(a)})
		for b := 0; b < 256; b++ {
			check([]byte{byte(a), byte(b)})
		}
	}
	// Every three-byte sequence over the bytes that shape a sequence.
	interesting := []byte{0x00, 0x41, 0x7F, 0x80, 0x8F, 0x90, 0x9F, 0xA0, 0xBF, 0xC0, 0xC1, 0xC2, 0xDF, 0xE0, 0xE1, 0xEC, 0xED, 0xEE, 0xEF, 0xF0, 0xF1, 0xF3, 0xF4, 0xF5, 0xF8, 0xFF}
	for _, a := range interesting {
		for _, b := range interesting {
			for _, c := range interesting {
				check([]byte{a, b, c})
				for _, d := range interesting {
					check([]byte{a, b, c, d})
				}
			}
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 20000; i++ {
		b := make([]byte, r.IntN(12))
		for j := range b {
			b[j] = interesting[r.IntN(len(interesting))]
		}
		check(b)
	}
}

func TestParseNumberBoundaries(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ body, want string }{
		{"[0]", "0"}, {"[-0]", "-0"}, {"[1e400]", "1e400"}, {"[1E-400]", "1E-400"}, {"[0.0]", "0.0"}, {"[10]", "10"},
	} {
		v, _, err := parseString(t, c.body)
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if got := v.([]any)[0].(json.Number); string(got) != c.want {
			t.Errorf("%s: %q", c.body, got)
		}
	}
	for _, body := range []string{"[1.]", "[1.e5]", "[1e]", "[1e+]", "[-]", "[--1]", "[00]", "[1 2]"} {
		if _, _, err := parseString(t, body); err == nil {
			t.Errorf("%s parsed", body)
		}
	}
}

func TestRejectionIsAnError(t *testing.T) {
	t.Parallel()
	_, _, err := Decode([]byte("nope"))
	var r *Rejection
	if !errors.As(err, &r) || r.Status != 400 || r.Code != "bad_request" || !strings.Contains(err.Error(), "bad_request") {
		t.Fatalf("err = %v", err)
	}
}
