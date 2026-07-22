# Hang / 504 experiment (2026-07-22)

Harness: `go run ./cmd/hangbench` (+ `-idle-burst`). Raw: `runs/hangbench-raw-2026-07-22.txt`.

## Environment

Host already had **~33 Chrome processes** before the harness started. Extra headless workers compete with that baseline.

## Results

### A — sticky extract

| step | latency |
|------|---------|
| warm+alloc | 9.3s |
| first after warm | 3.3s |
| steady | 1.6s → **164ms** |
| post-idle (65s) | **2.3s** (re-nav path, not 90s hang) |

### B — concurrent drain (healthy, no idle)

| workers | warm | wall (4 Takes) | chrome peak |
|---------|------|----------------|-------------|
| 1 | 3.7s | **774ms** | ~45 |
| 2 | 8.3s | **559ms** | ~54 |

`workers=2` saves ~200ms under burst, costs **+1 Chrome process** and slower warm.

### C — idle-burst (full pool held quiet, then 4 Takes)

| scenario | idle | fills during idle (takes=0) | burst wall | notes |
|----------|------|-----------------------------|------------|-------|
| w=1 | 70s | **5** (size=2) | 631ms | reaper churn refreshed sticky |
| w=2 | 70s | **8** | 1ms | more Chrome (55→63), still churn |
| w=1 | 100s (>TTL) | **6**, expired later 2 | 1.3s | same churn pattern |

## Failure mechanism (evidence-backed)

1. **Reaper race**: `discardStale` drained the whole channel into `keep`, then put back. While empty, blocked workers minted and `fills` climbed even though tokens were still within TTL. Idle was not idle — Chrome kept working.
2. **Multi-Chrome default**: `-pool-workers=2` doubled Chrome for a small drain win; on a machine already at 30+ Chromes this matches “浏览器开太多”.
3. **504 vs serve**: serve returns **503** on `captcha-wait` / empty pool, **502** on upstream errors. Client-visible **504** is almost certainly a reverse-proxy timeout in front of a long captcha/upstream wait — not an HTTP status from `serve` itself.

Healthy sticky + pool did **not** hard-hang in this run (post-idle ≤2.3s). The chronic risk is resource pressure + unnecessary idle mint + long worst-case extract (15s sticky / 90s re-nav) under a short proxy timeout.

## Optimization direction (chosen)

1. **Fix reaper + wait-for-space before mint** — mutex FIFO; drop expired head only; workers do not extract while buffer is full (stops idle mint churn).
2. **Default `-pool-workers=1`** — one Chrome unless explicitly scaled up.
3. Keep `captcha-wait` (30s → 503); document proxy read timeout ≥ that / upstream TTFB.

## After fix (same idle-burst, workers=1, idle=70s)

| metric | before | after |
|--------|--------|-------|
| fills during idle (takes=0) | 5 | **2 (flat)** |
| expired during idle | 0 | 0 |
| burst max Take | 630ms (sticky kept warm by churn) | **2.24s** (honest sticky re-nav) |
| chrome during idle | climbing / busy | stable |

Raw appended in `runs/hangbench-raw-2026-07-22.txt`.
