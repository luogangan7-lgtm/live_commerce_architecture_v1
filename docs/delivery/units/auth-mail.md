# Unit auth-mail — generic SMTP adapter (implicit TLS 465) + loopback fake `mailtest`

Role: integration_worker (mid tier). Base = `00c1d94` + integrator commit F0 (below), SHA recorded at
dispatch. Worktree `.worktrees/auth-mail`, branch `unit/auth-mail`. No delegation, no network, no real
mailbox, no secret. Parallel with `auth-core`, `auth-ui`, `auth-tests` (disjoint paths).
Contract: `contracts/merchant-password-auth-v1.md` (v1 FROZEN 2026-09-30) incl. §14 rulings; owner
rulings O-B/O-D in `docs/delivery/units/r2-design-rulings.md`. "Defaults adopted" below bind unless the
integrator overrides.

**Goal:** one real SMTP adapter that sends one message once over implicit TLS with PLAIN auth and
classifies the outcome SENT/FAILED/UNKNOWN without ever retrying; plus a loopback TLS fake that the
MOCK/HTTP/BROWSER gates drive the same adapter against.

## Read (by section; grep headings, `sed -n` ranges)
PROCESS.md; contract §0 PD7, PD15 + rejected alternatives; §1 F7, F8; §3 (all); §5 (SMTP variables
only); §9 PA02, PA12, PA14; §11 "Personal-mailbox transport". Pattern to copy by symbol:
`internal/integrations/psp/stripe/stripetest` (`New`, `SetNextFault`, package comment, self-tests).

## F0 (integrator, lands before dispatch; unit must not edit)
- `go.mod`/`go.sum`: `golang.org/x/crypto v0.57.0` (argon2 only, ruling R-1) + line in
  `docs/engineering/dependencies.md` (auth-core is the importer).
- `internal/mail/message.go` with exactly the "F0 frozen" part of the block below + package comment.

## Defaults adopted (P2s the frozen text left open)
- M1 `Config.Port` must be 465; any other value only when `AllowLoopback` ∧ host is a loopback literal
  (the fake listens on a random port). cmd/api never sets `Port` ≠ 465 and never sets `RootCAs`
  (loopback overrides are Config fields used by in-process tests only; no env var for them).
- M2 `From` is parsed with `net/mail.ParseAddress`; its address must equal `Username` byte-for-byte
  after ASCII lower-casing, else `NewSMTP` fails (PA02 "From ≠ username refused at config").
- M3 `Message-ID` = `<` + 16 random bytes hex + `@` + From domain + `>`; `Date` = RFC 5322 in UTC.
- M4 Text part `quoted-printable`, HTML part `base64`; boundary random; `Subject` via
  `mime.BEncoding.Encode("UTF-8", …)` only when non-ASCII.
- M5 "before the `.`" means before the data writer's `Close()` returns control of the final reply:
  any error from dial/TLS/`NewClient`/`Auth`/`Mail`/`Rcpt`/`Data`/body `Write` ⇒ `ErrFailed`; an error
  from the data writer's `Close()` that is an `*textproto.Error` (a reply was read) ⇒ `ErrFailed`
  (kind `smtp_rejected`, code only); any other `Close()` error (timeout, EOF, reset) ⇒ `ErrUnknown`.
  `Quit()` errors after a 2xx are ignored (still SENT).
- M6 Reply text stored as `provider_message_id`: final reply message truncated to 128 bytes on a UTF-8
  boundary, control characters removed.
- M7 Probe (PA12): dial, TLS, AUTH, `MAIL FROM:<from>`, `RSET`, `QUIT`; returns the MAIL FROM reply
  code; never calls `Rcpt`/`Data`.

