# Unit maintainability — package docs, orphan tests, strict parser

1. Every Go package has a package comment with **Owns** (single responsibility) and **Never**
   (non-goals); `cmd/*` get `doc.go` (api, expiry-worker, media-worker, meta-worker, migrate,
   payment-worker, admin-fixture, and any new cmd). Fix stale docs (e.g. internal/payments says it owns
   only method configuration but now runs Stripe query/signal/refund workers; cmd/api's doc was taken from
   buyer_payment.go). Dependencies are NOT hand-written: docs/engineering/dependency-map.md (generated,
   CI-checked) is the single source (PROCESS.md §5 amended). A CI step fails if any package lacks a doc
   (`go list -f '{{.ImportPath}} {{.Doc}}' ./... | awk '$2==""'`).
2. Orphan admin Playwright specs (tests/admin/ledger.spec.ts, production.spec.ts, visual-states.spec.ts
   and any spec no test-local mode or CI runs): wire each into an existing or new test-local mode with the
   server it needs, or delete it if a newer gate covers the same claims (state which). Nothing may exist
   that no gate runs.
3. Export a strict JSON parser from internal/integrations/meta (duplicate-member rejecting) and use it in
   the metareply keyring loader (ruling n).
4. Add every new test-local mode to CI? No — keep CI = full foundation + static checks; browser modes run
   in the release gate script (unit deploy-release). Document the full gate list in docs/delivery/GATES.md
   (mode → what it proves → tier → how to run), generated or hand-kept but checked by a script that every
   mode listed in test-local.sh's usage line appears in GATES.md.
