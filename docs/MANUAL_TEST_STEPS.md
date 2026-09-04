# Manual Test Steps

This document records the deliberate gap between CI-automated test coverage and
the full mandate lifecycle. It is a stated, intentional gap — not silent skips.

## Why these steps are manual

The Registration Link (Auth Link) flow works as follows:

1. Merchant calls `CreateRegistrationLink` → Razorpay returns a `short_url`.
2. Customer opens `short_url` in a browser, enters card details, completes 3DS.
3. Razorpay creates a `token_` on the customer account (status: `confirmed`).
4. Merchant discovers the token via `WaitForNewConfirmedToken`.

Steps 2 and 3 require a human with a browser. There is no Razorpay test-mode
API call that simulates a customer completing the hosted page — unlike UPI
Autopay's `success@razorpay` shortcut, no equivalent exists for card CoFT in
test mode.

Recording a go-vcr cassette that covers steps 6–8 of the lifecycle test
(`ExecuteMandateDebit` against a confirmed token) requires first completing step
2 in a live browser session to produce a real token, then recording the cassette
in that same session. This is a one-time step that should be done before demo day.

---

## Steps to record the full cassette (do before demo day)

**Prerequisites:**
- `.env` file in the project root with `RAZORPAY_KEY_ID` and `RAZORPAY_KEY_SECRET` set.
- A browser available for the Razorpay hosted card-registration page.
- A Razorpay test-mode card: `4718609108204366` (Visa, expires 12/29, CVV 123).

**Step 1 — Delete the existing empty/partial cassette (if any):**
```bash
rm -f test/integration/cassettes/mandate_lifecycle.yaml
rm -f test/integration/cassettes/reg_link_token_discovery.yaml
```

**Step 2 — Run the lifecycle test in record mode:**
```bash
go test -v -tags=integration -run TestMandateLifecycle ./test/integration/
```

Steps 1 and 2 will record against the live Razorpay test API.
Steps 3, 5, 10 will be skipped (documented replacements).

**Step 3 — Run the registration link discovery test:**
```bash
go test -v -tags=integration -run TestRegistrationLink_TokenDiscovery ./test/integration/
```

This test will print a `short_url`. Open it in a browser and complete the card
registration using the test card `4718609108204366`. The test polls for the
resulting confirmed token. Once it appears, the cassette is saved.

**Step 4 — Run the ceiling test and timeout test (no manual action needed):**
```bash
go test -v -tags=integration -run TestRegistrationLink_MaxAmountCeiling ./test/integration/
go test -v -tags=integration -run TestRegistrationLink_TimeoutOnIncompletion ./test/integration/
```

These record cleanly without browser interaction.

**Step 5 — Verify all cassettes replay without live keys:**
```bash
RAZORPAY_KEY_ID=dummy RAZORPAY_KEY_SECRET=dummy \
  go test -v -tags=integration ./test/integration/
```

If any test makes a live API call instead of replaying the cassette, it will
fail with an authentication error, making the gap visible.

**Step 6 — Commit the cassettes:**
```bash
git add test/integration/cassettes/
git commit -m "test: record go-vcr cassettes for mandate lifecycle and registration link tests"
```

---

## What the skipped lifecycle subtests cover (once cassette is recorded)

| Subtest | What it proves | Status |
|---------|---------------|--------|
| `3_AuthorizeMandate_Skipped` | Card S2S gated by PCI-DSS cert | Permanently skipped — replaced by registration_link_test.go |
| `5_WaitForConfirmation_Skipped` | Old WaitForTokenConfirmation removed | Permanently skipped — covered by TestRegistrationLink_TokenDiscovery |
| `6_ExecuteDebit_Unconfirmed_Rejected` | Debit against unconfirmed token returns ErrTokenNotConfirmed | **Needs cassette** — self-skips when validTokenID is empty |
| `7_ExecuteDebit_InCap` | Debit ≤ max_amount succeeds | **Needs cassette** |
| `8_ExecuteDebit_MaxAmountExceeded` | Debit > max_amount returns ErrDebitMaxAmountExceeded | **Needs cassette** |
| `10_Authorize_Failure_Path_Skipped` | Failure path | Permanently skipped — replaced by TestRegistrationLink_TimeoutOnIncompletion |

Subtests 6–8 self-skip only because `validTokenID` is empty. Once a real token
is recorded in the cassette and the subtest flow is updated to retrieve the
token from the cassette's `TestRegistrationLink_TokenDiscovery` run, these will
execute normally in CI.

---

## Confirm before demo day

- [ ] `TestRegistrationLink_MaxAmountCeiling` passes — ceiling assertion prints ✓
- [ ] `TestRegistrationLink_TokenDiscovery` produces and discovers a real token
- [ ] `TestRegistrationLink_TimeoutOnIncompletion` returns clean `ErrTokenTimeout`
- [ ] Cassettes committed; CI passes with `RAZORPAY_KEY_ID=dummy`
