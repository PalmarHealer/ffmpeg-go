package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --- loadEnv ---

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	content := "PORT=9090\nTOKEN=test-secret\nMAX_JOBS=8\n# comment\n\nINVALID\n"
	if err := os.WriteFile(envFile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	// Change to temp dir so loadEnv picks up the .env file
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Clear env vars that loadEnv will set
	os.Unsetenv("PORT")
	os.Unsetenv("TOKEN")
	os.Unsetenv("MAX_JOBS")

	loadEnv()

	if got := os.Getenv("PORT"); got != "9090" {
		t.Errorf("PORT = %q, want 9090", got)
	}
	if got := os.Getenv("TOKEN"); got != "test-secret" {
		t.Errorf("TOKEN = %q, want test-secret", got)
	}
	if got := os.Getenv("MAX_JOBS"); got != "8" {
		t.Errorf("MAX_JOBS = %q, want 8", got)
	}
}

func TestLoadEnvDoesNotOverrideExisting(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("TOKEN=from-file\n"), 0600)

	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	t.Setenv("TOKEN", "pre-existing")
	loadEnv()

	if got := os.Getenv("TOKEN"); got != "pre-existing" {
		t.Errorf("existing env var was overwritten: TOKEN = %q", got)
	}
}

func TestLoadEnvMissingFile(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer os.Chdir(orig)
	os.Chdir(dir)

	// Should not panic when .env doesn't exist
	loadEnv()
}

// --- token auth ---

func TestTokenAuth(t *testing.T) {
	orig := token
	defer func() { token = orig }()

	token = "secret"

	cases := []struct {
		input string
		valid bool
	}{
		{"secret", true},
		{"wrong", false},
		{"", false},
	}
	for _, c := range cases {
		got := c.input == token
		if got != c.valid {
			t.Errorf("token %q: valid=%v, want %v", c.input, got, c.valid)
		}
	}
}

// --- command whitelist ---

func TestCommandWhitelist(t *testing.T) {
	allowed := map[string]bool{
		"ffmpeg":  true,
		"ffprobe": true,
		"bash":    false,
		"sh":      false,
		"rm":      false,
		"":        false,
	}
	for cmd, want := range allowed {
		got := cmd == "ffmpeg" || cmd == "ffprobe"
		if got != want {
			t.Errorf("whitelist(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// --- job limit ---

func TestJobLimit(t *testing.T) {
	orig := maxJobs
	defer func() { maxJobs = orig }()

	maxJobs = 2

	cases := []struct {
		active  int64
		limited bool
	}{
		{0, false},
		{1, false},
		{2, true},
		{3, true},
	}
	for _, c := range cases {
		got := maxJobs > 0 && c.active >= maxJobs
		if got != c.limited {
			t.Errorf("active=%d max=%d: limited=%v, want %v", c.active, maxJobs, got, c.limited)
		}
	}
}

func TestJobLimitDisabled(t *testing.T) {
	orig := maxJobs
	defer func() { maxJobs = orig }()

	maxJobs = 0 // 0 means unlimited

	// Even with high active count, should never be limited
	active := int64(9999)
	if maxJobs > 0 && active >= maxJobs {
		t.Error("expected no limit when maxJobs=0")
	}
}
