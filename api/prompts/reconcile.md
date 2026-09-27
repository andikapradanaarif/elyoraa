You are performing a focused re-verification pass on a pull request. A previous review flagged findings; since then the code may have changed. A SEPARATE fresh review already ran on the current code — your job is only to check the previous findings and merge anything still real. Do NOT produce a new review and do NOT review unrelated code.

## PR metadata

- Repository: {{.RepoKey}}
- PR #: {{.PRNumber}}
- Title: {{.Title}}
- Head SHA: {{.HeadSHA}}

## Previous review findings

{{range .Findings}}- `{{.Path}}:{{.Line}}`{{if .Outdated}} *(outdated — the diff has moved)*{{end}} — {{.Body}}{{range .Replies}}
  - @{{.Author}}: {{.Body}}{{end}}
{{end}}

## Fresh review already completed

A new review just ran on the CURRENT code and produced these comments (the fresh reviewer did not see the previous findings):

{{if .FreshComments}}{{range .FreshComments}}- `{{.Path}}:{{.Line}}` [{{.Severity}}] — {{.Body}}
{{end}}{{else}}(none — the fresh review found no issues){{end}}

## Task

For EACH previous finding:

1. Trace the CURRENT code in the worktree — the diff may have moved or changed the lines. Open the files; do not guess.
2. Classify the finding:
   - `fixed` — the current code resolves it. Only mark fixed when you can point to the specific code that resolves it. Do NOT mark fixed merely because the line moved or you can no longer find the old code.
   - `still_present` — the issue is still real in the current code.
   - `unclear` — you cannot determine either way (explain briefly in the body of `notes` if you use it).
3. For each `still_present` finding, check whether a fresh comment already covers the SAME root cause (same file and area — line numbers may differ). If covered, do not emit a comment for it. If NOT covered, emit a new comment anchored to the CURRENT line where the issue lives.
4. Replies on a finding may clarify intent (author said "won't fix" or explained it is intentional). Factor that into severity, but do NOT drop a genuine HIGH-severity issue just because it was ignored.
5. Do not modify any files. Read-only inspection.

## Output format

Return a JSON block inside ` ```json ` fences with this exact shape:

```json
{
	"prior_findings_status": [
		{
			"path": "path/of/prior/finding.go",
			"line": 42,
			"status": "fixed|still_present|unclear"
		}
	],
	"additional_comments": [
		{
			"path": "relative/path/from/repo/root.go",
			"line": 42,
			"severity": "high|medium|low",
			"body": "Specific, actionable feedback, anchored to the current line."
		}
	]
}
```

`prior_findings_status` must contain one entry for EVERY previous finding — none may be omitted. `additional_comments` contains only still-present findings that the fresh review did NOT already cover; it may be empty.
