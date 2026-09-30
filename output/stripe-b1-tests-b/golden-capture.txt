# SP14 PAYUNi golden capture (evidence)
- Captured on base code (HEAD ba08af8, code identical to b563bb5; no checkout/buyerhttp change merged) before any start-http merge.
- Method: temporary TestZZCaptureSP14 (deleted, not committed) drove bphSetup HTTP prepare/view/handoff on real PG via test-focused.sh (exit 0, PASS=1).
- Normalization (applied identically in stripe_buyer_http_test.go sbhNormalize): UUID -> 00000000-0000-4000-8000-000000000000, quoted RFC3339 -> "2000-01-01T00:00:00Z", hex run >=32 -> HEX. Everything else byte-for-byte.
- Never regenerate to pass a failing SP14; a diff means the PAYUNi wire changed.
