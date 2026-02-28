package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// --- detectInputFile ---

func TestDetectInputFile(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-i", "input.mkv", "-f", "mp4", "out.mp4"}, "input.mkv"},
		{[]string{"-f", "mp4", "out.mp4"}, ""},
		{[]string{"-i"}, ""},
		{[]string{"-vf", "scale=1280:720", "-i", "/data/file.mkv", "out.mp4"}, "/data/file.mkv"},
	}
	for _, c := range cases {
		got := detectInputFile(c.args)
		if got != c.want {
			t.Errorf("detectInputFile(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

// --- detectOutputFile ---

func TestDetectOutputFile(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-i", "input.mkv", "-f", "mp4", "output.mp4"}, "output.mp4"},
		{[]string{"-i", "input.mkv", "-vf", "scale=1280:720", "-f", "mp4", "output.mp4"}, "output.mp4"},
		{[]string{"-i", "input.mkv"}, ""},
		// last arg is input (after -i), so no output
		{[]string{"-f", "mp4", "-i", "input.mkv"}, ""},
		// last non-flag arg that is not the -i value
		{[]string{"-i", "a.mkv", "b.mp4"}, "b.mp4"},
	}
	for _, c := range cases {
		got := detectOutputFile(c.args)
		if got != c.want {
			t.Errorf("detectOutputFile(%v) = %q, want %q", c.args, got, c.want)
		}
	}
}

// --- sanitizeArgs ---

func TestSanitizeArgs(t *testing.T) {
	args := []string{"-i", "input.mkv", "-vf", "scale=1280:720", "-f", "mp4", "output.mp4"}
	got := sanitizeArgs(args, "input.mkv", "output.mp4")

	if got[1] != "pipe:0" {
		t.Errorf("expected input replaced with pipe:0, got %q", got[1])
	}
	if got[len(got)-1] != "pipe:1" {
		t.Errorf("expected output replaced with pipe:1, got %q", got[len(got)-1])
	}
	// other args unchanged
	if got[2] != "-vf" || got[3] != "scale=1280:720" {
		t.Errorf("middle args modified unexpectedly: %v", got)
	}
	// original slice not mutated
	if args[1] != "input.mkv" {
		t.Errorf("sanitizeArgs mutated original slice")
	}
}

// --- mergeConfig ---

func TestMergeConfig(t *testing.T) {
	base := Config{ServerURL: "base:50051", Token: "base-token", FallbackBin: "ffmpeg.real", Fallback: "always"}
	overlay := Config{Token: "new-token", ServerURL: ""}
	got := mergeConfig(base, overlay)

	if got.Token != "new-token" {
		t.Errorf("token not overridden: %q", got.Token)
	}
	if got.ServerURL != "base:50051" {
		t.Errorf("server_url should stay base when overlay is empty: %q", got.ServerURL)
	}
	if got.FallbackBin != "ffmpeg.real" {
		t.Errorf("fallback_bin should stay base: %q", got.FallbackBin)
	}
}

// --- defaultConfig ---

func TestDefaultConfig(t *testing.T) {
	cfg := defaultConfig()
	if cfg.FallbackBin != "ffmpeg.real" {
		t.Errorf("default fallback_bin = %q, want ffmpeg.real", cfg.FallbackBin)
	}
	if cfg.Fallback != "always" {
		t.Errorf("default fallback = %q, want always", cfg.Fallback)
	}
	if cfg.ServerURL != "" {
		t.Errorf("default server_url should be empty, got %q", cfg.ServerURL)
	}
}

// --- loadConfig priority ---

func TestLoadConfigPriority(t *testing.T) {
	// Create a temp dir and write a config file there, point FFMPEG_REMOTE_CONFIG to it.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := Config{ServerURL: "env-server:50051", Token: "env-token"}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FFMPEG_REMOTE_CONFIG", cfgPath)

	// Change working dir so ./ffmpeg-remote.json doesn't interfere
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	got := loadConfig()
	if got.ServerURL != "env-server:50051" {
		t.Errorf("server_url = %q, want env-server:50051", got.ServerURL)
	}
	if got.Token != "env-token" {
		t.Errorf("token = %q, want env-token", got.Token)
	}
}

func TestLoadConfigCurrentDirOverrides(t *testing.T) {
	dir := t.TempDir()

	// Lower-priority env config
	envCfg := Config{ServerURL: "env-server:50051", Token: "env-token"}
	envData, _ := json.Marshal(envCfg)
	envPath := filepath.Join(dir, "env-config.json")
	os.WriteFile(envPath, envData, 0600)
	t.Setenv("FFMPEG_REMOTE_CONFIG", envPath)

	// Higher-priority ./ffmpeg-remote.json
	localCfg := Config{ServerURL: "local-server:50051"}
	localData, _ := json.Marshal(localCfg)
	os.WriteFile(filepath.Join(dir, "ffmpeg-remote.json"), localData, 0600)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	got := loadConfig()
	if got.ServerURL != "local-server:50051" {
		t.Errorf("local config should override env config, got server_url=%q", got.ServerURL)
	}
	// Token from lower-priority env config should still be merged in
	if got.Token != "env-token" {
		t.Errorf("token from env config should survive merge, got %q", got.Token)
	}
}