## FROZEN Go interface (auth-core, auth-tests and the integrator call exactly these)
```go
package mail // internal/mail
// --- F0 frozen (message.go, integrator) ---
type Message struct{ To, Subject, Text, HTML string }
var ErrFailed  = errors.New("mail: not accepted")      // server cannot have accepted the message
var ErrUnknown = errors.New("mail: outcome unknown")   // '.' written, no final reply
// --- auth-mail ---
type Config struct {
    Host, Username, Password, From string // Host DNS name (loopback only with AllowLoopback)
    Port          int                     // 465 (M1)
    RootCAs       *x509.CertPool          // nil = system roots; non-nil only with AllowLoopback
    AllowLoopback bool                    // mirrors COMMERCE_IDENTITY_ALLOW_LOOPBACK_TESTS=1
}
type SMTP struct{ /* unexported */ }
func NewSMTP(c Config) (*SMTP, error)                                  // validates M1/M2; never dials
func (c *SMTP) Send(ctx context.Context, m Message) (reply string, err error) // nil | wraps ErrFailed | wraps ErrUnknown; exactly one DATA, never retried
func (c *SMTP) Probe(ctx context.Context, from string) (code int, err error)  // M7, PA12 only
func (c *SMTP) String() string                                         // redacted: host only

package mailtest // internal/mail/mailtest — imported only by *_test.go and test binaries
type Fault int
const (FaultNone Fault = iota; FaultDial; FaultAuth; FaultRcpt; FaultData354; FaultReply4xx; FaultReply5xx; FaultFrequency550; FaultDropAfterDot)
type Received struct{ From, To, Subject, Text, HTML string; Raw []byte; Header textproto.MIMEHeader }
type Server struct{ Host string; Port int; RootCAs *x509.CertPool; Username, Password string }
func New(t testing.TB) *Server                 // loopback TLS, random port, self-signed CA; t.Cleanup closes
func (s *Server) Config(from string) mail.Config // ready-to-use loopback Config
func (s *Server) SetNextFault(f Fault)
func (s *Server) SetDelay(d time.Duration)     // before the final reply (PA08b 400 ms)
func (s *Server) Messages() []Received
func (s *Server) DataCount() int               // DATA commands seen (PA02/PA09 "zero re-sends")
func (s *Server) RejectFromMismatch(on bool)   // 553 when MAIL FROM ≠ Username (PA12 twin)
```

## Build
1. `internal/mail/smtp.go`: §3 dial sequence exactly (`tls.DialWithDialer` 10 s, `ServerName`=host,
   `MinVersion` TLS 1.2; `smtp.NewClient`; `conn.SetDeadline(min(ctx deadline, now+25 s))`;
   `smtp.PlainAuth("", user, secret, host)`; `Mail`; `Rcpt`; `Data`; write; `Close`; `Quit`).
   Classification per §3 + M5; **no retry branch anywhere** — each error branch carries a comment
   "no retry: SMTP has no idempotency key (PD7/I06)".
2. MIME builder in `internal/mail/mime.go` (never in the F0 `message.go`; stdlib `mime`,
   `mime/multipart`, `mime/quotedprintable`, `encoding/base64`): headers of §3, `Auto-Submitted:
   auto-generated`, `multipart/alternative`, no links/tracking added by the adapter.
3. Errors/logs never contain recipient, subject, body, username or secret (I11); `String()` redacted.
4. `internal/mail/mailtest/`: TLS SMTP server + in-memory mailbox + faults above; package comment says
   "test-only; cmd/api must not import (PA14 `go list -deps`)".
5. Unit tests `internal/mail/smtp_test.go` + the gate `TestMailPA02SMTP` (contract §10 gives PA02 to
   this unit; red run = mutate the classification of FaultDropAfterDot to FAILED, then green).

PROCESS §5: package comment `// Package mail owns …` / `// It never …` (retries, queues, templates,
recipient validation beyond syntax) / names the external hosts (`smtp.qq.com:465`,
`smtp.exmail.qq.com:465`, configured, not constants) and why. Every wire constant (port 465, reply
classes, header names) carries the docs URL + retrieval date (`https://pkg.go.dev/net/smtp`,
RFC 5321 §4.2/§4.5.3, RFC 2047, retrieved <date>). No hand-written "used by" lists (depmap is generated).

## Write paths
`internal/mail/{doc.go,smtp.go,mime.go,smtp_test.go}` (not `message.go`), `internal/mail/mailtest/**`,
`output/auth-mail/**`. Forbidden: `internal/identity/**`, `cmd/**`, `migrations/**`, `apps/**`,
`tests/**`, contracts, go.mod/go.sum, deploy/.

## Verify
```sh
GOTOOLCHAIN=go1.27.1 go vet ./internal/mail/... && gofmt -l internal/mail
GOTOOLCHAIN=go1.27.1 go test -race -count=1 ./internal/mail/...          # incl. TestMailPA02SMTP
GOTOOLCHAIN=go1.27.1 go test -race -count=1 -run '^TestMailPA02SMTP$' -v ./internal/mail > output/auth-mail/pa02-green.log
python3 scripts/check_packet.py
```
Logs → `/Volumes/data/live_commerce_architecture_v1/output/auth-mail/` (main checkout path, PROCESS §4)
with command, SHA, exit code, PASS/FAIL/SKIP counts; `pa02-red.log` before `pa02-green.log`.

## Order / Non-goals / Return
Dispatch after F0. Merge first (M1): auth-core's `cmd/api/identity.go` wiring compiles against it.
No STARTTLS/587, no connection pooling, no queue, no retry, no DKIM signing, no bounce handling, no
provider HTTP API, no real send (PA12/PA13 are owner-run LIVE; PA12 test file is auth-tests').
Return commit SHA, model/reasoning, base, paths, commands + exit codes + counts, evidence paths, how
M1–M7 were handled, risks, NOT_RUN (PA12/PA13). Any deviation from the frozen signatures = stop and escalate.
