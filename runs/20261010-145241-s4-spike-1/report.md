# Run report 20261010-145241-s4-spike-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261010-145241-s4-spike-1 |
| epoch_id | 98d9309aa7127520ea6ea5579e01fafe |
| run_kind | bench |
| run_label | benchmark |
| baseline | false |
| outcome | ok |
| start_ns | 1791643961581769000 |
| end_ns | 1791644123352528000 |
| scenario.name | s4-spike |
| scenario.path | scenarios/s4-spike.toml |
| scenario.sha256 | 8a2fab6cfb6de4468f4ac4628b6c8550c838370e6194d94aa23103871660af3e |
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
| store_settings.aclfile | runs/20261010-145241-s4-spike-1/store/acl.conf |
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
| schedule_sha256 | c1db98cbcb8290f0b28e1882c4ff67601332384356b8828e96f6d0116d54c885 |
| thresholds_sha256 | 800e7e9153b31e546a9245e28c0e9dba40ac90d51637eb9757bde78e8aca0415 |
| faults | [] |
| rewrites | [] |
| label | benchmark |
| policy.capacity | 20 |
| policy.cleanup_interval_ns | 5000000000 |
| policy.clock_source | store |
| policy.failure_mode | closed |
| policy.max_live_markers | 50000 |
| policy.max_markers_per_key | 1000 |
| policy.max_tracked_buckets | 100000 |
| policy.outage_tolerance_ns | 5000000000 |
| policy.rate_period_ns | 1000000000 |
| policy.rate_tokens | 10 |
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
| api | postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64 | true | 512 | 0 | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null |
| instance | null | null | null | null | 1000000000 | 1152 | 1152 | 5000000000 | 128 | null | null | null | null | null | null | null | null | null | null | null | null | null |
| policy | null | null | null | null | null | null | null | null | null | 20 | 5000000000 | store | closed | 50000 | 1000 | 100000 | 5000000000 | 1000000000 | 10 | 60000000000 | 30000000000 | null |
| store | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | 127.0.0.1:6390 |

### context.children

| name | role | pid | exit | exited_before_stop | grace_ns | killed | instance |
|---|---|---|---|---|---|---|---|
| hitgen | hitgen | 52233 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 52229 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 52230 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 52231 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 52232 | exit 0 | false | 10000000000 | false | null |

### context.workload

| field | value |
|---|---|
| scenario_file | scenarios/s4-spike.toml |
| step_bounds_ns | [0,60000000000,70000000000,160000000000] |
| users | 10000 |
| instances | 3 |
| verdict | spike |
| spike_step | 1 |
| forced_aof_rewrites_s | [] |
| stop_early | false |

#### context.workload.steps

| rate_per_s | duration_s |
|---|---|
| 100 | 60 |
| 500 | 10 |
| 100 | 90 |

## Load (PRF-1)

