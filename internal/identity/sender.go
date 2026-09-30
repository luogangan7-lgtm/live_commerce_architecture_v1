package identity

// Mail dispatch for password challenges (contract PD7, A5, A6, A9, A12).
//
// One SMTP attempt per challenge, never retried: SMTP has no idempotency key, so a retry after an
// uncertain result could deliver two different-looking mails for one code (I06). A lost mail is
// recovered by the user pressing "resend", which creates a NEW challenge with a new code.
//
//   - login waits for the send (the caller already proved the password), 10 s cap (A6);
//   - signup/reset return their 202 first and send from a bounded detached goroutine (X3), so the
//     response time never depends on SMTP or on whether the email exists (PD6). The plaintext code
//     exists only in this process's memory until the send ends; a crash leaves the row PENDING.

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"livecommerce/internal/mail"
)

// Mailer sends one message once. *mail.SMTP satisfies it; tests use a loopback fake.
type Mailer interface {
	Send(ctx context.Context, m mail.Message) (reply string, err error)
}

const (
	syncSendTimeout  = 10 * time.Second // A6: keeps a login inside http.Server.WriteTimeout 15 s and the BFF's 14 s
	asyncSendTimeout = 25 * time.Second // A6: detached background sends
	asyncSlots       = 8                // A5: separate non-blocking semaphore for background sends
)

// send performs the single attempt and maps it to a record_challenge_mail state (A9): nil error is
// SENT (reply kept), mail.ErrUnknown is UNKNOWN (never retried, I06), anything else FAILED.
func (p *Passwords) send(ctx context.Context, m mail.Message) (state, reply string) {
	reply, err := p.mailer.Send(ctx, m)
	switch {
	case err == nil:
		return "SENT", reply
	case errors.Is(err, mail.ErrUnknown):
		slog.Warn("password_auth_mail", "state", "UNKNOWN")
		return "UNKNOWN", ""
	default:
		slog.Warn("password_auth_mail", "state", "FAILED")
		return "FAILED", ""
	}
}

// recordMail stores the outcome once (identity.record_challenge_mail: only from PENDING). It runs on a
// context detached from the caller so a cancelled request still records what was actually sent.
func (p *Passwords) recordMail(parent context.Context, id [16]byte, state, reply string) {
	if p.recordHook != nil { // unit-test seam: sender logic without a database
		p.recordHook(id, state, reply)
		return
	}
	ctx := context.WithoutCancel(parent)
	err := withTx(ctx, p.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT identity.record_challenge_mail($1,$2,$3)`, id, state, reply)
		return err
	})
	if err != nil {
		// The row stays PENDING; nothing is re-sent because of it (PD7).
		slog.Warn("password_auth_mail_record_failed", "state", state)
	}
}

// sendSync is the login path: one attempt inside the request, bounded by syncSendTimeout.
func (p *Passwords) sendSync(ctx context.Context, id [16]byte, m mail.Message) string {
	sctx, cancel := context.WithTimeout(ctx, syncSendTimeout)
	defer cancel()
	state, reply := p.send(sctx, m)
	p.recordMail(ctx, id, state, reply)
	return state
}

// sendAsync is the signup/reset path (X3). record is false for the "account exists" notice, which has
// no challenge row. When the 8-slot semaphore is full or Close was called, nothing is dialled and the
// challenge is recorded FAILED (A5/A12); the caller's response never changes.
func (p *Passwords) sendAsync(parent context.Context, id [16]byte, record bool, m mail.Message) {
	p.bgMu.Lock()
	if p.bgClosed {
		p.bgMu.Unlock()
		if record {
			p.recordMail(parent, id, "FAILED", "")
		}
		return
	}
	select {
	case p.bgSlots <- struct{}{}:
	default:
		p.bgMu.Unlock()
		if record {
			p.recordMail(parent, id, "FAILED", "")
		}
		return
	}
	p.bgWG.Add(1)
	p.bgMu.Unlock()
	ctx := context.WithoutCancel(parent) // detached: the 202 has been (or is being) written
	go func() {
		defer p.bgWG.Done()
		defer func() { <-p.bgSlots }()
		defer func() {
			if recover() != nil { // a mailer bug must not take the API down; the row stays PENDING
				slog.Error("password_auth_mail_panic")
			}
		}()
		sctx, cancel := context.WithTimeout(ctx, asyncSendTimeout)
		defer cancel()
		state, reply := p.send(sctx, m)
		if record {
			p.recordMail(ctx, id, state, reply)
		}
	}()
}

// Close stops accepting background sends and waits for in-flight ones until ctx ends (A12,
// PD7: graceful shutdown waits up to 25 s). Call it before closing the pool.
func (p *Passwords) Close(ctx context.Context) error {
	p.bgMu.Lock()
	p.bgClosed = true
	p.bgMu.Unlock()
	done := make(chan struct{})
	go func() { p.bgWG.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
