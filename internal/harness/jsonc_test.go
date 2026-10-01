package harness

import (
	"errors"
	"testing"
)

func TestInsertRemoveMember_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		path []string
		want string
	}{
		{"empty object", "{}\n", []string{"mcpServers"}, "{\n  \"mcpServers\": {\n    \"agentfeedback\": {\n      \"url\": \"u\"\n    }\n  }\n}\n"},
		{"existing parent", "{\n  \"mcpServers\": {\n    \"other\": {}\n  }\n}\n", []string{"mcpServers"},
			"{\n  \"mcpServers\": {\n    \"other\": {},\n    \"agentfeedback\": {\n      \"url\": \"u\"\n    }\n  }\n}\n"},
		{"empty parent", "{\n  \"mcpServers\": {}\n}\n", []string{"mcpServers"},
			"{\n  \"mcpServers\": {\n    \"agentfeedback\": {\n      \"url\": \"u\"\n    }\n  }\n}\n"},
		{"comments and trailing comma", "{\n  // the servers\n  \"mcp\": {\n    \"a\": 1, // first\n  },\n}\n", []string{"mcp"},
			"{\n  // the servers\n  \"mcp\": {\n    \"a\": 1, // first\n    \"agentfeedback\": {\n      \"url\": \"u\"\n    },\n  },\n}\n"},
		{"comment after last member", "{\n  \"a\": 1 // note\n}\n", nil, "{\n  \"a\": 1, // note\n  \"agentfeedback\": {\n    \"url\": \"u\"\n  }\n}\n"},
		{"tabs", "{\n\t\"a\": {\n\t\t\"b\": 1\n\t}\n}\n", []string{"a"}, "{\n\t\"a\": {\n\t\t\"b\": 1,\n\t\t\"agentfeedback\": {\n\t\t\t\"url\": \"u\"\n\t\t}\n\t}\n}\n"},
		{"crlf", "{\r\n  \"a\": 1\r\n}\r\n", nil, "{\r\n  \"a\": 1,\r\n  \"agentfeedback\": {\r\n    \"url\": \"u\"\r\n  }\r\n}\r\n"},
		{"inline object", "{\"a\": 1}", nil, "{\"a\": 1,\n  \"agentfeedback\": {\n    \"url\": \"u\"\n  }\n}"},
		{"nested creation", "{\n  \"x\": 1\n}\n", []string{"p", "q"},
			"{\n  \"x\": 1,\n  \"p\": {\n    \"q\": {\n      \"agentfeedback\": {\n        \"url\": \"u\"\n      }\n    }\n  }\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, from, err := InsertMember([]byte(tt.doc), tt.path, "agentfeedback", []byte(`{"url":"u"}`))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.want {
				t.Fatalf("insert:\n%q\nwant\n%q", out, tt.want)
			}
			back, err := RemoveMember(out, tt.path, "agentfeedback", from)
			if err != nil {
				t.Fatal(err)
			}
			if string(back) != tt.doc {
				t.Fatalf("round trip:\n%q\nwant\n%q", back, tt.doc)
			}
		})
	}
}

func TestAppendRemoveElement_RoundTrip(t *testing.T) {
	hook := []byte(`{"hooks":[{"type":"command","command":"/bin/af flush --hook","timeout":5}]}`)
	tests := []struct {
		name string
		doc  string
	}{
		{"missing array", "{\n  \"model\": \"x\"\n}\n"},
		{"empty file object", "{}\n"},
		{"existing hooks", "{\n  \"hooks\": {\n    \"Stop\": [\n      {\n        \"hooks\": [{\"type\": \"command\", \"command\": \"other\"}]\n      }\n    ]\n  }\n}\n"},
		{"empty array", "{\n  \"hooks\": {\n    \"Stop\": []\n  }\n}\n"},
		{"inline array", "{\"hooks\": {\"Stop\": [{\"hooks\": []}]}}\n"},
		{"jsonc", "{\n  /* hooks */\n  \"hooks\": {\n    \"Stop\": [\n      {\"hooks\": []}, // keep\n    ],\n  },\n}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, from, err := AppendElement([]byte(tt.doc), []string{"hooks", "Stop"}, hook)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := HasElement(out, []string{"hooks", "Stop"}, hook); err != nil || !ok {
				t.Fatalf("element not found after insert (%v):\n%s", err, out)
			}
			back, err := RemoveElement(out, []string{"hooks", "Stop"}, hook, from)
			if err != nil {
				t.Fatal(err)
			}
			if string(back) != tt.doc {
				t.Fatalf("round trip:\n%q\nwant\n%q\nafter insert:\n%s", back, tt.doc, out)
			}
		})
	}
}

func TestEditor_Refusals(t *testing.T) {
	if _, _, err := InsertMember([]byte(`{"a": {"agentfeedback": 1}}`), []string{"a"}, "agentfeedback", []byte(`1`)); !errors.Is(err, ErrExists) {
		t.Fatalf("existing member: %v", err)
	}
	for _, doc := range []string{"", "{", `{"a": 1 "b": 2}`, `[1]`, `{"a": /* x`, `{} {}`} {
		if _, _, err := InsertMember([]byte(doc), nil, "k", []byte(`1`)); err == nil {
			t.Fatalf("%q: want an error", doc)
		}
	}
	if _, _, err := AppendElement([]byte(`{"hooks": {"Stop": {}}}`), []string{"hooks", "Stop"}, []byte(`1`)); err == nil {
		t.Fatal("non-array: want an error")
	}
}

func TestStandard(t *testing.T) {
	in := "{\"a\": \"//not a comment\", /* c */ \"b\": [1, 2,], // x\n}"
	got := string(Standard([]byte(in)))
	want := "{\"a\": \"//not a comment\",         \"b\": [1, 2 ]      \n}"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}
