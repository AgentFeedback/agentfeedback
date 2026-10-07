package scrub

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Fixtures are built from repeated characters so no real credential, and
// nothing a secret scanner would take for one, is in the source.
var (
	fakeJWT    = "eyJhbGciOi." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12)
	fakeAWS    = "AKIAIOSFODNN7EXAMPLE"
	fakeGCP    = "AIza" + strings.Repeat("x", 35)
	fakeGH     = "ghp_" + strings.Repeat("a", 36)
	fakeGHPAT  = "github_pat_" + strings.Repeat("a", 22)
	fakeGitlab = "glpat-" + strings.Repeat("a", 20)
	fakeSlack  = "xoxb-" + strings.Repeat("1", 10)
	fakeStripe = "sk_test_" + strings.Repeat("a", 16)
	fakeAnth   = "sk-ant-" + strings.Repeat("a", 20)
	fakeOpenAI = "sk-proj-" + strings.Repeat("a", 20)
	fakeBearer = strings.Repeat("t", 20)
	fakePEM    = "-----BEGIN RSA PRIVATE KEY-----\nAAAA\nBBBB\n-----END RSA PRIVATE KEY-----"
)

func TestString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
		counts         Counts
	}{
		{"private key block", "key:\n" + fakePEM + "\ndone", "key:\n[REDACTED:private_key]\ndone", Counts{"private_key": 1}},
		{"private key without end", "x -----BEGIN PRIVATE KEY-----\nAAAA", "x [REDACTED:private_key]", Counts{"private_key": 1}},
		{"public key untouched", "-----BEGIN PUBLIC KEY-----\nAAAA", "-----BEGIN PUBLIC KEY-----\nAAAA", Counts{}},
		{"jwt", "tok " + fakeJWT + " end", "tok [REDACTED:jwt] end", Counts{"jwt": 1}},
		{"jwt short segment", "eyJabc.def.ghi", "eyJabc.def.ghi", Counts{}},
		{"aws", "id " + fakeAWS, "id [REDACTED:aws_access_key]", Counts{"aws_access_key": 1}},
		{"aws too short", "AKIA1234", "AKIA1234", Counts{}},
		{"gcp", fakeGCP, "[REDACTED:gcp_api_key]", Counts{"gcp_api_key": 1}},
		{"gcp too short", "AIzaabc", "AIzaabc", Counts{}},
		{"github classic", fakeGH, "[REDACTED:github_token]", Counts{"github_token": 1}},
		{"github pat", fakeGHPAT, "[REDACTED:github_token]", Counts{"github_token": 1}},
		{"github too short", "ghp_abc", "ghp_abc", Counts{}},
		{"gitlab", fakeGitlab, "[REDACTED:gitlab_token]", Counts{"gitlab_token": 1}},
		{"gitlab too short", "glpat-abc", "glpat-abc", Counts{}},
		{"slack", fakeSlack, "[REDACTED:slack_token]", Counts{"slack_token": 1}},
		{"slack too short", "xoxb-1", "xoxb-1", Counts{}},
		{"stripe", fakeStripe, "[REDACTED:stripe_key]", Counts{"stripe_key": 1}},
		{"stripe wrong mode", "sk_prod_" + strings.Repeat("a", 16), "sk_prod_" + strings.Repeat("a", 16), Counts{}},
		{"anthropic", fakeAnth, "[REDACTED:anthropic_key]", Counts{"anthropic_key": 1}},
		{"anthropic too short", "sk-ant-abc", "sk-ant-abc", Counts{}},
		{"openai", fakeOpenAI, "[REDACTED:openai_key]", Counts{"openai_key": 1}},
		{"openai inside a word", "task-" + strings.Repeat("a", 20), "task-" + strings.Repeat("a", 20), Counts{}},
		{"bearer keeps prefix", "Authorization: Bearer " + fakeBearer, "Authorization: Bearer [REDACTED:bearer_token]", Counts{"bearer_token": 1}},
		{"bearer lower case", "bearer  " + fakeBearer, "bearer  [REDACTED:bearer_token]", Counts{"bearer_token": 1}},
		{"bearer short", "Bearer abc", "Bearer abc", Counts{}},
		{"bearer jwt counted once", "Bearer " + fakeJWT, "Bearer [REDACTED:jwt]", Counts{"jwt": 1}},
		{"url credentials", "git clone https://bob:hunter2@example.com/r.git", "git clone https://[REDACTED:url_credentials]@example.com/r.git", Counts{"url_credentials": 1}},
		{"url without password", "https://bob@example.com", "https://bob@example.com", Counts{}},
		{"url with token password", "https://x:" + fakeGH + "@example.com", "https://[REDACTED:url_credentials]@example.com", Counts{"github_token": 1, "url_credentials": 1}},
		{"url password with @", "https://user:p@ss@host/x@y", "https://[REDACTED:url_credentials]@host/x@y", Counts{"url_credentials": 1}},
		{"env line bearer jwt", "AUTH_TOKEN=Bearer " + fakeJWT, "AUTH_TOKEN=[REDACTED:env_line]", Counts{"jwt": 1, "env_line": 1}},
		{"env line dollar secret", "PASSWORD=$ecret123", "PASSWORD=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env line var ref", "TOKEN=$TOKEN", "TOKEN=$TOKEN", Counts{}},
		{"env line quoted with trailer", "SECRET='a b' # note", "SECRET='[REDACTED:env_line]' # note", Counts{"env_line": 1}},
		{"env name MY_API_KEY", "MY_API_KEY=x", "MY_API_KEY=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env name GITHUB_TOKEN", "GITHUB_TOKEN=x", "GITHUB_TOKEN=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env name DB_PASSWORD", "DB_PASSWORD=x", "DB_PASSWORD=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env name APIKEY", "APIKEY=x", "APIKEY=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env name AUTHOR", "AUTHOR=x", "AUTHOR=x", Counts{}},
		{"env name KEYSTONE_PATH", "KEYSTONE_PATH=x", "KEYSTONE_PATH=x", Counts{}},
		{"env name PWD", "PWD=/home/x", "PWD=/home/x", Counts{}},
		{"assignment bcrypt hash", "password=$2y$10$abcdefgh", "password=[REDACTED:assignment]", Counts{"assignment": 1}},
		{"assignment marker inside", "password=x[REDACTED:y]realsecret", "password=[REDACTED:assignment]", Counts{"assignment": 1}},
		{"assignment quoted with spaces", `password="correct horse battery"`, `password="[REDACTED:assignment]"`, Counts{"assignment": 1}},
		{"assignment single quoted", "token: 'a b'", "token: '[REDACTED:assignment]'", Counts{"assignment": 1}},
		{"env line", "A=1\nexport DB_PASSWORD=\"s3cr3t\"\nB=2", "A=1\nexport DB_PASSWORD=\"[REDACTED:env_line]\"\nB=2", Counts{"env_line": 1}},
		{"env line unquoted", "MY_API_KEY=abc def", "MY_API_KEY=[REDACTED:env_line]", Counts{"env_line": 1}},
		{"env line other name", "HOME=/home/x", "HOME=/home/x", Counts{}},
		{"env line reference", "API_TOKEN=$OTHER", "API_TOKEN=$OTHER", Counts{}},
		{"env line not at line start", "x API_TOKEN=abc", "x API_TOKEN=[REDACTED:assignment]", Counts{"assignment": 1}},
		{"assignment", "password=hunter2&x=1", "password=[REDACTED:assignment]&x=1", Counts{"assignment": 1}},
		{"assignment quoted json", `{"client_secret": "abc"}`, `{"client_secret": "[REDACTED:assignment]"}`, Counts{"assignment": 1}},
		{"assignment api-key", "api-key: abc123", "api-key: [REDACTED:assignment]", Counts{"assignment": 1}},
		{"assignment reference", "token=${TOKEN}", "token=${TOKEN}", Counts{}},
		{"working directory is not a secret", "PWD=/home/u/repo\nOLDPWD=/tmp\npwd: /home/u", "PWD=/home/u/repo\nOLDPWD=/tmp\npwd: /home/u", Counts{}},
		{"max_tokens is not a secret", "max_tokens=1000", "max_tokens=1000", Counts{}},
		{"tokens is not a secret", "tokens: 5", "tokens: 5", Counts{}},
		{"plain prose", "the build failed on step 3", "the build failed on step 3", Counts{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, counts := String(tc.in)
			if got != tc.want {
				t.Fatalf("String(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if counts.String() != tc.counts.String() {
				t.Fatalf("counts = %q, want %q", counts, tc.counts)
			}
			again, more := String(got)
			if again != got || more.Total() != 0 {
				t.Fatalf("not idempotent: %q -> %q (%v)", got, again, more)
			}
		})
	}
}

