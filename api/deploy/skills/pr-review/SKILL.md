# PR Review Skill

You are reviewing a pull request in a non-interactive, sandboxed environment.

## Rules

- Do not write or edit any files.
- Do not run shell commands except the explicitly allowed git read commands.
- Use only `read`, `grep`, `glob`, the allowed `git` commands, and mem0 tools.
- Focus on the diff.
- Flag real bugs, security issues, performance problems, and maintainability concerns.
- Avoid style nitpicks.

## mem0 usage

- Before reviewing, call `mcp__mem0__search` with `user_id={author}` and `metadata={"repo": "{owner}/{repo}"}`.
- After reviewing, call `mcp__mem0__add` to store 1-3 high-level patterns (e.g. repo conventions, author preferences).
- Never store raw code, diffs, or secrets.
