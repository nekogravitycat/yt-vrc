package config

import (
	"bufio"
	"os"
	"strings"
)

// DotEnvFile holds credentials (e.g. DISCORD_BOT_TOKEN) that shouldn't
// be typed into a shell or committed on every run.
const DotEnvFile = ".env"

// LoadDotEnv reads KEY=VALUE lines from path into the environment,
// skipping keys already set.
//
// NOTE: an explicit env var always wins over the file -- deliberate, so
// a container's env or a test override is never silently shadowed by
// .env. A missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Tolerate "export FOO=bar" pasted from a shell script.
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, set := os.LookupEnv(k); set {
			continue
		}
		if err := os.Setenv(k, unquote(strings.TrimSpace(v))); err != nil {
			return err
		}
	}
	return sc.Err()
}

// unquote strips one layer of matching quotes -- an opaque token like a
// Discord bot token is often pasted pre-quoted.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
