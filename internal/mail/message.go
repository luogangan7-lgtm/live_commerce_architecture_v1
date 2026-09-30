// Package mail owns outbound transactional email for merchant password auth
// (contracts/merchant-password-auth-v1.md §3): one message, sent once, over a
// generic SMTP adapter with implicit TLS on port 465, classified as sent,
// ErrFailed or ErrUnknown.
//
// It never retries, queues, renders templates, tracks links or validates
// recipients beyond syntax: SMTP has no idempotency key (PD7/I06), so an
// uncertain outcome is surfaced to the user, who requests a new code.
//
// External hosts: the configured SMTP server only (first configuration
// smtp.qq.com:465, recommended upgrade smtp.exmail.qq.com:465; ruling O-B in
// docs/delivery/units/r2-design-rulings.md). Hosts are configuration, not
// constants.
//
// This file is the integrator's F0 freeze (brief docs/delivery/units/auth-mail.md);
// units must not edit it.
package mail

import "errors"

// Message is one outbound email. Text and HTML become the two parts of a
// multipart/alternative body.
type Message struct{ To, Subject, Text, HTML string }

// ErrFailed means the server cannot have accepted the message.
var ErrFailed = errors.New("mail: not accepted")

// ErrUnknown means '.' was written but no final reply was read; the message
// may or may not have been accepted. Never retried (PD7/I06).
var ErrUnknown = errors.New("mail: outcome unknown")