| field | value |
|---|---|
| total.scheduled | 19961 |
| total.planned | 19961 |
| total.sent | 19961 |
| total.initial | 19961 |
| total.duplicates | 0 |
| total.retries_429 | 5078 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 19961 |
| total.sched_lag_ns.p50 | 597277 |
| total.sched_lag_ns.p95 | 1168386 |
| total.sched_lag_ns.p99 | 2413203 |
| total.got_conn_wait_ns_p99 | 528375 |
| stopped_after_step | null |
| excluded_step | null |
| summary.planned | 19961 |
| summary.launched | 19961 |
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
| 0 | 100 | 60 | 0 | 60000000000 | full | 6002 | 6002 | 6002 | 6002 | 0 | 962 | 0 | 100.033 | 100.033 | 6002 | 597310 | 1111288 | 1908416 | 267959 | false | 5716 | 1248 | 0 | 0 |
| 1 | 500 | 10 | 60000000000 | 70000000000 | full | 4950 | 4950 | 4950 | 4950 | 0 | 2319 | 0 | 495 | 495 | 4950 | 603764 | 1431938 | 2748447 | 682084 | false | 3995 | 3274 | 0 | 0 |
| 2 | 100 | 90 | 70000000000 | 160000000000 | full | 9009 | 9009 | 9009 | 9009 | 0 | 1797 | 0 | 100.1 | 100.1 | 9009 | 594344 | 1146529 | 2315891 | 445542 | false | 8464 | 2342 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 25039 |
| by_err.ERR-1 | 18175 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 0 |
| by_err.ERR-4 | 6864 |
| by_err.ERR-5 | 0 |
| by_err.ERR-6 | 0 |
| by_err.ERR-7 | 0 |
| by_err.ERR-8 | 0 |
| by_err.ERR-9 | 0 |
| by_err.ERR-9a | 0 |
| by_err.ERR-10 | 0 |
| by_err.ERR-11 | 0 |
| by_err.ERR-12 | 0 |
| by_err.ERR-12a | 0 |
| by_err.ERR-12b | 0 |
| by_err.ERR-12c | 0 |
| by_err.ERR-12d | 0 |
| by_err.ERR-13 | 0 |
| by_err.ERR-14 | 0 |
| by_err.ERR-15 | 0 |
| by_err.ERR-16 | 0 |
| unmatched | {} |
| transport_errors.total | 0 |
| transport_errors.by_kind | {} |
| decisions.allowed | 18175 |
| decisions.degraded | 0 |
| releases | 0 |
| refunds.applied | 0 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 18175 |
| prf2.new_rows | 18175 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 0 |
| prf2.quota_rejections | 6864 |
| prf2.capacity_refusals | 0 |
| prf2.integrity_failures | 0 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 0 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 18175 |

### results.attempt_outcomes

| name | 200 | 429 rate_limited |
|---|---|---|
| initial | 17105 | 2856 |
| duplicate | null | null |
| retry | 1070 | 4008 |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| fault_windows | [] |
| total.attempts | 19961 |
| total.unexpected | 0 |
| total.rate | 0 |
| total.transport_errors | 0 |
| total.server_errors | 0 |
| total.excluded_service_aborted_after_acquire | 0 |
| total.excluded_fault_window | 0 |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 60000000000 | 5511 | 0 | 0 | 0 | 0 | 0 | 0 |
| 1 | 65000000000 | 70000000000 | 2469 | 0 | 0 | 0 | 0 | 0 | 0 |
| 2 | 75000000000 | 160000000000 | 8516 | 0 | 0 | 0 | 0 | 0 | 0 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 6941 | 5714 | 5714 | 1227 |
| 1 | 6859 | 3911 | 3911 | 2948 |
| 2 | 11239 | 8550 | 8550 | 2689 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 18175 |
| db_ns.p50 | 702875 |
| db_ns.p95 | 6829708 |
| db_ns.p99 | 16771250 |
| api_total_ns.count | 25039 |
| api_total_ns.p50 | 10015958 |
| api_total_ns.p95 | 401879375 |
| api_total_ns.p99 | 521129209 |
| rewrite_windows | [] |
| rewrite_p99_ns | null |
| rewrite_latency_ns | null |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 18175 | 9358667 | 390875541 | 509526166 |
| limited | 6864 | 9110583 | 402226292 | 519428000 |
| degraded | 0 | null | null | null |
| error | 0 | null | null | null |

### timing.refund_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 0 | null | null | null |
| applied | 0 | null | null | null |
| already_applied | 0 | null | null | null |
| receipt_expired | 0 | null | null | null |
| receipt_invalid | 0 | null | null | null |
| unresolved | 0 | null | null | null |

