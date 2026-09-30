package metaads

// secretenv_test.go: the two delivery forms of a secret (lcentry-expanded contents, or a path read
// directly) and every refusal. Values are synthetic.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFromEnv(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	ok := map[string]map[string]string{
		"inline":           {"S": "abc"},
		"inline newline":   {"S": "abc\r\n"},
		"file":             {"S_FILE": write("a", "abc")},
		"file trailing nl": {"S_FILE": write("b", "abc\n\n")},
	}
	for name, m := range ok {
		got, err := SecretFromEnv(env(m), "S", 16)
		if err != nil || string(got) != "abc" {
			t.Errorf("%s: %q,%v", name, got, err)
		}
	}
	for name, m := range map[string]map[string]string{
		"neither":             {},
		"both":                {"S": "abc", "S_FILE": write("c", "abc")},
		"missing file":        {"S_FILE": filepath.Join(dir, "nope")},
		"directory":           {"S_FILE": dir},
		"empty file":          {"S_FILE": write("d", "")},
		"only newline":        {"S_FILE": write("e", "\n")},
		"oversize file":       {"S_FILE": write("f", strings.Repeat("a", 17))},
		"oversize inline":     {"S": strings.Repeat("a", 17)},
		"only newline inline": {"S": "\n"},
	} {
		if got, err := SecretFromEnv(env(m), "S", 16); !errors.Is(err, ErrConfig) || got != nil {
			t.Errorf("%s: %q,%v", name, got, err)
		}
	}
	if _, err := SecretFromEnv(nil, "S", 16); !errors.Is(err, ErrConfig) {
		t.Error("nil getenv")
	}
}
