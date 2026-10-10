# Run report 20261010-152157-s6-capacity-open-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261010-152157-s6-capacity-open-1 |
| epoch_id | 0b47c1b055abc8f54d38f4080eb03519 |
| run_kind | test |
| run_label | test-build scenario, not a benchmark |
| baseline | false |
| outcome | ok |
| start_ns | 1791645717077529000 |
| end_ns | 1791645853567346000 |
| scenario.name | s6-capacity-open |
| scenario.path | scenarios/s6-capacity-open.toml |
| scenario.sha256 | cde6f695c1d61b5a496152349bddec77a92e40442fe1e3bfe6b968881bb922c5 |
| seed | 1 |
| instances | 3 |
| hardware.model | Mac15,9 |
| hardware.ncpu | 16 |
| hardware.memsize_bytes | 137438953472 |
| os.BuildVersion | 25G83 |
| os.ProductName | macOS |
| os.ProductVersion | 26.6.2 |
| versions.go | go1.27.1 |
| versions.valkey | Valkey server v=9.1.2 sha=00000000:1 malloc=libc bits=64 build=1af85cd1d1bb3237 |
| versions.valkey_path | /opt/homebrew/bin/valkey-server |
| versions.postgres | postgres (PostgreSQL) 14.19 (Homebrew) |
| store_settings.aclfile | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-152157-s6-capacity-open-1/store/acl.conf |
| store_settings.aof-use-rdb-preamble | yes |
| store_settings.appendfsync | always |
| store_settings.appendonly | yes |
| store_settings.auto-aof-rewrite-min-size | 1073741824 |
| store_settings.auto-aof-rewrite-percentage | 0 |
| store_settings.bind | 127.0.0.1 |
| store_settings.maxclients | 4096 |
| store_settings.maxmemory | 536870912 |
| store_settings.maxmemory-policy | noeviction |
| store_settings.no-appendfsync-on-rewrite | no |
| store_settings.port | 6390 |
| store_settings.protected-mode | yes |
| store_settings.save |  |
| postgres_settings.fsync | on |
| postgres_settings.max_connections | 300 |
| postgres_settings.statement_timeout | 2500ms |
| postgres_settings.synchronous_commit | on |
| instance_configs | null |
| controlled_clock_ns | null |
| schedule_sha256 | a547cf35a179356fc2821dd93bffd7c27fc511f6e408b401f38b78389febf8a4 |
| thresholds_sha256 | null |
| rewrites | [] |
| label | test-build scenario, not a benchmark |
| policy.capacity | 1 |
| policy.cleanup_interval_ns | 5000000000 |
| policy.clock_source | store |
| policy.failure_mode | open |
| policy.max_live_markers | 20 |
| policy.max_markers_per_key | 5 |
| policy.max_tracked_buckets | 50 |
| policy.outage_tolerance_ns | 5000000000 |
| policy.rate_period_ns | 10000000000 |
| policy.rate_tokens | 1 |
| policy.receipt_lifetime_ns | 60000000000 |
| policy.refund_deadline_ns | 30000000000 |
| instance_settings.margin_ns | 1000000000 |
| instance_settings.max_held_receipts | 1152 |
| instance_settings.max_pending_refunds | 1152 |
| instance_settings.max_release_delay_ns | 5000000000 |
| instance_settings.refund_workers | 128 |
| p_ns | 5000000000 |
| limiter_enabled | true |
| conformance_floor.durable_script_p50_us | 6460.5 |
| conformance_floor.f_fullfsync_floor_p50_us | 5667.9 |
| conformance_floor.plain_fsync_p50_us | 41.6 |

### context.conformance