### timing.end_to_end_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 19961 | 10526500 | 405694084 | 521603667 |
| success | 17105 | 10651500 | 404106875 | 524129375 |
| limited | 2856 | 9268375 | 415081542 | 512001917 |
| error | 0 | null | null | null |
| retries | 5078 | 10428208 | 394690750 | 524457375 |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 60000000000 | 5511 | 9686708 | 307751625 | 515537584 |
| 1 | 65000000000 | 70000000000 | 2469 | 15393542 | 502413958 | 547440000 |
| 2 | 75000000000 | 160000000000 | 8516 | 9706833 | 379428167 | 513638375 |

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
| t0_wall_ns | 1791643961893790000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 161 |
| health_reads | 483 |
| max_in_flight | 118 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 4.32 | 12.41 | 38108268 | 43319296 | 161 | 161 |
| hitapi-2 | 4.33 | 12.46 | 38633777 | 43335680 | 161 | 161 |
| hitapi-3 | 4.36 | 12.75 | 37787813 | 42827776 | 161 | 161 |
| hitgen | 11.2 | 33.19 | 41028995 | 49381376 | 161 | 161 |
| postgres | 3.78 | 12.32 | 956866712 | 1409400832 | 161 | 161 |
| sampler | 9.34 | 11.42 | 28235941 | 28950528 | 161 | 161 |
| valkey-server | 5.5 | 13.51 | 10600855 | 11141120 | 161 | 161 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 0 | 0 | 149 |
| quarantined_keys | 0 | 0 | 149 |
| tracked_buckets | 2230 | 1543 | 149 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 106 | 0 | 161 |
| 1 | listener_drops | 0 | 0 | 161 |
| 1 | log_drops | 0 | 0 | 161 |
| 1 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 161 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 161 |
| 1 | rl_cleanup_entries_total{action="removed"} | 441 | 441 | 161 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 53 | 53 | 161 |
| 1 | rl_cleanup_lag_seconds | 4.44708 | 1.70479 | 161 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_passes_total{status="complete"} | 33 | 33 | 161 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 161 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 161 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 161 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 161 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 161 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 161 |
| 1 | rl_listener_drops_total | 0 | 0 | 161 |
| 1 | rl_live_markers | 0 | 0 | 161 |
| 1 | rl_pending_refunds | 0 | 0 | 161 |
| 1 | rl_quarantined_keys | 0 | 0 | 161 |
| 1 | rl_tracked_buckets | 2229 | 1543 | 161 |
| 2 | in_flight | 118 | 0 | 161 |
| 2 | listener_drops | 0 | 0 | 161 |
| 2 | log_drops | 0 | 0 | 161 |
| 2 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 161 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 161 |
| 2 | rl_cleanup_entries_total{action="removed"} | 448 | 448 | 161 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 161 |
| 2 | rl_cleanup_lag_seconds | 4.82248 | 0.106706 | 161 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_passes_total{status="complete"} | 33 | 33 | 161 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 161 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 161 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 161 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 161 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 161 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 161 |
| 2 | rl_listener_drops_total | 0 | 0 | 161 |
| 2 | rl_live_markers | 0 | 0 | 161 |
| 2 | rl_pending_refunds | 0 | 0 | 161 |
| 2 | rl_quarantined_keys | 0 | 0 | 161 |
| 2 | rl_tracked_buckets | 2229 | 1543 | 161 |
| 3 | in_flight | 112 | 0 | 161 |
| 3 | listener_drops | 0 | 0 | 161 |
| 3 | log_drops | 0 | 0 | 161 |
| 3 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 161 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 161 |
| 3 | rl_cleanup_entries_total{action="removed"} | 1267 | 1267 | 161 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 82 | 82 | 161 |
| 3 | rl_cleanup_lag_seconds | 4.85696 | 4.61229 | 161 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_passes_total{status="complete"} | 33 | 33 | 161 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 161 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 161 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 161 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 161 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 161 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 161 |
| 3 | rl_listener_drops_total | 0 | 0 | 161 |
| 3 | rl_live_markers | 0 | 0 | 161 |
| 3 | rl_pending_refunds | 0 | 0 | 161 |
| 3 | rl_quarantined_keys | 0 | 0 | 161 |
| 3 | rl_tracked_buckets | 2229 | 1543 | 161 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 52224 | null |
| hitapi | 52229 | 1 |
| hitapi | 52230 | 2 |
| hitapi | 52231 | 3 |
| sampler | 52232 | null |
| hitgen | 52233 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | runs/20261010-145241-s4-spike-1/schedule.jsonl |
| schedule_sha256 | c1db98cbcb8290f0b28e1882c4ff67601332384356b8828e96f6d0116d54c885 |
| schedule_sha256_context | c1db98cbcb8290f0b28e1882c4ff67601332384356b8828e96f6d0116d54c885 |
| schedule_sha256_matches | true |
| schedule_entries | 19961 |
| planned_per_step | [6002,4950,9009] |
| instances | 3 |
| requests_per_instance.1 | 6654 |
| requests_per_instance.2 | 6654 |
| requests_per_instance.3 | 6653 |
| served_per_instance.1 | 8310 |
| served_per_instance.2 | 8390 |
| served_per_instance.3 | 8339 |
| instances_used | 3 |
| users | 3344 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 3081 |
| busiest_user.instances | [1,2,3] |

