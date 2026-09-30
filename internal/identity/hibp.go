package identity

// Have I Been Pwned breach check for sign-up and reset passwords (contract PD11, F6, ruling Q3).
//
// Only the first 5 hex characters of the password's SHA-1 leave the process (k-anonymity range API).
// The check is fail-open: any error, timeout or non-200 accepts the password and logs
// `password.breach_check_unavailable`. It runs under the PD3 limiter (shared with Argon2) so it cannot
// be used to open unbounded outbound connections.

import (
	"bufio"
	"context"
	"crypto/sha1" // #nosec: SHA-1 is mandated by the HIBP range protocol, not used for security here
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// hibpDefaultBase is the production host. Docs https://haveibeenpwned.com/API/v3#PwnedPasswords
// (retrieved 2026-09-29, contract F6): GET /range/{first 5 hex of SHA-1}, no API key, no rate limit,
// `Add-Padding: true` pads responses to 800-1,000 rows (padding rows have count 0).
const hibpDefaultBase = "https://api.pwnedpasswords.com"

const hibpTimeout = 2 * time.Second // PD11: unreachable within 2 s => accept

// newHIBPClient never follows redirects: a redirect is a non-200 and therefore fail-open.
func newHIBPClient() *http.Client {
	return &http.Client{
		Timeout:       hibpTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// hibpBreached reports whether password appears in the range response with count > 0. Errors are
// returned to the caller (checkBreach decides fail-open); ErrBusy means the limiter was full.
func (p *Passwords) hibpBreached(ctx context.Context, password string) (bool, error) {
	release, err := hashLimiter.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	sum := sha1.Sum([]byte(password))
	full := strings.ToUpper(hex.EncodeToString(sum[:]))
	prefix, suffix := full[:5], full[5:]

	ctx, cancel := context.WithTimeout(ctx, hibpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.hibpBase+"/range/"+prefix, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Add-Padding", "true")
	req.Header.Set("User-Agent", "livecommerce-identity/1")
	resp, err := p.hibp.Do(req)
	if err != nil {
		return false, errors.New("hibp unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("hibp status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20))
	for sc.Scan() {
		line, count, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if !ok || !strings.EqualFold(line, suffix) {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(count), 10, 64)
		if err == nil && n > 0 {
			return true, nil
		}
	}
	return false, sc.Err()
}

// checkBreach applies the breach policy: nil (accept), PolicyError{breached}, or ErrBusy. Every other
// failure of the check itself is fail-open (ruling Q3) and only logged with the audit action name;
// contract §4.2 has no definer to write `password.breach_check_unavailable` to identity.auth_events.
func (p *Passwords) checkBreach(ctx context.Context, password string) error {
	if p.policy.BreachCheck == "off" {
		return nil
	}
	breached, err := p.hibpBreached(ctx, password)
	switch {
	case errors.Is(err, ErrBusy):
		return ErrBusy
	case err != nil:
		slog.Warn("password_auth_event", "action", "password.breach_check_unavailable")
		return nil
	case breached:
		return PolicyError{Reason: "breached"}
	}
	return nil
}
