package canonjson

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMarshal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"null", nil, `null`},
		{"true", true, `true`},
		{"false", false, `false`},
		{"number 1.0", json.Number("1.0"), `1.0`},
		{"number 1e2", json.Number("1e2"), `1e2`},
		{"number 1E2", json.Number("1E2"), `1E2`},
		{"number -0", json.Number("-0"), `-0`},
		{"number 0.10", json.Number("0.10"), `0.10`},
		{"number 30 digits", json.Number("123456789012345678901234567890"), `123456789012345678901234567890`},

		{"empty string", "", `""`},
		{"quote", `"`, `"\""`},
		{"backslash", `\`, `"\\"`},
		{"LF", "\n", `"\n"`},
		{"CR", "\r", `"\r"`},
		{"TAB", "\t", `"\t"`},
		{"0x00", "\x00", `"\u0000"`},
		{"0x08", "\x08", `"\u0008"`},
		{"0x0C", "\x0c", `"\u000c"`},
		{"0x1F", "\x1f", `"\u001f"`},
		{"slash", "/", `"/"`},
		{"DEL", "\x7f", "\"\x7f\""},
		{"U+2028", " ", "\" \""},
		{"U+2029", " ", "\" \""},
		{"e acute", "é", `"é"`},
		{"U+1F600", "\U0001F600", "\"\U0001F600\""},
		{"U+FFFD", "�", "\"�\""},

		{"empty object", map[string]any{}, `{}`},
		{"empty array", []any{}, `[]`},
		{"key order", map[string]any{"b": json.Number("1"), "a": json.Number("2"), "ab": json.Number("3")}, `{"a":2,"ab":3,"b":1}`},
		{"utf8 byte order", map[string]any{"\U0001F600": true, "ﬁ": false}, "{\"ﬁ\":false,\"\U0001F600\":true}"},
		{"nested", map[string]any{
			"z": []any{map[string]any{"y": nil, "x": "s"}, json.Number("2")},
			"a": map[string]any{"d": true, "c": []any{}},
		}, `{"a":{"c":[],"d":true},"z":[{"x":"s","y":null},2]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Marshal(tc.in)
			if string(got) != tc.want {
				t.Fatalf("Marshal = %q, want %q", got, tc.want)
			}
			if app := Append(nil, tc.in); !bytes.Equal(app, got) {
				t.Fatalf("Append(nil) = %q, Marshal = %q", app, got)
			}
			prefixed := Append([]byte("x"), tc.in)
			if string(prefixed) != "x"+tc.want {
				t.Fatalf("Append(prefix) = %q", prefixed)
			}
		})
	}
}

func TestPanicsOnNonTreeValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   any
	}{
		{"int", 1},
		{"float64", 1.5},
		{"struct", struct{ A int }{1}},
		{"nested int", map[string]any{"a": []any{1}}},
		{"number not a JSON number", json.Number("abc")},
		{"number with leading zero", json.Number("01")},
		{"number with trailing dot", json.Number("1.")},
		{"string not UTF-8", "\xff"},
		{"nested string not UTF-8", []any{map[string]any{"a": "\xc0\x80"}}},
		{"member name not UTF-8", map[string]any{"\xff": "v"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("Marshal(%T) did not panic", tc.in)
				}
			}()
			Marshal(tc.in)
		})
	}
}

func TestConformanceCanonical(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("../../conformance/hash/*/canonical.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no conformance/hash/*/canonical.json fixtures found")
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f)), func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.HasSuffix(raw, []byte("\n")) {
				t.Fatalf("%s ends in a newline; canonical.json must carry no trailing newline", f)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				t.Fatalf("parse %s: %v", f, err)
			}
			if got := Marshal(v); !bytes.Equal(got, raw) {
				t.Fatalf("Marshal mismatch for %s\n got: %q\nwant: %q", f, got, raw)
			}
		})
	}
}

func TestSum(t *testing.T) {
	t.Parallel()
	// SHA-256 of the two bytes "{}".
	if got, want := Sum(map[string]any{}), "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"; got != want {
		t.Errorf("Sum({}) = %s, want %s", got, want)
	}
}
