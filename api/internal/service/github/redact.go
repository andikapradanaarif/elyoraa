package github

import "regexp"

// redactSecrets strips credential-shaped text from anything the bot posts to
// GitHub. The agent reads untrusted input (PR diffs, comments); if hostile text
// convinces it to copy a secret into its output, this is the last line of
// defense before the content goes public.
var secretPatterns = []*regexp.Regexp{
	// PEM-encoded private keys (GitHub App key, TLS keys).
	regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	// GitHub tokens: PATs, OAuth, user-to-server, server-to-server, refresh, fine-grained.
	regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{16,}\b`),
	// Slack tokens.
	regexp.MustCompile(`\bxox[bapors]-[A-Za-z0-9-]{10,}\b`),
	// AWS access key IDs.
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	// OpenAI / Anthropic-style API keys.
	regexp.MustCompile(`\bsk-(proj|ant)-[A-Za-z0-9_-]{16,}\b`),
	// Devin CLI credentials.toml material.
	regexp.MustCompile(`(?i)(access_token|refresh_token|session_token)\s*=\s*"[^"]{8,}"`),
}

// redactSecrets replaces secret-shaped substrings with a marker. Applied to
// every outbound body at the GitHub client boundary.
func redactSecrets(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}