func TestClassesOrder(t *testing.T) {
	t.Parallel()
	got := strings.Join(Classes(), ",")
	want := "private_key,jwt,aws_access_key,gcp_api_key,github_token,gitlab_token,slack_token,stripe_key,anthropic_key,openai_key,bearer_token,url_credentials,env_line,assignment"
	if got != want {
		t.Fatalf("Classes() = %s, want %s", got, want)
	}
}

func TestCounts(t *testing.T) {
	t.Parallel()
	c := Counts{"jwt": 1}
	c.Add(Counts{"jwt": 1, "bearer_token": 1, "aws_access_key": 0})
	if got := c.String(); got != "bearer_token 1, jwt 2" {
		t.Fatalf("String() = %q", got)
	}
	if c.Total() != 3 {
		t.Fatalf("Total() = %d", c.Total())
	}
	if (Counts{}).String() != "" {
		t.Fatal("empty counts not empty")
	}
}

func TestTree(t *testing.T) {
	t.Parallel()
	tree := map[string]any{
		"details": "Bearer " + fakeBearer,
		"nested":  map[string]any{fakeAWS: []any{fakeGH, json.Number("1.0"), true, nil}},
	}
	counts := Tree(tree)
	if got := counts.String(); got != "bearer_token 1, github_token 1" {
		t.Fatalf("counts = %q", got)
	}
	if tree["details"] != "Bearer [REDACTED:bearer_token]" {
		t.Fatalf("details = %v", tree["details"])
	}
	items := tree["nested"].(map[string]any)[fakeAWS].([]any)
	if items[0] != "[REDACTED:github_token]" || items[1] != json.Number("1.0") {
		t.Fatalf("items = %v (the key must stay)", items)
	}
	if Tree("Bearer "+fakeBearer).Total() != 0 {
		t.Fatal("a bare string cannot change in place")
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()
	in := []byte(`{"b": 1.0, "a": [1e3, 123456789012345678901234567890, "` + fakeAWS + `"], "a": "x<y", "` + fakeGH + `": {"k": "Bearer ` + fakeBearer + `"}, "n": null, "t": false}`)
	got, counts, err := JSON(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"b":1.0,"a":[1e3,123456789012345678901234567890,"[REDACTED:aws_access_key]"],"a":"x<y","` + fakeGH + `":{"k":"Bearer [REDACTED:bearer_token]"},"n":null,"t":false}`
	if string(got) != want {
		t.Fatalf("JSON =\n%s\nwant\n%s", got, want)
	}
	if counts.String() != "aws_access_key 1, bearer_token 1" {
		t.Fatalf("counts = %q", counts)
	}
	again, more, err := JSON(got)
	if err != nil || !bytes.Equal(again, got) || more.Total() != 0 {
		t.Fatalf("not idempotent: %s %v %v", again, more, err)
	}
}

func TestJSONNoMatchReturnsInput(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`{ "a" : 1.0 , "a": "x" }`, `"plain"`, `[1e3, true]`, `12`} {
		got, counts, err := JSON([]byte(in))
		if err != nil || string(got) != in || counts.Total() != 0 {
			t.Fatalf("JSON(%s) = %s %v %v", in, got, counts, err)
		}
	}
	if _, _, err := JSON([]byte(`{"a":`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestJSONTopLevelString(t *testing.T) {
	t.Parallel()
	got, counts, err := JSON([]byte(`"` + fakeAWS + `"`))
	if err != nil || string(got) != `"[REDACTED:aws_access_key]"` || counts.Total() != 1 {
		t.Fatalf("JSON = %s %v %v", got, counts, err)
	}
}

func TestSecretNamedMembers(t *testing.T) {
	t.Parallel()
	in := `{"password":"hunter2","Client_Secret":"abc","max_tokens":"5","key":"abc","tokens":"x","token":"$TOKEN","api-key":"","x":[{"auth_token":"a"}]}`
	want := `{"password":"[REDACTED:assignment]","Client_Secret":"[REDACTED:assignment]","max_tokens":"5","key":"abc","tokens":"x","token":"$TOKEN","api-key":"","x":[{"auth_token":"[REDACTED:assignment]"}]}`
	got, counts, err := JSON([]byte(in))
	if err != nil || string(got) != want || counts["assignment"] != 3 || counts.Total() != 3 {
		t.Fatalf("JSON = %s %v %v", got, counts, err)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(in), &tree); err != nil {
		t.Fatal(err)
	}
	if c := Tree(tree); c["assignment"] != 3 || c.Total() != 3 {
		t.Fatalf("Tree counts %v", c)
	}
	if tree["password"] != "[REDACTED:assignment]" || tree["max_tokens"] != "5" || tree["key"] != "abc" || tree["tokens"] != "x" || tree["token"] != "$TOKEN" {
		t.Fatalf("Tree = %v", tree)
	}
}
