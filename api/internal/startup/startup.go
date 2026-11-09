package startup

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/andikapradanaarif/elyoraa/internal/config"
)

// CheckResult is a single validation result.
type CheckResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Check validates all runtime prerequisites and returns the results.
func Check(cfg *config.Config) []CheckResult {
	var results []CheckResult

	results = append(results, checkBinary("git", "git"))
	results = append(results, checkBinary("crush", cfg.CrushBin))
	results = append(results, checkFile("github-app-private-key", cfg.GitHubAppPrivateKeyPath))
	results = append(results, checkWritableDir("workspace", cfg.WorkspaceBase))
	results = append(results, checkCrushConfigDir(cfg.CrushConfigDir))

	return results
}

// HasFailures returns true if any check failed.
func HasFailures(results []CheckResult) bool {
	for _, r := range results {
		if !r.OK {
			return true
		}
	}
	return false
}

func checkBinary(name, path string) CheckResult {
	if path == "" {
		return CheckResult{Name: name, OK: false, Message: "no path configured"}
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return CheckResult{Name: name, OK: false, Message: fmt.Sprintf("not found in PATH: %s", path)}
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return CheckResult{Name: name, OK: false, Message: err.Error()}
	}
	if info.IsDir() {
		return CheckResult{Name: name, OK: false, Message: "path is a directory"}
	}
	return CheckResult{Name: name, OK: true, Message: resolved}
}

func checkFile(name, path string) CheckResult {
	if path == "" {
		return CheckResult{Name: name, OK: false, Message: "no path configured"}
	}
	info, err := os.Stat(path)
	if err != nil {
		return CheckResult{Name: name, OK: false, Message: err.Error()}
	}
	if info.IsDir() {
		return CheckResult{Name: name, OK: false, Message: "path is a directory"}
	}
	return CheckResult{Name: name, OK: true, Message: path}
}

func checkWritableDir(name, path string) CheckResult {
	if path == "" {
		return CheckResult{Name: name, OK: false, Message: "no path configured"}
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return CheckResult{Name: name, OK: false, Message: err.Error()}
	}
	tmp := filepath.Join(path, ".elyoraa-write-test")
	if err := os.WriteFile(tmp, []byte("ok"), 0o600); err != nil {
		return CheckResult{Name: name, OK: false, Message: err.Error()}
	}
	_ = os.Remove(tmp)
	return CheckResult{Name: name, OK: true, Message: path}
}

// Global Crush configuration may be mounted read-only; only auth data and
// per-job state need writes. Create the conventional directory when absent.
func checkCrushConfigDir(dir string) CheckResult {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return CheckResult{Name: "crush-config-dir", Message: err.Error()}
	}
	if _, err := os.ReadDir(dir); err != nil {
		return CheckResult{Name: "crush-config-dir", Message: err.Error()}
	}
	return CheckResult{Name: "crush-config-dir", OK: true, Message: dir}
}

// LogResults logs each check at the appropriate level.
func LogResults(results []CheckResult) {
	for _, r := range results {
		if r.OK {
			slog.Info("startup check passed", "name", r.Name, "detail", r.Message)
		} else {
			slog.Error("startup check failed", "name", r.Name, "detail", r.Message)
		}
	}
}