## Baseline comparison

| field | value |
|---|---|
| run_id | 20261010-144959-s4-spike-1 |
| source | thresholds.json |
| schedule_sha256 | c1db98cbcb8290f0b28e1882c4ff67601332384356b8828e96f6d0116d54c885 |
| same_schedule | true |
| problem | null |

### baseline.steps

| step | p99_ns | baseline_p99_ns | p99_delta_ns | unexpected_rate | baseline_unexpected_rate | unexpected_rate_delta | baseline_ran | baseline_step_exists | baseline_generator_limited |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 515537584 | 9955292 | 505582292 | 0 | 0 | 0 | full | true | false |
| 1 | 547440000 | 9231208 | 538208792 | 0 | 0 | 0 | full | true | false |
| 2 | 513638375 | 8959333 | 504679042 | 0 | 0 | 0 | full | true | false |

## Correctness (BUD-11, BUD-11a)

| field | value |
|---|---|
| verdict | pass |
| known_credits | 0 |
| unknown_credits | 0 |
| listener_drops | 0 |
| pre_kill_lines | 0 |
| pre_kill_read_ok_false | 0 |
| invalid_reasons | [] |
| violation | null |

## Capacity (T-RPT-2)

| field | value |
|---|---|
| verdict | spike |
| thresholds.file | thresholds.json |
| thresholds.sha256 | 800e7e9153b31e546a9245e28c0e9dba40ac90d51637eb9757bde78e8aca0415 |
| thresholds.context_sha256 | 800e7e9153b31e546a9245e28c0e9dba40ac90d51637eb9757bde78e8aca0415 |
| thresholds.created_ns | 1791643960968164000 |
| thresholds.baseline_run_id | 20261010-144959-s4-spike-1 |
| thresholds.schedule_sha256 | c1db98cbcb8290f0b28e1882c4ff67601332384356b8828e96f6d0116d54c885 |
| problem | null |
| sustainable_capacity | null |
| sustainable_step | null |
| spike.spike_step | 1 |
| spike.post_spike_step | 2 |
| spike.spike_end_ns | 70000000000 |
| spike.deadline_ns | 100000000000 |
| spike.window_start_ns | 100000000000 |
| spike.window_end_ns | 160000000000 |
| spike.clock | hitgen's monotonic clock, ns from its t0 (as planned_ns) |
| spike.recorded.spike_step | 1 |
| spike.recorded.post_spike_step | 2 |
| spike.recorded.spike_end_ns | 70000000000 |
| spike.recorded.deadline_ns | 100000000000 |
| spike.recorded.window_start_ns | 100000000000 |
| spike.recorded.window_end_ns | 160000000000 |
| spike.latency_threshold_ns | 58959333 |
| spike.error_rate_threshold | 0.001 |
| spike.window_latency_ns.count | 6045 |
| spike.window_latency_ns.p50 | 9972458 |
| spike.window_latency_ns.p95 | 430918584 |
| spike.window_latency_ns.p99 | 522608042 |
| spike.window_attempts | 6045 |
| spike.window_unexpected | 0 |
| spike.window_unexpected_rate | 0 |
| spike.window_planned | 6045 |
| spike.window_unsent | 0 |
| spike.outstanding_cutoff_ns | 99941040667 |
| spike.planned_before_deadline | 13916 |
| spike.first_attempts_before_deadline | 13916 |
| spike.finished_by_deadline | 13911 |
| spike.not_judged | 5 |
| spike.outstanding | 0 |
| spike.outstanding_detail.never_written | 0 |
| spike.outstanding_detail.ended_after_deadline | 0 |
| spike.outstanding_detail.no_line | 0 |
| spike.latency_ok | false |
| spike.errors_ok | true |
| spike.met | false |
| spike.problems | ["window p99 above the post-spike step's latency threshold"] |
| rule | thresholds.json is used only when its schedule_sha256 is this run's; a step passes iff it is not generator-limited, ran in full, its baseline step exists and is not generator-limited (from thresholds.json: a step with thresholds exists and is not, one marked generator_limited is, one not named is missing), its window p99 ≤ latency_threshold_ns, its window unexpected-error rate ≤ error_rate_threshold, unsent = 0 and it had no ERR-9 or ERR-16; excluded steps get no verdict; sustainable_capacity = the highest achieved_sent_per_s of a passing step (PRF-11); spike: deadline = spike end + 30 s, window = [deadline, deadline + 60 s] by planned_ns, judged against the post-spike step's thresholds; outstanding at the deadline (PRF-6, the orchestrator's reading of plan H5 step 4): a first attempt planned before outstanding_cutoff_ns (deadline − the post-spike step's latency_threshold_ns) with no response or failure by the deadline, and every first attempt planned before the deadline never written or never launched |

