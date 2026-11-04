# Measured results

What the test rig measured on one laptop in October and November 2026. The [README](../README.md#results) has the short version. The raw reports are in [`runs/`](../runs), one folder per run.

## Scenario runs (October 2026)

The October 2026 scenario runs (`runs/h6-manifest.json`) used:
- a MacBook Pro (Mac15,9, 16 cores);
- three `hitapi` instances unless noted;
- Valkey 9.1.2 with `appendfsync always`, and PostgreSQL 14;
- a policy of 10 tokens a second with capacity 20, per user.

Clients retry after a 429, up to 2 times, so the limiter runs send more requests than the runs without it. "Correctness" is the guarantee check.

| Scenario | Traffic | Requests | Allowed | DB rows: no limiter → limiter | Correctness | Performance verdict |
|---|---|---|---|---|---|---|
| s1-ramp | 100 req/s, doubling every 30 s, 10,000 users | 3,444 | 2,950 | stopped early | pass | **no sustainable step**: p99 0.50 s at 100 req/s (threshold 61 ms) |
| s1-ramp-1api | same, one instance | 11,179 | 8,167 | stopped early | pass | **100 req/s sustained** (p99 24 ms). 200 req/s failed (p99 0.52 s) |
| s2-users-100 | 500 → 10 → 500 req/s. 100 users away 60–150 s, plus 10 always active | 117,953 | 36,426 | 61,033 → 36,426 | pass | — |
| s2-users-1000 | same, 1,000 users | 100,882 | 43,991 | 61,033 → 43,991 | pass | — |
| s2-users-10000 | same, 10,000 users | 92,493 | 47,791 | 61,033 → 47,791 | pass | — |
| s3-busy-user | one user at 100/s beside 100 users at 5/s each | 94,426 | 61,163 | 71,866 → 61,163 | pass | — |
| s4-spike | 100 → 500 → 100 req/s | 25,039 | 18,175 | 19,961 → 18,175 | pass | **recovery not met**: p99 0.52 s after the spike (threshold 59 ms), 0 errors |
| s5-retry | 200 req/s over 10 users (heavy retry load) | 55,160 | 10,965 | 24,028 → 10,965 | pass | — |
| s7-long | 70 req/s for 10 min, two forced AOF rewrites, 2% of allowed calls released for refund | 43,018 | 41,022 | 41,170 → 41,022 | pass | — |
| s6 (8 runs) | store down, paused, state deleted, capacity exhausted, each in closed and open mode | — | — | — | pass | — |

- **Without the limiter**, the same API ran every step of the s1 ramp, up to 10,000 req/s (680,262 requests), with an overall p99 of 3.8 ms.
- **Refunds.** In s7-long, all 833 released calls were credited. In s6-down-closed, refunds went through a store outage: 710 were applied, 2 retries were recognized as already applied, and none were left unresolved.
- **Latency cost: durable profile.** The limiter adds about **8–12 ms at p50** and **0.5–0.75 s at p99**. Without it, the API's p50 is 0.2–0.8 ms and its p99 is 3.1–4.4 ms.
- **Cause: this Mac's SSD.**
  - Each decision waits for `F_FULLFSYNC`, about 6 ms.
  - About every 2,750 flushes, the SSD runs about 15 flushes at 0.23–0.51 s each, over 3.6 s. Those windows hold 8–15% of requests and set p99. Outside them, p99 is 19–25 ms.
  - A bare `F_FULLFSYNC` loop, with no Valkey and no limiter, shows the same pattern (5 min, 39,161 flushes: p50 6.4 ms, p99.9 262 ms, worst 513 ms).
- **The failed verdicts are flush slowdowns, not throughput.** Each step's p99 is judged over 30 s.
  - In s1-ramp, one slowdown at 28.5 s (251 requests, 7%) failed the 100 req/s step and stopped the run.
  - The one-instance ramp's first step had no slowdown and passed.
  - The design's p99 of 33 ms (DESIGN §A3.0 row 9) was a store-only benchmark. It did not include these slowdowns.

## Store profiles

The slow part is Valkey making each decision permanent on disk before it answers. You can choose how strict that is when you start the store. The library itself does not change.

- **`durable`** (default): every answer is on disk first. Nothing answered is ever lost, even on a power cut. On this Mac: p50 about 10–13 ms, p99 about 0.5 s.
- **`barrier`**: every answer is on its way to disk, in order. A Valkey or OS crash loses nothing. A power cut can lose the last few writes. On this Mac: p50 about 0.6–0.8 ms, p99 under 3 ms. It needs a Valkey built with a one-line patch (`deploy/build-valkey-barrier.sh`).
- **`everysec`**: Valkey flushes to disk about once a second. An OS crash or power cut can lose about 1–2 s of decisions. p50 0.2 ms, p99 under 1.5 ms.
- **`buffered`**: macOS decides when to flush. An OS crash or power cut can lose about 30 s or more. p50 0.2 ms, p99 under 1.5 ms.

How to pick one:

```bash
deploy/start-store.sh --profile barrier --port 6390 --dir /path/to/store --aclfile /path/to/acl.conf
# or: RL_STORE_PROFILE=everysec deploy/start-store.sh --port …
bin/test/orchestrator --scenario scenarios/profile-ramp.toml --store-profile everysec   # harness runs
```

All four profiles passed correctness with no failed requests, up to 10,000 req/s offered. The three opt-in profiles handled about 21,400 decisions a second at the top step. Under `durable`, the test client fell behind from 3,200 req/s during the disk pauses (3.6% of requests unsent). A lost write under an opt-in profile cannot be detected later, so the guarantee does not cover it (SPEC `STA-16a`). The full comparison, run IDs and advice on which to use are in the [operator guide](operator-guide.md#store-profiles). Linux has not been measured.
