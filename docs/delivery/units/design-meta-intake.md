# Unit design-meta-intake — contract draft: Meta comment → keyword claim → private reply

Role: contract designer (read-only on code; writes one DRAFT contract file).

## Goal
Draft `contracts/meta-claims-intake-v1.md`, the T10c amendment that connects what already
exists:
- Meta webhook ingress + inbox + consumer (`contracts/meta-webhook-protocol-v1.md`,
  `meta-inbox-v1.md`, `meta-consumer-v1.md`, `meta-runtime-v1.md`; code under
  `internal/integrations/meta`, `cmd/meta-worker`). Find where normalized comment events land
  (grep `social` / `comment` in migrations and `internal/integrations/meta/normalize.go`).
- The FROZEN claims core (`contracts/live-keyword-claims-v1.md`, `internal/claims`): today its
  ingress is MOCK/manual only.
- The first private reply (Messenger/IG) carrying the claim/cart link: exactly one per comment
  (SHOPLINE parity + Meta policy; see memory rules in 架构.md §9–§10: never reset the first
  private-reply budget by rule/token version; never fall back to another channel; UNKNOWN send
  never blind-retried).

The draft must specify: mapping from a normalized FB Page feed comment / IG comment event to
`claims` ingest input (live session ↔ post/video binding, commenter identity as a platform-scoped
id only, no PII beyond what the claims contract allows); idempotency on the Meta comment id;
the private-reply send path (Graph API endpoint + permission names, window rules, the
operation-ledger/dispatcher reuse from `contracts/external-dispatcher-v1.md`), failure/UNKNOWN
handling; what can be proven MOCK (signed webhook replay) vs LIVE (needs Page access token +
`pages_messaging`/`instagram_manage_messages` + App Review). Gates MI01..MInn with tiers.

## Facts
Use WebFetch on developers.facebook.com for: Page feed webhooks (comment item), Instagram
`comments` webhook field, Private Replies (`/{page-id}/messages` with `recipient.comment_id`,
IG `recipient.comment_id`), the 7-day private-reply window, permissions. Cite URL + retrieval
date 2026-09-28. If a fact cannot be retrieved, mark it UNKNOWN; don't guess.

## Read
`docs/delivery/PROCESS.md`; 架构.md §9, §10, §11 (grep headings); the contracts above by
section; `contracts/live-keyword-claims-v1.md` §0.1 and its ingest interface.

## Write paths
Only `contracts/meta-claims-intake-v1.md` (status `DRAFT`, 2026-09-28). Migration number if
needed: 0064; post-River 0014.

## Return
Path, a 12-line summary, the owner-input list (Meta app/Page permissions, App Review), and the
integrator-ruling list.