| field | value |
|---|---|
| eligible | true |
| fsync.durable_script_p50_us | 6460.5 |
| fsync.f_fullfsync_floor_p50_us | 5667.9 |
| fsync.plain_fsync_p50_us | 41.6 |
| generated_at | 2026-10-05T16:54:35Z |
| go | go1.27.1 |
| os | darwin/arm64 |
| passed | true |
| product | Valkey |
| recommended_store_conns | 16 |
| reload_ms_a7 | 468.7 |
| row14.idle_p50_us | 49.4 |
| row14.loaded_p50_us | 12994 |
| row14.writers | 16 |
| scan_calls_200k | 200 |
| store_bin | /opt/homebrew/bin/valkey-server |
| store_version | 9.1.2 |

#### context.conformance.crash

| acked | lost | in_flight | kept_unacked |
|---|---|---|---|
| 4716 | 0 | 32 | 17 |
| 4982 | 0 | 32 | 14 |
| 3279 | 0 | 32 | 11 |

#### context.conformance.row13

| conns | depth | size | cmd_bytes | ops_per_s | p50_us | p99_us | ops | samples |
|---|---|---|---|---|---|---|---|---|
| 2 | 1 | small | 103 | 161.7 | 12789.1 | 16013.8 | 485 | null |
| 2 | 16 | small | 103 | 2549.3 | 12866 | 17634.8 | 7648 | null |
| 2 | 64 | small | 103 | 9514.7 | 13786.9 | 17045.8 | 28544 | null |
| 4 | 1 | small | 103 | 323.7 | 12232.2 | 16004.5 | 971 | null |
| 4 | 16 | small | 103 | 1898.7 | 12942.5 | 463328 | 5696 | null |
| 4 | 64 | small | 103 | 18453.3 | 13980.8 | 17923.8 | 55360 | null |
| 8 | 1 | small | 103 | 640 | 12750.1 | 16603.6 | 1920 | null |
| 8 | 16 | small | 103 | 9386.7 | 13821.9 | 17747.1 | 28160 | null |
| 8 | 64 | small | 103 | 32448 | 15927.6 | 19109.6 | 97344 | null |
| 16 | 1 | small | 103 | 1281.7 | 12758.2 | 16881.6 | 3845 | null |
| 16 | 16 | small | 103 | 517.3 | 508771 | 530600 | 1552 | null |
| 16 | 64 | small | 103 | 31445.3 | 31420 | 61230.8 | 94336 | null |
| 2 | 1 | max | 1505 | 93 | 22394 | 54675.4 | 279 | null |
| 2 | 16 | max | 1505 | 1664 | 19317.3 | 23440.8 | 4992 | null |
| 2 | 64 | max | 1505 | 3242.7 | 39107.3 | 44113.4 | 9728 | [{"ops_per_s":3242.7,"p50_us":39107.3,"p99_us":44113.4,"ops":9728},{"ops_per_s":3242.7,"p50_us":39034.8,"p99_us":44834.3,"ops":9728},{"ops_per_s":3157.3,"p50_us":40180.7,"p99_us":47179.4,"ops":9472}] |
| 4 | 1 | max | 1505 | 315.7 | 12842.8 | 23946.7 | 947 | null |
| 4 | 16 | max | 1505 | 1685.3 | 20234.8 | 768276 | 5056 | null |
| 4 | 64 | max | 1505 | 6144 | 42006.3 | 47831.8 | 18432 | [{"ops_per_s":6400,"p50_us":40046.3,"p99_us":46061.8,"ops":19200},{"ops_per_s":6144,"p50_us":42006.3,"p99_us":47831.8,"ops":18432},{"ops_per_s":192,"p50_us":1383685.6,"p99_us":1432940.2,"ops":576}] |
| 8 | 1 | max | 1505 | 649 | 12106.3 | 17120.1 | 1947 | null |
| 8 | 16 | max | 1505 | 6314.7 | 20396.8 | 25607.4 | 18944 | null |
| 8 | 64 | max | 1505 | 11434.7 | 45407.2 | 51959.4 | 34304 | [{"ops_per_s":11093.3,"p50_us":46036,"p99_us":52877,"ops":33280},{"ops_per_s":11434.7,"p50_us":45407.2,"p99_us":51959.4,"ops":34304},{"ops_per_s":11434.7,"p50_us":45746.6,"p99_us":51687.3,"ops":34304}] |
| 16 | 1 | max | 1505 | 1254.3 | 12946.9 | 16894.7 | 3763 | null |
| 16 | 16 | max | 1505 | 1984 | 21427.9 | 765305 | 5952 | null |
| 16 | 64 | max | 1505 | 20138.7 | 50788.9 | 56848.5 | 60416 | [{"ops_per_s":20821.3,"p50_us":49100.7,"p99_us":66090,"ops":62464},{"ops_per_s":20138.7,"p50_us":50788.9,"p99_us":56848.5,"ops":60416},{"ops_per_s":19797.3,"p50_us":51887.7,"p99_us":58964.4,"ops":59392}] |

