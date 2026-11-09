package config

import (
	"path/filepath"
	"testing"
)

func TestCrushSettingsDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"CRUSH_GLOBAL_CONFIG", "CRUSH_GLOBAL_DATA", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(key, "")
	}
	if got := crushConfigDir(); got != filepath.Join(home, ".config", "crush") {
		t.Fatalf("config directory: %s", got)
	}
	if got := crushDataDir(); got != filepath.Join(home, ".local", "share", "crush") {
		t.Fatalf("data directory: %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "xdg-data"))
	if got := crushConfigDir(); got != filepath.Join(home, "xdg-config", "crush") {
		t.Fatalf("XDG config directory: %s", got)
	}
	if got := crushDataDir(); got != filepath.Join(home, "xdg-data", "crush") {
		t.Fatalf("XDG data directory: %s", got)
	}
	t.Setenv("CRUSH_GLOBAL_CONFIG", filepath.Join(home, "custom-config"))
	t.Setenv("CRUSH_GLOBAL_DATA", filepath.Join(home, "custom-data"))
	if got := crushConfigDir(); got != filepath.Join(home, "custom-config") {
		t.Fatalf("Crush config override: %s", got)
	}
	if got := crushDataDir(); got != filepath.Join(home, "custom-data") {
		t.Fatalf("Crush data override: %s", got)
	}
}

func TestCrushConfigurationDefaultsAndOverrides(t *testing.T) {
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "/test/key.pem")
	t.Setenv("GITHUB_WEBHOOK_SECRET", "test")
	for _, key := range []string{"CRUSH_BIN", "CRUSH_MODEL", "CRUSH_CONFIG_DIR", "CRUSH_AUTH_DATA_DIR"} {
		t.Setenv(key, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrushBin != "crush" || cfg.CrushModel != "" {
		t.Fatalf("runtime defaults: binary=%q model=%q", cfg.CrushBin, cfg.CrushModel)
	}
	t.Setenv("CRUSH_BIN", "/custom/crush")
	t.Setenv("CRUSH_MODEL", "provider/model")
	t.Setenv("CRUSH_CONFIG_DIR", "/custom/config")
	t.Setenv("CRUSH_AUTH_DATA_DIR", "/custom/data")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CrushBin != "/custom/crush" || cfg.CrushModel != "provider/model" || cfg.CrushConfigDir != "/custom/config" || cfg.CrushDataDir != "/custom/data" {
		t.Fatal("Crush environment overrides were not applied")
	}
}