### capacity.steps

| step | ran | offered_per_s | achieved_sent_per_s | admitted | admitted_per_s | limited_429 | limited_429_per_s | unique_rows | unique_rows_per_s | p99_ns | latency_threshold_ns | latency_ok | window_attempts | window_unexpected | unexpected_rate | error_rate_threshold | errors_ok | unsent | unknown_outcomes | generator_limited | baseline_step_exists | baseline_generator_limited | verdict | excluded | failed |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 0 | full | 100.033 | 100.033 | 5714 | 95.2333 | 1227 | 20.45 | 5714 | 95.2333 | 515537584 | 59955292 | false | 5511 | 0 | 0 | 0.001 | true | 0 | 0 | false | true | false | fail | null | ["p99"] |
| 1 | full | 495 | 495 | 3911 | 391.1 | 2948 | 294.8 | 3911 | 391.1 | 547440000 | 59231208 | false | 2469 | 0 | 0 | 0.001 | true | 0 | 0 | false | true | false | fail | null | ["p99"] |
| 2 | full | 100.1 | 100.1 | 8550 | 95 | 2689 | 29.8778 | 8550 | 95 | 513638375 | 58959333 | false | 8516 | 0 | 0 | 0.001 | true | 0 | 0 | false | true | false | fail | null | ["p99"] |

## Evidence validity (§B8)

| field | value |
|---|---|
| valid | true |

### evidence_validity.checks

| name | ok | value | detail |
|---|---|---|---|
| log_drops | true | 0 | log_drops summed over every stop and pre_kill line of every api-N.jsonl |
| listener_drops | true | {"sum":0,"stop_lines":3,"pre_kill_lines":0,"pre_kill_unread":0,"segments_without_count":0} | HAR-13: listener_drops summed over every stop and pre_kill line is 0, every pre_kill read succeeded (read_ok), and every process ended with one of the two |
| fixtures_off | true | {"applies":true,"health_reads":483,"fixtures_on":0,"clock_not_store":0,"faults":0} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 0 | store outage segments armed: faultctl store down or pause, and timed store refusals |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":false,"windows":0} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | true | {"verdict":"pass","invalid_reasons":[]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