#### context.conformance.row9

| clients | appendfsync | ops_per_s | p50_us | p99_us | ops |
|---|---|---|---|---|---|
| 50 | always | 3641.7 | 13927.9 | 20129.8 | 10925 |
| 600 | always | 36418.7 | 17220.8 | 22973.2 | 109256 |
| 50 | everysec | 96507.3 | 512.8 | 823.7 | 289522 |

### context.settings

| name | db_dsn | limiter_enabled | max_in_flight | refuse_after_acquire_bp | margin_ns | max_held_receipts | max_pending_refunds | max_release_delay_ns | refund_workers | capacity | cleanup_interval_ns | clock_source | failure_mode | max_live_markers | max_markers_per_key | max_tracked_buckets | outage_tolerance_ns | rate_period_ns | rate_tokens | receipt_lifetime_ns | refund_deadline_ns | addr |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| api | postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64 | true | 512 | 300 | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null |
| instance | null | null | null | null | 1000000000 | 1152 | 1152 | 5000000000 | 128 | null | null | null | null | null | null | null | null | null | null | null | null | null |
| policy | null | null | null | null | null | null | null | null | null | 1 | 5000000000 | store | open | 20 | 5 | 50 | 5000000000 | 10000000000 | 1 | 60000000000 | 30000000000 | null |
| store | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | 127.0.0.1:6390 |

### context.faults

| at_s | cmd | instance | start_ns | end_ns | outage_start_ns | outage_ns | targets |
|---|---|---|---|---|---|---|---|
| 60 | {"target":"store","mode":"refuse","match":"acquire","duration_ms":5000} | 1 | 1791645777583962000 | 1791645777586746000 | null | null | [{"instance":1,"status":200}] |

### context.children

| name | role | pid | exit | exited_before_stop | grace_ns | killed | instance |
|---|---|---|---|---|---|---|---|
| hitgen | hitgen | 56766 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 56761 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 56762 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 56763 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 56765 | exit 0 | false | 10000000000 | false | null |

### context.workload

| field | value |
|---|---|
| scenario_file | scenarios/s6-capacity-open.toml |
| step_bounds_ns | [0,120000000000] |
| users | 200 |
| instances | 3 |
| verdict | none |
| spike_step | null |
| forced_aof_rewrites_s | [] |
| stop_early | false |

#### context.workload.steps

| rate_per_s | duration_s |
|---|---|
| 300 | 120 |

## Load (PRF-1)

| field | value |
|---|---|
| total.scheduled | 36066 |
| total.planned | 36066 |
| total.sent | 36066 |
| total.initial | 36066 |
| total.duplicates | 0 |
| total.retries_429 | 49289 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 36066 |
| total.sched_lag_ns.p50 | 570191 |
| total.sched_lag_ns.p95 | 1094000 |
| total.sched_lag_ns.p99 | 1672270 |
| total.got_conn_wait_ns_p99 | 30875 |
| stopped_after_step | null |
| excluded_step | null |
| summary.planned | 36066 |
| summary.launched | 36066 |
| summary.unsent | 0 |
| summary.log_drops | 0 |
| summary.stopped_after_step | null |
| summary.excluded_step | null |
| summary.skipped_no_eligible | 0 |
| retry_drops | 0 |
| generator_drops | 0 |
| rule | steps by planned_ns; sent = first attempts written (sent_ns set); unsent = planned − sent; generator_limited: step p99 of sent_ns − planned_ns over every planned first attempt, an unwritten one counting above any bound, > 5 ms (§B9) |

