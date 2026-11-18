package usecase

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Crush's permissions are tool-level, not Devin's path/command deny rules.
// Disable native tools and expose only server-enforced repository operations.
// Keep in sync with allToolNames in the pinned Crush fork.
var crushDisabledTools = []string{
	"agent", "agent_dispatch", "agent_send", "agent_status", "agent_stop",
	"bash", "crush_info", "crush_logs", "job_output", "job_kill", "download",
	"edit", "multiedit", "lsp_diagnostics", "lsp_references", "lsp_restart",
	"lsp_symbols", "lsp_definition", "lsp_call_hierarchy", "lsp_rename",
	"lsp_replace_symbol", "fetch", "agentic_fetch", "glob", "grep", "ls",
	"question", "sourcegraph", "todos", "view", "write", "list_mcp_resources",
	"read_mcp_resource", "map", "recall", "notebook_search",
}

func writeCrushConfig(dir string, repo *crushRepo) error {
	settings := map[string]any{}
	settings["options"] = map[string]any{
		"disabled_tools": crushDisabledTools,
		"context_paths":  []string{}, "global_context_paths": []string{},
		"skills_paths": []string{}, "disable_metrics": true, "auto_lsp": false,
		"disable_provider_auto_update": true, "project_index": false,
		"notebook_hydration": false, "progress": false,
		// Pin the fork's experimental arms off so a future default flip
		// can't change run behavior under a review job.
		"edge_advisor": false, "evidence_gate": false, "fresh_review": false,
		"find_bugs_review": false, "fold_turns": false, "fold_segments": false,
	}
	settings["permissions"] = map[string]any{"allowed_tools": []string{
		"mcp_elyoraa_read_file", "mcp_elyoraa_list_files", "mcp_elyoraa_search", "mcp_elyoraa_git", "mcp_elyoraa_sync",
		"mcp_mem0_search", "mcp_mem0_add",
	}}
	mcp := map[string]any{"elyoraa": map[string]any{
		"type": "http", "url": repo.url, "sessionless": true,
		"headers": map[string]string{"Authorization": "Bearer " + repo.token},
	}}
	if key := os.Getenv("MEM0_API_KEY"); key != "" {
		mcp["mem0"] = map[string]any{"type": "http", "url": "https://mcp.mem0.ai/mcp", "headers": map[string]string{"Authorization": "Bearer $MEM0_API_KEY"}}
	}
	settings["mcp"] = mcp
	// A closed tool boundary also covers new native tools in later fork builds
	// and additional MCP servers in the user's trusted global configuration.
	settings["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{
		"name": "elyoraa-repository-only", "timeout": 5,
		"command": `case "$CRUSH_TOOL_NAME" in mcp_elyoraa_read_file|mcp_elyoraa_list_files|mcp_elyoraa_search|mcp_elyoraa_git|mcp_elyoraa_sync|mcp_mem0_*) exit 0 ;; *) echo 'Tool unavailable in elyoraa review mode' >&2; exit 2 ;; esac`,
	}}}

	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "crush.json"), b, 0o600)
}

// crushRepo implements the small stateless Streamable HTTP MCP subset needed
// by Crush. The bearer token and listener live only for this run.
type crushRepo struct {
	url, token, root string
	server           *http.Server
	rootFS           *os.Root
	sync             func(context.Context) error
}

func startCrushRepo(ctx context.Context, worktree string, sync func(context.Context) error) (*crushRepo, error) {
	root, err := os.OpenRoot(worktree)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		_ = root.Close()
		_ = listener.Close()
		return nil, err
	}
	r := &crushRepo{url: "http://" + listener.Addr().String() + "/mcp", token: hex.EncodeToString(token[:]), root: worktree, rootFS: root, sync: sync}
	r.server = &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = r.server.Serve(listener) }()
	return r, nil
}

func (r *crushRepo) Close() { _ = r.server.Close(); _ = r.rootFS.Close() }

func (r *crushRepo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Authorization") != "Bearer "+r.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&call); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	if len(call.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch call.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(call.Params, &params)
		result = map[string]any{"protocolVersion": params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "elyoraa", "version": "1.0.0"}}
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = map[string]any{"tools": crushRepoTools()}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		var out string
		err := json.Unmarshal(call.Params, &params)
		if err == nil {
			out, err = r.call(req.Context(), params.Name, params.Arguments)
		}
		if err != nil {
			out = err.Error()
		}
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": out}}, "isError": err != nil}
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

func crushRepoTools() []any {
	definitions := []struct {
		name, description string
		properties        map[string]any
	}{
		{"read_file", "Read a repository-relative file (up to 128 KiB). Optional offset is a byte offset.", map[string]any{"path": map[string]string{"type": "string"}, "offset": map[string]string{"type": "integer"}}},
		{"list_files", "List repository files. Optional path filters by prefix.", map[string]any{"path": map[string]string{"type": "string"}}},
		{"search", "Search repository text using a Go regular expression. Optional path filters by prefix.", map[string]any{"pattern": map[string]string{"type": "string"}, "path": map[string]string{"type": "string"}}},
		{"git", "Read Git history with argv, no shell. Commands: diff, show, log, status, ls-files, ls-tree, rev-parse, merge-base, blame. Mutations, remote commands, external diff/textconv, and arbitrary flags are prohibited.", map[string]any{"args": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}},
		{"sync", "Refresh this repository's refs using server-held credentials.", map[string]any{}},
	}
	var tools []any
	for _, d := range definitions {
		tools = append(tools, map[string]any{"name": d.name, "description": d.description, "inputSchema": map[string]any{"type": "object", "properties": d.properties, "additionalProperties": false}})
	}
	return tools
}

