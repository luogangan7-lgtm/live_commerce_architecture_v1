// Package jobqueue owns the server-chosen River queue names (checkout expiry and the per-profile
// payment queues) and the shared worker Run loop that starts a client, announces readiness and stops
// it cleanly.
//
// It never defines a job kind or worker (each domain package does), never maps an unknown profile to
// a queue, and never opens a database pool itself.
package jobqueue
