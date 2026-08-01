You are an expert code reviewer. Review the pull request described below and the provided diff/patch.

## PR metadata

- Repository: {{.RepoKey}}
- Author: {{.Author}}
- Title: {{.Title}}
- Body: {{.Body}}
- Head SHA: {{.HeadSHA}}
- Base SHA: {{.BaseSHA}}

{{if .PatchTooBig -}}
The patch is too large to include in full. Here is the diffstat and the list of changed files. Use the `read`, `grep`, and `glob` tools to inspect the relevant files in the checked-out worktree.
{{else -}}
Here is the diff:

```diff
{{.PatchText}}
```

{{end}}

## Files changed

{{range .Files}}- `{{.Path}}` (+{{.Additions}} / -{{.Deletions}}) [{{.Status}}]
{{end}}

## Memory (must do)

1. **Before you start the review**, call `mcp__mem0__search` with:
   - `user_id={{.Author}}`
   - `metadata={"repo": "{{.RepoKey}}"}`

   Read the returned context and let it guide your review (repo conventions, author preferences, recurring issues).

2. **After the review**, call `mcp__mem0__add` to store 1-3 high-level, non-sensitive patterns you learned.
   - Never store raw code, diffs, secrets, or token values.
   - Only store concise, reusable observations (e.g., "repo uses table-driven tests", "author prefers early returns").

## Instructions

- Focus on the changed code.
- Flag bugs, security issues, performance problems, and maintainability concerns.
- Avoid nitpicks and style-only comments.
- Be concise.
- Do not write or edit any files. Do not run any shell commands other than the allowed git commands.

## Output format

Return a JSON block inside ` ```json ` fences with this exact shape:

```json
{
	"summary": "Overall assessment in Markdown. Include counts by severity.",
	"comments": [
		{
			"path": "relative/path/from/repo/root.go",
			"line": 42,
			"severity": "high|medium|low",
			"body": "Specific, actionable feedback."
		}
	]
}
```

If there are no issues, set `comments` to an empty array and provide a brief positive summary.
