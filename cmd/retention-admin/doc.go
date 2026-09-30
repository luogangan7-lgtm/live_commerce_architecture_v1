// Command retention-admin is the operator-only CLI of the claims retention purge and
// actor-level deletion (contracts/claims-retention-purge-v1.md §5, U08). It owns argument and
// stdin parsing, the derivation of the actor key and peer keys for a Meta sender, and the fixed
// exit codes; every database effect is one internal/retention call (one 0071 definer).
//
// It never starts as a service, never runs on the deploy host or in CI with the operator login
// (only `status` accepts the job login), never puts a sender id, comment id, key or DSN in argv,
// a log line, stdout or an error, and never makes a network call.
//
// Environment:
//
//	COMMERCE_RETENTION_OPERATOR_DATABASE_URL  operator login (lc_retention_operator); every subcommand
//	COMMERCE_RETENTION_JOB_DATABASE_URL       job login (lc_retention_job); `status` only, used when the operator DSN is unset
//	COMMERCE_CLAIMS_ACTOR_KEY                 K_actor (base64 of 32 bytes); `erase` with a sender id only
//
// Subcommands:
//
//	status                                                       enforced= version= link_days= intake_days= claims_days= social_days= last_run_unix= last_run_more=
//	policy-set --expected-version N --enforced=BOOL --link-days N --intake-days N --claims-days N --social-days N
//	run [--limit N]                                              one manual batch (default 500, max 1000); prints counts
//	erase --request UUID (--object page|instagram --asset ID [--app ID ...] | --tenant UUID --store UUID --bundle UUID)
//	      stdin: one JSON object {"sender_id":"..."} or {"comment_ref":"..."} (empty stdin only with --bundle)
//	replay [--tombstones-file PATH]                              JSON array of full tombstone tuples; none = replay the log
//
// Exit codes: 0 done/replayed, 2 usage (including the job DSN on any subcommand but status),
// 3 held (prints retry_after, RFC 3339 UTC; nothing was written), 4 not found, 5 conflict or busy,
// 1 anything else. stderr carries one fixed code such as retention_admin_usage, never a driver
// message, DSN, key or selector.
package main
