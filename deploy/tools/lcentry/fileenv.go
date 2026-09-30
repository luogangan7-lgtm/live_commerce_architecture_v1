// File: deploy/tools/lcentry/fileenv.go
// Purpose: *_FILE -> env expansion rules for `lcentry run` (deploy-design §6).
// Runs as/in: inside every app container before exec (see main.go).
// Reads env: NAME_FILE for NAME ~ ^(DATABASE_URL|COMMERCE_[A-Z0-9_]+)$ only.
// Reads secrets: /run/secrets/<file> (Compose bind-mounted file secrets,
// mode 0440 root:${LC_SECRETS_GID}; the container joins that group).
// Status: MODEL_ONLY; verified by smoke S04 (unit) and S26 (no values in inspect).
// Change rules: the literal "__UNSET__" sentinel is shared with
// deploy/scripts/secrets-init.sh and preflight.sh; change all three together.

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// secretsRoot is a variable (not a const) only so tests can point it at a
// temporary directory. Production always uses Compose/K8s' /run/secrets.
var secretsRoot = "/run/secrets"

const (
	maxSecretBytes = 65536
	// unsetSentinel marks an owner-supplied secret that has not been provided
	// yet. The variable is then left unset so the app's own validation fails
	// closed (or treats the feature as optional, e.g. a public PKCE client).
	unsetSentinel = "__UNSET__"
)

var expandable = regexp.MustCompile(`^(DATABASE_URL|COMMERCE_[A-Z0-9_]+)$`)

// expandFileEnv returns a new environment where every expandable NAME_FILE is
// replaced by NAME=<contents> (or removed, for the unset sentinel). Other
// *_FILE variables pass through untouched. Errors never contain values.
func expandFileEnv(environ []string) ([]string, error) {
	present := make(map[string]bool, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			present[kv[:i]] = true
		}
	}
	out := make([]string, 0, len(environ))
	var added []string
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			out = append(out, kv)
			continue
		}
		key, value := kv[:i], kv[i+1:]
		name, isFile := strings.CutSuffix(key, "_FILE")
		if !isFile || !expandable.MatchString(name) {
			out = append(out, kv)
			continue
		}
		if present[name] {
			return nil, errors.New(name + " and " + key + " both set")
		}
		content, err := readSecretFile(value)
		if err != nil {
			return nil, errors.New(key + ": " + err.Error())
		}
		// NAME_FILE is always dropped so the child never sees secret paths it
		// does not need; the sentinel additionally leaves NAME unset.
		if content != unsetSentinel {
			added = append(added, name+"="+content)
		}
	}
	return append(out, added...), nil
}

// readSecretFile enforces the path and content rules. The returned error text
// is fixed per rule and never includes the path contents.
func readSecretFile(path string) (string, error) {
	root := filepath.Clean(secretsRoot)
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	clean := filepath.Clean(path)
	if !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", errors.New("path must be under " + root)
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", errors.New("secret file not readable")
	}
	// K8s projects secrets through ..data symlinks inside the same mount; a
	// link that escapes the secrets root is refused.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil || !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return "", errors.New("secret file resolves outside " + root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", errors.New("secret file not readable")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("secret path is not a regular file")
	}
	if info.Size() < 1 || info.Size() > maxSecretBytes {
		return "", errors.New("secret file size out of range")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", errors.New("secret file not readable")
	}
	if len(data) > maxSecretBytes {
		return "", errors.New("secret file size out of range")
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("secret file contains NUL")
	}
	// Strip exactly one trailing newline (and a CR before it) so files written
	// by editors or `echo` behave like printf-written ones.
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
		if m := len(data); m > 0 && data[m-1] == '\r' {
			data = data[:m-1]
		}
	}
	if len(data) == 0 {
		return "", errors.New("secret file is empty")
	}
	return string(data), nil
}