### load.steps

| step | rate_per_s | duration_s | start_ns | end_ns | ran | scheduled | planned | sent | initial | duplicates | retries_429 | unsent | achieved_sent_per_s | offered_per_s | sched_lag_ns.count | sched_lag_ns.p50 | sched_lag_ns.p95 | sched_lag_ns.p99 | got_conn_wait_ns_p99 | generator_limited | status_200 | status_429 | unknown_acquires | unknown_application_outcomes |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 0 | 300 | 120 | 0 | 120000000000 | full | 36066 | 36066 | 36066 | 36066 | 0 | 49289 | 0 | 300.55 | 300.55 | 36066 | 570191 | 1094000 | 1672270 | 30875 | false | 2557 | 72434 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 85355 |
| by_err.ERR-1 | 691 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 1866 |
| by_err.ERR-4 | 72434 |
| by_err.ERR-5 | 0 |
| by_err.ERR-6 | 0 |
| by_err.ERR-7 | 0 |
| by_err.ERR-8 | 0 |
| by_err.ERR-9 | 0 |
| by_err.ERR-9a | 0 |
| by_err.ERR-10 | 10268 |
| by_err.ERR-11 | 0 |
| by_err.ERR-12 | 0 |
| by_err.ERR-12a | 0 |
| by_err.ERR-12b | 0 |
| by_err.ERR-12c | 0 |
| by_err.ERR-12d | 0 |
| by_err.ERR-13 | 0 |
| by_err.ERR-14 | 0 |
| by_err.ERR-15 | 96 |
| by_err.ERR-16 | 0 |
| unmatched | {} |
| transport_errors.total | 0 |
| transport_errors.by_kind | {} |
| decisions.allowed | 718 |
| decisions.degraded | 1935 |
| releases | 96 |
| refunds.applied | 27 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 2557 |
| prf2.new_rows | 2557 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 1866 |
| prf2.quota_rejections | 72434 |
| prf2.capacity_refusals | 10268 |
| prf2.integrity_failures | 0 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 96 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 2557 |

### results.attempt_outcomes

| name | 200 | 429 rate_limited | 503 limiter_capacity_exceeded | 503 service_aborted_after_acquire |
|---|---|---|---|---|
| initial | 624 | 25153 | 10268 | 21 |
| duplicate | null | null | null | null |
| retry | 1933 | 47281 | null | 75 |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| total.attempts | 36066 |
| total.unexpected | 9963 |
| total.rate | 0.276244 |
| total.transport_errors | 0 |
| total.server_errors | 10289 |
| total.excluded_service_aborted_after_acquire | 21 |
| total.excluded_fault_window | 305 |

#### results.unexpected_errors.fault_windows

| start_ns | end_ns | source |
|---|---|---|
| 1791645777583962000 | 1791645782586746000 | {"target":"store","mode":"refuse","match":"acquire","duration_ms":5000} |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 120000000000 | 34572 | 9530 | 0.275657 | 0 | 9853 | 18 | 305 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 85355 | 2557 | 718 | 72434 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 2557 |
| db_ns.p50 | 189584 |
| db_ns.p95 | 1164750 |
| db_ns.p99 | 3209625 |
| api_total_ns.count | 85355 |
| api_total_ns.p50 | 270500 |
| api_total_ns.p95 | 7258958 |
| api_total_ns.p99 | 12930417 |
| rewrite_windows | [] |
| rewrite_p99_ns | null |
| rewrite_latency_ns | null |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 718 | 7421250 | 14579250 | 19698917 |
| limited | 72434 | 239500 | 7343292 | 13094875 |
| degraded | 1935 | 1542 | 10500 | 16708 |
| error | 10268 | 447916 | 1943916 | 8098083 |

