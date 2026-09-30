// Command lcentry owns the tiny, stdlib-only launcher baked into every deploy image (lc-go,
// lc-admin, lc-storefront). It never prints a secret value (variable names only), never talks to
// anything but a loopback health URL, and never changes an application binary. Sub-commands:
//
//	lcentry run -- /abs/target [args...]
//	    Expand NAME_FILE=/run/secrets/<file> into NAME=<file contents> for the
//	    variables the apps read (DATABASE_URL and COMMERCE_*), drop the *_FILE
//	    variable, then replace itself with the target via execve(2). The
//	    application binaries stay unchanged and `docker inspect` only ever shows
//	    secret *paths*, never values. Rules live in fileenv.go.
//	lcentry probe [-timeout 3s] [-max-status 399] http://127.0.0.1:<port>/path
//	    Loopback-only HTTP health probe for Docker healthchecks in distroless
//	    and Node images (neither ships curl/wget). Rules live in probe.go.
//
// Runs as/in: PID 2 (under Docker's tini, `init: true`) of api, admin,
// storefront and all workers, as their non-root UID (65532 Go, 1000 Node).
// Reads env: every NAME_FILE whose NAME matches ^(DATABASE_URL|COMMERCE_[A-Z0-9_]+)$
// (wired in deploy/compose.yml). Reads secrets: the files those variables
// point at, only under /run/secrets/ (Compose file secrets).
// Status: MODEL_ONLY until smoke S04 (unit tests) and S15/S26 (runtime) pass.
// Change rules: keep stdlib-only; any rule change must update lcentry_test.go
// and deploy-design §6; never print a secret value, only variable names.
//
// Exit codes: 0 ok (probe), 1 probe failed, 78 configuration error (EX_CONFIG),
// 126 exec failure. `run` never returns on success because execve replaces
// the process image (so /proc/self/exe is the application itself).
package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

const (
	exitOK     = 0
	exitProbe  = 1
	exitConfig = 78
	exitExec   = 126
)

// execFunc is syscall.Exec in production; tests inject a recorder so the
// dispatch logic is covered without replacing the test process.
type execFunc func(argv0 string, argv []string, envv []string) error

func main() {
	os.Exit(realMain(os.Args[1:], os.Environ(), syscall.Exec, os.Stderr))
}

// realMain dispatches sub-commands. It only returns when exec was not
// performed (errors, probe results, or an injected exec in tests).
func realMain(args, environ []string, exec execFunc, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "lcentry: usage: lcentry run -- /abs/target [args...] | lcentry probe [-timeout d] [-max-status n] URL")
		return exitConfig
	}
	switch args[0] {
	case "run":
		return runCommand(args[1:], environ, exec, stderr)
	case "probe":
		return probeCommand(args[1:], stderr)
	default:
		fmt.Fprintln(stderr, "lcentry: unknown sub-command")
		return exitConfig
	}
}

// runCommand expands *_FILE secrets and execs the target. Messages name only
// variables, never values or file contents.
func runCommand(args, environ []string, exec execFunc, stderr io.Writer) int {
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(stderr, "lcentry: usage: lcentry run -- /abs/target [args...]")
		return exitConfig
	}
	target := args[1]
	if len(target) == 0 || target[0] != '/' {
		fmt.Fprintln(stderr, "lcentry: target must be an absolute path")
		return exitConfig
	}
	env, err := expandFileEnv(environ)
	if err != nil {
		fmt.Fprintln(stderr, "lcentry: "+err.Error())
		return exitConfig
	}
	if err := exec(target, args[1:], env); err != nil {
		fmt.Fprintln(stderr, "lcentry: exec failed")
		return exitExec
	}
	return exitOK
}
