package core

import (
	"strings"
	"testing"

	"github.com/agentfeedback/agentfeedback/v4/pkg/envelope"
)

func TestCapImportWarnings(t *testing.T) {
	t.Parallel()
	var res ImportResult
	for line := 1; line <= 3; line++ {
		for i := 0; i < 60; i++ {
			res.Warnings = append(res.Warnings, ImportWarning{Line: line, Code: "unknown_field", Pointer: "/payload/x", Message: "m"})
		}
	}
	out := capImportWarnings(res)
	if len(out.Warnings) != envelope.WarningsMax {
		t.Fatalf("got %d warnings, want %d (the closing entry included)", len(out.Warnings), envelope.WarningsMax)
	}
	last := out.Warnings[envelope.WarningsMax-1]
	if last.Code != envelope.WarningsTruncated || last.Line != 2 || last.Pointer != "" || last.Message != "81 more warnings withheld; the first 99 are listed in line order" {
		t.Fatalf("closing warning = %+v", last)
	}
	if len(capImportWarnings(ImportResult{Warnings: out.Warnings[:100]}).Warnings) != 100 {
		t.Fatal("exactly WarningsMax import warnings must pass uncut")
	}
	// Two records under the count but over the byte budget together.
	long := ImportResult{}
	for line := 1; line <= 2; line++ {
		for i := 0; i < 20; i++ {
			long.Warnings = append(long.Warnings, ImportWarning{Line: line, Code: "unknown_field", Pointer: "/payload/" + strings.Repeat("x", 2000)})
		}
	}
	cut := capImportWarnings(long)
	if n := len(cut.Warnings); n >= 40 || cut.Warnings[n-1].Code != envelope.WarningsTruncated {
		t.Fatalf("byte budget not applied: %d warnings", n)
	}
	used := 0
	for _, w := range cut.Warnings {
		used += len(w.Code) + len(w.Pointer)
	}
	if used > envelope.WarningsBytes {
		t.Fatalf("kept %d bytes, over %d", used, envelope.WarningsBytes)
	}
}
