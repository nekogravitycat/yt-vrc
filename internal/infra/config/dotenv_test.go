package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDotEnvParsesTheFormsPeopleActuallyWrite(t *testing.T) {
	p := writeEnv(t, "# a comment\n\nPLAIN=one\n  SPACED = two \nexport EXPORTED=three\nQUOTED=\"four\"\nSINGLE='five'\nEMPTY=\nnovalue\n")
	t.Setenv("PLAIN", "")
	_ = os.Unsetenv("PLAIN")
	for _, k := range []string{"PLAIN", "SPACED", "EXPORTED", "QUOTED", "SINGLE", "EMPTY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"PLAIN": "one", "SPACED": "two", "EXPORTED": "three",
		"QUOTED": "four", "SINGLE": "five", "EMPTY": "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// The file supplies credentials on a dev machine; it must never
// override what a container or a one-off test run set explicitly.
func TestLoadDotEnvDoesNotOverrideTheEnvironment(t *testing.T) {
	p := writeEnv(t, "DISCORD_BOT_TOKEN=from-file\n")
	t.Setenv("DISCORD_BOT_TOKEN", "from-env")

	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DISCORD_BOT_TOKEN"); got != "from-env" {
		t.Errorf("token = %q, want the environment's value", got)
	}
}

// No .env is the normal case in deployment, not a failure.
func TestLoadDotEnvIgnoresAMissingFile(t *testing.T) {
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatal(err)
	}
}
