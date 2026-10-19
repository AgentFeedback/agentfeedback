package envelope

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/schema"
)

func unlistedBody(members int, nameLen int) string {
	var b strings.Builder
	b.WriteString(`{"kind":"friction","summary":"s","machine":"m","model":"x","harness":"h","project":"p","payload":{"category":"tooling","details":"d"`)
	for i := 1; i <= members; i++ {
		name := fmt.Sprintf("u%03d", i)
		if nameLen > len(name) {
			name += strings.Repeat("x", nameLen-len(name))
		}
		fmt.Fprintf(&b, `,"%s":1`, name)
	}
	b.WriteString("}}")
	return b.String()
}

func TestCapWarnings_Count(t *testing.T) {
	t.Parallel()
	_, warnings, err := Decode([]byte(unlistedBody(150, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != WarningsMax {
		t.Fatalf("got %d warnings, want %d (the closing entry included)", len(warnings), WarningsMax)
	}
	for i, w := range warnings[:WarningsMax-1] {
		if want := fmt.Sprintf("/payload/u%03d", i+1); w.Code != "unknown_field" || w.Pointer != want {
			t.Fatalf("warning %d = %s %s, want unknown_field %s (pointer order)", i, w.Code, w.Pointer, want)
		}
	}
	last := warnings[WarningsMax-1]
	if last.Code != WarningsTruncated || last.Pointer != "" || last.Message != "51 more warnings withheld; the first 99 are listed in pointer order" {
		t.Fatalf("closing warning = %+v", last)
	}
}

func TestCapWarnings_Bytes(t *testing.T) {
	t.Parallel()
	// 40 members with 2000-byte names: under the count, over the byte budget.
	_, warnings, err := Decode([]byte(unlistedBody(40, 2000)))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) >= 41 || warnings[len(warnings)-1].Code != WarningsTruncated {
		t.Fatalf("got %d warnings, last %+v", len(warnings), warnings[len(warnings)-1])
	}
	used := 0
	for _, w := range warnings[:len(warnings)-1] {
		if len(w.Pointer) < 2000 {
			t.Fatalf("a pointer was shortened: %q", w.Pointer)
		}
		used += warningBytes(w)
	}
	if used += warningBytes(warnings[len(warnings)-1]); used > WarningsBytes {
		t.Fatalf("kept %d bytes with the closing entry, over %d", used, WarningsBytes)
	}
}

func TestCapWarnings_NoOpBelowTheBounds(t *testing.T) {
	t.Parallel()
	in := []schema.Detail{{Code: "b", Pointer: "/z"}, {Code: "a", Pointer: "/a"}}
	out := capWarnings(in)
	if len(out) != 2 || out[0].Pointer != "/z" {
		t.Fatalf("order changed below the bounds: %+v", out)
	}
	if out := capWarnings(nil); len(out) != 0 {
		t.Fatalf("nil in, %+v out", out)
	}
	// Equal pointer and code: the message decides, so the kept set is deterministic.
	many := make([]schema.Detail, 0, WarningsMax+5)
	for i := WarningsMax + 4; i >= 0; i-- {
		many = append(many, schema.Detail{Code: "duplicate_key", Pointer: "/payload/a", Message: fmt.Sprintf("m%03d", i)})
	}
	if out := capWarnings(many); out[0].Message != "m000" || out[WarningsMax-2].Message != fmt.Sprintf("m%03d", WarningsMax-2) {
		t.Fatalf("equal keys not ordered by message: %+v", out[:2])
	}
	_, warnings, err := Decode([]byte(unlistedBody(100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 100 || warnings[99].Code == WarningsTruncated {
		t.Fatalf("exactly WarningsMax warnings must pass uncut: %d, last %+v", len(warnings), warnings[len(warnings)-1])
	}
}
