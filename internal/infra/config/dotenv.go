package config

import (
	"bufio"
	"os"
	"strings"
)

// DotEnvFile is the optional file read before the environment is
// consulted. It exists for one setting in particular: DISCORD_BOT_TOKEN
// is a credential that must not be typed into a shell on every start,
// pasted into a chat, or committed -- and it is the only value this
// service needs that a person cannot simply retype from memory.
const DotEnvFile = ".env"

// LoadDotEnv reads KEY=VALUE lines from path into the process
// environment, leaving any variable that is already set alone.
//
// The precedence is deliberate: a real environment variable is an
// explicit act by whoever started the process (a container's env, a
// one-off override in a test run), and a file on disk must never
// silently outrank it. A missing file is not an error -- the file is
// how the dev machine supplies credentials, not how deployment does.
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
		// "export FOO=bar" is what you get from copying a line out of a
		// shell script, and rejecting it would be a pointless surprise.
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

// unquote strips one layer of matching quotes. Quoting is worth
// supporting because a Discord token is opaque enough that a person
// will reasonably wrap it, and a stray quote character would fail
// authentication with no clue as to why.
func unquote(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