func crushSensitivePath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		lower := strings.ToLower(part)
		if part == ".." || lower == ".git" || lower == ".env" || strings.HasSuffix(lower, ".env") || strings.HasPrefix(lower, ".env.") || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") {
			return true
		}
	}
	return filepath.IsAbs(path)
}

func (r *crushRepo) read(path string, offset int64) (string, error) {
	if crushSensitivePath(path) || offset < 0 {
		return "", errors.New("path or offset is not allowed")
	}
	// Do not follow symlinks, even inside the root: a link can alias .env.
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for i := range parts {
		info, err := r.rootFS.Lstat(filepath.Join(parts[:i+1]...))
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlinks are not allowed")
		}
	}
	f, err := r.rootFS.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, 128<<10))
	out := string(b)
	if info.Size() > offset+int64(len(b)) {
		out += fmt.Sprintf("\n[truncated; read_file offset=%d to continue]", offset+int64(len(b)))
	}
	return out, err
}

func (r *crushRepo) call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	var p struct {
		Path, Pattern string
		Offset        int64
		Args          []string
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	switch name {
	case "read_file":
		return r.read(p.Path, p.Offset)
	case "sync":
		if r.sync == nil {
			return "", errors.New("sync unavailable")
		}
		if err := r.sync(ctx); err != nil {
			return "", err
		}
		return "Repository refs refreshed.", nil
	case "git":
		return r.git(ctx, p.Args)
	case "list_files", "search":
		if crushSensitivePath(p.Path) {
			return "", errors.New("path is not allowed")
		}
		var pattern *regexp.Regexp
		var err error
		if name == "search" {
			pattern, err = regexp.Compile(p.Pattern)
			if err != nil {
				return "", err
			}
		}
		var out strings.Builder
		count := 0
		err = filepath.WalkDir(r.root, func(path string, entry os.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if out.Len() >= 128<<10 || count >= 10000 {
				return filepath.SkipAll
			}
			if walkErr != nil {
				return nil
			}
			rel, err := filepath.Rel(r.root, path)
			if err != nil {
				return err
			}
			if crushSensitivePath(rel) || entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			if !strings.HasPrefix(filepath.ToSlash(rel), p.Path) {
				return nil
			}
			count++
			if pattern == nil {
				fmt.Fprintln(&out, filepath.ToSlash(rel))
				return nil
			}
			body, err := r.read(rel, 0)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(body, "\n") {
				if pattern.MatchString(line) {
					fmt.Fprintf(&out, "%s:%d:%s\n", filepath.ToSlash(rel), i+1, line)
					if out.Len() >= 128<<10 {
						return filepath.SkipAll
					}
				}
			}
			return nil
		})
		if out.Len() >= 128<<10 || count >= 10000 {
			out.WriteString("\n[truncated; narrow the path prefix or search pattern]\n")
		}
		return out.String(), err
	default:
		return "", errors.New("unknown repository tool")
	}
}

// A closed argv vocabulary avoids shell injection and Git flags that invoke
// commands or write files. Git config/environment and pagers are scrubbed.
func (r *crushRepo) git(ctx context.Context, args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("git args required")
	}
	allowedCommands := map[string]bool{"diff": true, "show": true, "log": true, "status": true, "ls-files": true, "ls-tree": true, "rev-parse": true, "merge-base": true, "blame": true}
	allowedFlags := map[string]bool{"--": true, "--stat": true, "--numstat": true, "--name-only": true, "--name-status": true, "--oneline": true, "--no-patch": true, "--patch": true, "--short": true, "--porcelain": true, "--all": true, "--first-parent": true, "--reverse": true, "--topo-order": true, "--raw": true, "-r": true, "-p": true, "--abbrev-ref": true, "--show-toplevel": true}
	if !allowedCommands[args[0]] {
		return "", errors.New("git command is not allowed")
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-") && !allowedFlags[arg] && !regexp.MustCompile(`^(--max-count=|-n|-U)[0-9]{1,4}$`).MatchString(arg) {
			return "", fmt.Errorf("git flag %q is not allowed", arg)
		}
		if strings.ContainsAny(arg, "\x00\r\n") {
			return "", errors.New("invalid git argument")
		}
		if crushSensitivePath(arg) {
			return "", errors.New("sensitive git path is not allowed")
		}
		// A show revision such as HEAD:.env must obey the same path policy.
		if _, path, ok := strings.Cut(arg, ":"); ok && crushSensitivePath(path) {
			return "", errors.New("sensitive git object path is not allowed")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prefix := []string{"--no-pager", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "diff.external=", args[0]}
	if args[0] == "diff" || args[0] == "show" || args[0] == "log" {
		prefix = append(prefix, "--no-ext-diff", "--no-textconv")
	}
	cmd := exec.CommandContext(ctx, "git", append(prefix, args[1:]...)...)
	cmd.Dir = r.root
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0"}
	cmd.WaitDelay = time.Second
	// Bound memory while still draining stdout/stderr so Git cannot block.
	out := &crushOutput{}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	return out.String(), err
}

type crushOutput struct{ strings.Builder }

func (w *crushOutput) Write(p []byte) (int, error) {
	n := len(p)
	if left := (128 << 10) - w.Len(); left > 0 {
		if len(p) > left {
			p = p[:left]
		}
		_, _ = w.Builder.Write(p)
	}
	return n, nil
}
