// Command meta-worker owns the process that runs the Meta webhook consumer queue: it decrypts stored
// inbox payloads with the payload keyring (internal/integrations/meta) and, when the claims actor
// key is configured, stages claim-intake rows (meta-claims-intake-v1 §3).
//
// It never receives webhooks (cmd/api admits and persists them), never calls graph.facebook.com
// (private replies belong to cmd/claims-worker) and never starts unless
// COMMERCE_META_WORKER_ENABLED=1. Its config type redacts itself in logs, JSON and %#v output.
package main
