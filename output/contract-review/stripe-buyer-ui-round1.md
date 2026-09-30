**Verdict: BLOCK.** Three P1s, all fixable by editing text. No P0.

**What holds.** I compared the draft to the Go code and it matches:
- `OrderPayment.cancel_requested` is omitempty and only appears when the attempt is Stripe.
- `HostedHandoff.redirect_url` is omitempty. `PaymentSignal` is `{order_id,scheduled}`. Prepare returns `PAYMENT_PENDING`.
- The redirect validator can't be bypassed. The literal `https://checkout.stripe.com/` prefix fixes the host, so `.evil`, `@evil`, `:443`, upper case, non-ASCII, `//x` and `\` all fail or stay on Stripe.
- No referrer leaks. The child's `no-referrer` meta does not apply to a `location.replace` started by the parent, but the storefront's `Referrer-Policy: same-origin` (next.config.ts:25) covers it.
- Only one session can be created: SQL allows one attempt per order and prepare is replayed under the Web Lock.
- A blocked popup throws before any fetch, so it makes zero BFF calls.

**P1-1 â browser-tests brief, Harness bullets 2â3: secrets reach Node and Next.** Go spawns Node, so the harness can't "unset STRIPE_* before spawning Node". `browserEnvironment` (tests/foundation/browser_identity_chain_test.go:302) copies all of `os.Environ()` except `COMMERCE_`/`LC_`/`DATABASE_URL`/`POSTGRES_PASSWORD`. With `set -a`, every secret in secrets.env would be passed on. Fix text:
"Read only `STRIPE_SECRET_KEY` from secrets.env in a subshell (`STRIPE_SECRET_KEY=$(set +x; . file; printf %s "$STRIPE_SECRET_KEY")`, then export only that name); never `set -a` the file. The Go test builds the Node/Next env by stripping `STRIPE_*` and every name defined in secrets.env, and a unit assertion proves the Node env has no `STRIPE_`-prefixed key or `sk_test_`/`rk_test_` value."

**P1-2 â contract Â§4 step 2 and Â§6: Continue breaks on the device that paid.** Stripe never writes `handoff_started`, so the `prepare` marker stays forever. Today's `payOrder` throws `uncertain` when a marker exists and the view is neither fresh nor `PREPARED`. "Existing replay rules" leaves that branch in place. Replace step 2 with:
"Stripe path: view NOT_STARTED/DRAFT/NONE with marker â replay prepare with the marker's key and body; without marker â persist a new marker, then prepare. View PENDING/AWAITING_PAYMENT/CREATING|READY with `cancel_requested=false` â skip prepare whether or not a marker exists and go to step 3. Any other view â render, no POST. A PAYUNi marker on a Stripe view, or the reverse, â `failed`."

**P1-3 â contract Â§9 SU09 (and brief SU09): the gate can't be run as written.** Real SANDBOX can't produce some Â§5 rows:
- `REVIEW_REQUIRED` needs a money mismatch.
- `AUTHORIZED` never happens for Stripe.
- The `cutoff` row can only appear if the client clock is off, because SQL (0061 view_v2) returns `UNAVAILABLE` once `now â¥ handoff_cutoff`.

Fix text: "SU09 rows that SANDBOX cannot reach (REVIEW_REQUIRED, UNAVAILABLE, cutoff, CREATING-budget, CLOSED_UNPAID screenshots) run in a BROWSER(MOCK) assembly using `stripetest` plus a fixture clock, labelled MOCK. Remove the AUTHORIZED row for Stripe."

**P2s**
- **Â§5/Â§7, `uncertain` copy:** it says "a payment page that was already issued will not be issued again", which is false for Stripe (D16 lets the same URL be reopened). Add: "Stripe errors, budget and CLOSED/UNAVAILABLE show `failed` (or `creating` for the budget), never `uncertain`."
- **Â§5, cutoff row:** say "UNAVAILABLE and now â¥ handoff_expires_at â `cutoff` copy + Cancel; otherwise `readOnly`."
- **Â§5, Cancel during Pay:** `take_stripe_handoff` doesn't check `cancel_requested`. Add: "Cancel is disabled while Pay/Continue is in flight; the Stripe path re-GETs before the handoff POST and aborts if `cancel_requested`."
- **Â§4 step 4:** the brief says set `used` before `location.replace`; the contract says after. Write "set `used` before `location.replace`".
- **Â§4 step 3:** say whether the 30 s READY poll holds `commerce-purchase-write-v1`. Proposed: "release the lock after prepare; re-acquire for the handoff POST". Otherwise other tabs' purchase writes stall for up to 30 s.
- **Â§6, "history":** the child tab's browser history will always contain the Stripe URL. Reword to "no URL in the store tab's URL, history API calls, storage, cookies, console or logs".
- **Â§6, polling:** state that mount counts as a restart of the 10-minute window, and that the step-3 READY poll pauses the background poll.
- **Â§8:** "when Cancel/Continue disappears, move focus to the payment status line (`tabIndex=-1`)".
- **Browser brief, CONNECT allowlist:** a refused non-Stripe host (for example a hosted-page captcha provider) should be recorded, and fail only if the flow can't complete. The CAPTCHA stop should be `t.Fatal("BLOCKED: captcha")`, recorded as NOT_RUN(BLOCKED), so it isn't a silent SKIP.
- **Browser brief:** reject any `STRIPE_ACCOUNT_ID` other than `acct_1UJDb0RusP6Wwj7e`, matching SP16. Check the 900 s timeout against 6 SANDBOX runs plus SU07/SU09, or split them into separate `-run` calls.
- **Â§3:** add the negative "Stripe path rejects ISSUED/ALREADY_ISSUED".

I made no repo edits; nothing was run beyond reads and greps.