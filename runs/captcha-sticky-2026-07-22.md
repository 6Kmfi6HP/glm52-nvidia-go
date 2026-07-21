# Captcha sticky-tab optimization (2026-07-22)

Harness: `go run ./cmd/captchaopt -runs=4`

## Question

Can we avoid full playground Navigate on every extract by keeping one warm tab and calling `hcaptcha.reset` + `hcaptcha.execute`?

## Results

| variant | med_all | med_steady | api_ok% | notes |
|---------|---------|------------|---------|-------|
| nav_each (child tab) | — | — | 0% | hung at 90s timeout in this harness; not used |
| **sticky_exec** | 412 | **341** | **100%** | execute only |
| **sticky_reset** | 400 | **326** | **100%** | **winner** (reset+execute) |
| userdata_nav | — | — | 0% | same child-tab hang |

Cold sticky navigate (trial 1): ~5.8–6.5s. Steady: **~300–480ms**.

vs prior navigate-each baseline (~7–10s extract): **~20× faster** steady-state.

## Chromedp lifecycle bug fixed

Chrome is allocated on the first `Run` via `exec.CommandContext(ctx)`. Canceling a timeout used for that first Run kills the process. Production previously canceled the warm tab context after `about:blank`, so each Extract effectively paid for a new Chrome. Sticky requires a long-lived `browserCtx`.

## Production choice

`internal/captcha/browser.go` + `extract.go`:

1. Keep allocator + browser context until `Close`
2. Warm playground once in `NewBrowser` (no token burn)
3. `Extract` = mutex + `reset`/`execute`; on failure, re-navigate
4. Pool workers serialize on the single sticky tab (~300ms each) — still far cheaper than navigate

Re-run: `go run ./cmd/captchaopt -only=sticky_reset -runs=4`