### timing.refund_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 27 | 8361125 | 16679292 | 19128542 |
| applied | 27 | 8361125 | 16679292 | 19128542 |
| already_applied | 0 | null | null | null |
| receipt_expired | 0 | null | null | null |
| receipt_invalid | 0 | null | null | null |
| unresolved | 0 | null | null | null |

### timing.end_to_end_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 36066 | 904209 | 4408292 | 12499459 |
| success | 624 | 958000 | 11650167 | 19449083 |
| limited | 25153 | 910875 | 4134375 | 12224500 |
| error | 10289 | 888750 | 3910042 | 12306792 |
| retries | 49289 | 315250 | 9558084 | 15758750 |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 120000000000 | 34572 | 895750 | 4207459 | 12019625 |

## Clocks (T-RPT-1)

| field | value |
|---|---|
| fields.context.start_ns, end_ns, faults[].start_ns, end_ns, rewrites[] | wall clock, ns since the UTC epoch |
| fields.api request arrival_ns | wall clock, ns since the UTC epoch |
| fields.samples t_ns | wall clock, ns since the UTC epoch |
| fields.api decision_ns, request decision_ns, gen decision_ns | budget time: the store's clock in µs × 1,000 (API-4); compared only with budget time |
| fields.refund credit_decision_ns | budget time: the store's clock in µs × 1,000 (API-4); compared only with budget time |
| fields.gen planned_ns, sent_ns | hitgen's monotonic clock, ns from its t0 |
| fields.gen latency, got_conn_wait_ns | hitgen's monotonic clock, ns (durations) |
| fields.api limiter_ns, db_ns, total_ns | hitapi's monotonic clock, ns (durations) |
| fields.release release_mono_ns | hitapi's monotonic clock, ns since the process started |
| fields.refund release_mono_ns, final_mono_ns | the limiter's monotonic clock, ns since it was built |
| t0_wall_ns | 1791645717582764000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 135 |
| health_reads | 405 |
| max_in_flight | 19 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 8.15 | 24.73 | 33816940 | 37060608 | 135 | 135 |
| hitapi-2 | 8.3 | 27.64 | 34323387 | 36618240 | 135 | 135 |
| hitapi-3 | 8.34 | 26.56 | 33764147 | 36241408 | 135 | 135 |
| hitgen | 23.12 | 56.82 | 41541207 | 44007424 | 135 | 135 |
| postgres | 0.33 | 5.64 | 204569531 | 233504768 | 135 | 135 |
| sampler | 8.77 | 10.83 | 28822368 | 29949952 | 135 | 135 |
| valkey-server | 7.53 | 22.07 | 9802122 | 9912320 | 135 | 135 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 14 | 12 | 135 |
| quarantined_keys | 0 | 0 | 135 |
| tracked_buckets | 50 | 50 | 135 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 19 | 0 | 135 |
| 1 | listener_drops | 0 | 0 | 135 |
| 1 | log_drops | 0 | 0 | 135 |
| 1 | rl_cleanup_entries_total{action="moved"} | 9 | 9 | 135 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 135 |
| 1 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 135 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 135 |
| 1 | rl_cleanup_lag_seconds | 1.25048 | 0 | 135 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_passes_total{status="complete"} | 28 | 28 | 135 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 135 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 135 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 135 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 135 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 135 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 135 |
| 1 | rl_listener_drops_total | 0 | 0 | 135 |
| 1 | rl_live_markers | 14 | 12 | 135 |
| 1 | rl_pending_refunds | 0 | 0 | 135 |
| 1 | rl_quarantined_keys | 0 | 0 | 135 |
| 1 | rl_tracked_buckets | 50 | 50 | 135 |
| 2 | in_flight | 11 | 0 | 135 |
| 2 | listener_drops | 0 | 0 | 135 |
| 2 | log_drops | 0 | 0 | 135 |
| 2 | rl_cleanup_entries_total{action="moved"} | 2 | 2 | 135 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 135 |
| 2 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 135 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 135 |
| 2 | rl_cleanup_lag_seconds | 0.337072 | 0 | 135 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_passes_total{status="complete"} | 28 | 28 | 135 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 135 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 135 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 135 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 135 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 135 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 135 |
| 2 | rl_listener_drops_total | 0 | 0 | 135 |
| 2 | rl_live_markers | 14 | 12 | 135 |
| 2 | rl_pending_refunds | 0 | 0 | 135 |
| 2 | rl_quarantined_keys | 0 | 0 | 135 |
| 2 | rl_tracked_buckets | 50 | 50 | 135 |
| 3 | in_flight | 14 | 0 | 135 |
| 3 | listener_drops | 0 | 0 | 135 |
| 3 | log_drops | 0 | 0 | 135 |
| 3 | rl_cleanup_entries_total{action="moved"} | 3 | 3 | 135 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 135 |
| 3 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 135 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 135 |
| 3 | rl_cleanup_lag_seconds | 1.81252 | 0.588701 | 135 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_passes_total{status="complete"} | 28 | 28 | 135 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 135 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 135 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 135 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 135 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 135 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 135 |
| 3 | rl_listener_drops_total | 0 | 0 | 135 |
| 3 | rl_live_markers | 14 | 12 | 135 |
| 3 | rl_pending_refunds | 0 | 0 | 135 |
| 3 | rl_quarantined_keys | 0 | 0 | 135 |
| 3 | rl_tracked_buckets | 50 | 50 | 135 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 56756 | null |
| hitapi | 56761 | 1 |
| hitapi | 56762 | 2 |
| hitapi | 56763 | 3 |
| sampler | 56765 | null |
| hitgen | 56766 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-152157-s6-capacity-open-1/schedule.jsonl |
| schedule_sha256 | a547cf35a179356fc2821dd93bffd7c27fc511f6e408b401f38b78389febf8a4 |
| schedule_sha256_context | a547cf35a179356fc2821dd93bffd7c27fc511f6e408b401f38b78389febf8a4 |
| schedule_sha256_matches | true |
| schedule_entries | 36066 |
| planned_per_step | [36066] |
| instances | 3 |
| requests_per_instance.1 | 12022 |
| requests_per_instance.2 | 12022 |
| requests_per_instance.3 | 12022 |
| served_per_instance.1 | 27552 |
| served_per_instance.2 | 28927 |
| served_per_instance.3 | 28876 |
| instances_used | 3 |
| users | 200 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 7587 |
| busiest_user.instances | [1,2,3] |

