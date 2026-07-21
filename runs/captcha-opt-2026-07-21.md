# Captcha extract optimization (2026-07-21)

Harness: `go run ./cmd/captchaopt -runs=2`  
Baseline already includes `imagesEnabled=false` + resource Chrome flags.

## Results (median extract_ms; all variants 100% API accept)

| variant | med_ext_ms | vs baseline | notes |
|---------|------------|-------------|-------|
| baseline | 10194 | — | fixed 1s sleep + 500ms poll |
| fast_wait | 8120 | −20% | WaitReady + 100ms poll, no 1s sleep |
| block_urls | 7816 | −23% | CDP `Network.setBlockedURLs` CSS/fonts/media/images |
| **block+fast** | **6846** | **−33%** | **winner** |
| fetch+fast | 7174 | −30% | Fetch abort by ResourceType; slower than block+fast |
| all | 7367 | −28% | +small window + both blockers; no extra gain |

API latency (~8–18s) is upstream-dominated and was not used for ranking.

## Production choice

Apply **block+fast** in `internal/captcha/extract.go`:

1. `network.Enable` + `SetBlockedURLs` before navigate (scripts not blocked)
2. Drop fixed `Sleep(1s)`; wait for widget + `hcaptcha` with 100ms poll
3. Token poll interval 200ms (was 1s)

Do **not** add Fetch interception or smaller window (no win over block+fast).

Re-run: `go run ./cmd/captchaopt -runs=2`
