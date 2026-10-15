package github

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA7\n-----END RSA PRIVATE KEY-----"

	cases := []struct {
		name, in, wantSub string
	}{
		{"pem", "leak: " + pem, "[redacted]"},
		{"pat", "token ghp_abcdefghijklmnopqrstuvwxyz123456 here", "[redacted]"},
		{"fine-grained pat", "github_pat_11AAAAAA0abcdefghijklmnopqrstuvwxyzABCDEFG", "[redacted]"},
		{"slack", "xoxb-1234567890123-abcdefghijkl", "[redacted]"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "[redacted]"},
		{"openai", "sk-proj-abcdefghijklmnopqrstuvwxyz123456", "[redacted]"},
		{"toml creds", `access_token = "deadbeefcafe123456"`, "[redacted]"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := redactSecrets(tc.in)
			if !strings.Contains(out, tc.wantSub) {
				t.Fatalf("redactSecrets(%q) = %q, want marker %q", tc.in, out, tc.wantSub)
			}
			for _, leak := range []string{"PRIVATE KEY-----\nMIIE", "ghp_abc", "xoxb-1234", "AKIAIOSFO", "sk-proj-abc"} {
				if strings.Contains(out, leak) {
					t.Fatalf("output still contains secret material %q: %q", leak, out)
				}
			}
		})
	}
}

func TestRedactSecrets_Passthrough(t *testing.T) {
	in := "LGTM — minor nit on the session timeout handling in auth/session.go."
	if out := redactSecrets(in); out != in {
		t.Fatalf("ordinary text must pass through: %q", out)
	}
	// Short sk- prefixes are not keys.
	in2 := "use sk-ant matcher, not sk-"
	if out := redactSecrets(in2); out != in2 {
		t.Fatalf("non-secret sk- text must pass through: %q", out)
	}
}