## Baseline comparison

Not applicable to this run, or not built yet.

## Correctness (BUD-11, BUD-11a)

| field | value |
|---|---|
| verdict | pass |
| known_credits | 27 |
| unknown_credits | 0 |
| listener_drops | 0 |
| pre_kill_lines | 0 |
| pre_kill_read_ok_false | 0 |
| invalid_reasons | [] |
| violation | null |

## Capacity (T-RPT-2)

Not applicable to this run, or not built yet.

## Evidence validity (§B8)

| field | value |
|---|---|
| valid | true |

### evidence_validity.checks

| name | ok | value | detail |
|---|---|---|---|
| log_drops | true | 0 | log_drops summed over every stop and pre_kill line of every api-N.jsonl |
| listener_drops | true | {"sum":0,"stop_lines":3,"pre_kill_lines":0,"pre_kill_unread":0,"segments_without_count":0} | HAR-13: listener_drops summed over every stop and pre_kill line is 0, every pre_kill read succeeded (read_ok), and every process ended with one of the two |
| fixtures_off | true | {"applies":false,"health_reads":405,"fixtures_on":405,"clock_not_store":0,"faults":1} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 1 | store outage segments armed: faultctl store down or pause, and timed store refusals |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":false,"windows":0} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | true | {"verdict":"pass","invalid_reasons":[]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
