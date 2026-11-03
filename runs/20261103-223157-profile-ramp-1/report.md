# Run report 20261103-223157-profile-ramp-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261103-223157-profile-ramp-1 |
| epoch_id | c7faf868970d081f77d306586781b420 |
| run_kind | test |
| run_label | test-build scenario, not a benchmark |
| baseline | false |
| outcome | ok |
| start_ns | 1793745117663280000 |
| end_ns | 1793745360411036000 |
| scenario.name | profile-ramp |
| scenario.path | scenarios/profile-ramp.toml |
| scenario.sha256 | 00e97f352c832db5120d0e4684d6ae50be3545852c387caa83d5dab354452297 |
| seed | 1 |
| instances | 3 |
| hardware.model | Mac15,9 |
| hardware.ncpu | 16 |
| hardware.memsize_bytes | 137438953472 |
| os.BuildVersion | 26A434 |
| os.ProductName | macOS |
| os.ProductVersion | 27.0.1 |
| versions.go | go1.27.1 |
| versions.valkey | Valkey server v=9.1.2 sha=00000000:1 malloc=libc bits=64 build=1af85cd1d1bb3237 |
| versions.valkey_path | /opt/homebrew/bin/valkey-server |
| versions.postgres | postgres (PostgreSQL) 14.19 (Homebrew) |
| store_profile | everysec |
| store_settings.aclfile | /Users/srirondla/distributed-systems/ratelimiter/runs/20261103-223157-profile-ramp-1/store/acl.conf |
| store_settings.aof-use-rdb-preamble | yes |
| store_settings.appendfsync | everysec |
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
| schedule_sha256 | 4e1d9cb0a7271c4fef421897538971d34830235c9c4f21b70f5879beb357e569 |
| thresholds_sha256 | null |
| faults | [] |
| rewrites | [] |
| label | test-build scenario, not a benchmark |
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
| hitgen | hitgen | 38744 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 38740 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 38741 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 38742 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 38743 | exit 0 | false | 10000000000 | false | null |

### context.workload

| field | value |
|---|---|
| scenario_file | scenarios/profile-ramp.toml |
| step_bounds_ns | [0,30000000000,60000000000,90000000000,120000000000,150000000000,180000000000,210000000000,240000000000] |
| users | 10000 |
| instances | 3 |
| verdict | none |
| spike_step | null |
| forced_aof_rewrites_s | [] |
| stop_early | false |

#### context.workload.steps

| rate_per_s | duration_s |
|---|---|
| 100 | 30 |
| 200 | 30 |
| 400 | 30 |
| 800 | 30 |
| 1600 | 30 |
| 3200 | 30 |
| 6400 | 30 |
| 10000 | 30 |

## Load (PRF-1)

| field | value |
|---|---|
| total.scheduled | 680262 |
| total.planned | 680262 |
| total.sent | 680262 |
| total.initial | 680262 |
| total.duplicates | 0 |
| total.retries_429 | 705445 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 680262 |
| total.sched_lag_ns.p50 | 535684 |
| total.sched_lag_ns.p95 | 995521 |
| total.sched_lag_ns.p99 | 1078164 |
| total.got_conn_wait_ns_p99 | 18667 |
| stopped_after_step | null |
| excluded_step | null |
| summary.planned | 680262 |
| summary.launched | 680262 |
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
| 0 | 100 | 30 | 0 | 30000000000 | full | 3006 | 3006 | 3006 | 3006 | 0 | 388 | 0 | 100.2 | 100.2 | 3006 | 571886 | 1093248 | 1436255 | 52417 | false | 2889 | 505 | 0 | 0 |
| 1 | 200 | 30 | 30000000000 | 60000000000 | full | 5960 | 5960 | 5960 | 5960 | 0 | 1889 | 0 | 198.667 | 198.667 | 5960 | 567007 | 1082073 | 1332754 | 47792 | false | 5269 | 2580 | 0 | 0 |
| 2 | 400 | 30 | 60000000000 | 90000000000 | full | 11994 | 11994 | 11994 | 11994 | 0 | 5688 | 0 | 399.8 | 399.8 | 11994 | 542029 | 1069970 | 1244664 | 17625 | false | 9658 | 8024 | 0 | 0 |
| 3 | 800 | 30 | 90000000000 | 120000000000 | full | 24053 | 24053 | 24053 | 24053 | 0 | 15435 | 0 | 801.767 | 801.767 | 24053 | 539213 | 1060238 | 1229616 | 17625 | false | 17277 | 22211 | 0 | 0 |
| 4 | 1600 | 30 | 120000000000 | 150000000000 | full | 47949 | 47949 | 47949 | 47949 | 0 | 37920 | 0 | 1598.3 | 1598.3 | 47949 | 526344 | 1019214 | 1144679 | 4750 | false | 30818 | 55051 | 0 | 0 |
| 5 | 3200 | 30 | 150000000000 | 180000000000 | full | 95593 | 95593 | 95593 | 95593 | 0 | 89651 | 0 | 3186.43 | 3186.43 | 95593 | 530384 | 1002168 | 1111863 | 6167 | false | 54282 | 130962 | 0 | 0 |
| 6 | 6400 | 30 | 180000000000 | 210000000000 | full | 191403 | 191403 | 191403 | 191403 | 0 | 205986 | 0 | 6380.1 | 6380.1 | 191403 | 532570 | 988257 | 1055556 | 14000 | false | 95137 | 302252 | 0 | 0 |
| 7 | 10000 | 30 | 210000000000 | 240000000000 | full | 300304 | 300304 | 300304 | 300304 | 0 | 348488 | 0 | 10010.1 | 10010.1 | 300304 | 539496 | 990065 | 1046097 | 24917 | false | 137253 | 511539 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 1385707 |
| by_err.ERR-1 | 352583 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 0 |
| by_err.ERR-4 | 1033124 |
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
| decisions.allowed | 352583 |
| decisions.degraded | 0 |
| releases | 0 |
| refunds.applied | 0 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 352583 |
| prf2.new_rows | 352583 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 0 |
| prf2.quota_rejections | 1033124 |
| prf2.capacity_refusals | 0 |
| prf2.integrity_failures | 0 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 0 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 352583 |

### results.attempt_outcomes

| name | 200 | 429 rate_limited |
|---|---|---|
| initial | 318394 | 361868 |
| duplicate | null | null |
| retry | 34189 | 671256 |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| fault_windows | [] |
| total.attempts | 680262 |
| total.unexpected | 0 |
| total.rate | 0 |
| total.transport_errors | 0 |
| total.server_errors | 0 |
| total.excluded_service_aborted_after_acquire | 0 |
| total.excluded_fault_window | 0 |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 30000000000 | 2515 | 0 | 0 | 0 | 0 | 0 | 0 |
| 1 | 35000000000 | 60000000000 | 4992 | 0 | 0 | 0 | 0 | 0 | 0 |
| 2 | 65000000000 | 90000000000 | 10009 | 0 | 0 | 0 | 0 | 0 | 0 |
| 3 | 95000000000 | 120000000000 | 20000 | 0 | 0 | 0 | 0 | 0 | 0 |
| 4 | 125000000000 | 150000000000 | 39847 | 0 | 0 | 0 | 0 | 0 | 0 |
| 5 | 155000000000 | 180000000000 | 79634 | 0 | 0 | 0 | 0 | 0 | 0 |
| 6 | 185000000000 | 210000000000 | 159640 | 0 | 0 | 0 | 0 | 0 | 0 |
| 7 | 215000000000 | 240000000000 | 250318 | 0 | 0 | 0 | 0 | 0 | 0 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 3372 | 2883 | 2883 | 489 |
| 1 | 7798 | 5262 | 5262 | 2536 |
| 2 | 17445 | 9646 | 9646 | 7799 |
| 3 | 39050 | 17253 | 17253 | 21797 |
| 4 | 84619 | 30778 | 30778 | 53841 |
| 5 | 182640 | 54192 | 54192 | 128448 |
| 6 | 391411 | 94904 | 94904 | 296507 |
| 7 | 659372 | 137665 | 137665 | 521707 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 352583 |
| db_ns.p50 | 139959 |
| db_ns.p95 | 276000 |
| db_ns.p99 | 805292 |
| api_total_ns.count | 1385707 |
| api_total_ns.p50 | 240917 |
| api_total_ns.p95 | 676292 |
| api_total_ns.p99 | 1042458 |
| rewrite_windows | [] |
| rewrite_p99_ns | null |
| rewrite_latency_ns | null |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 352583 | 255875 | 621250 | 919584 |
| limited | 1033124 | 172334 | 559833 | 787333 |
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
| all | 680262 | 438000 | 884666 | 1556167 |
| success | 318394 | 513791 | 1012834 | 2205583 |
| limited | 361868 | 370250 | 757625 | 1019542 |
| error | 0 | null | null | null |
| retries | 705445 | 206250 | 640000 | 961917 |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 30000000000 | 2515 | 1549167 | 3642375 | 5005666 |
| 1 | 35000000000 | 60000000000 | 4992 | 730500 | 2862542 | 3878042 |
| 2 | 65000000000 | 90000000000 | 10009 | 584125 | 2098750 | 3054417 |
| 3 | 95000000000 | 120000000000 | 20000 | 520667 | 1165792 | 2778500 |
| 4 | 125000000000 | 150000000000 | 39847 | 403625 | 729334 | 972500 |
| 5 | 155000000000 | 180000000000 | 79634 | 326250 | 548917 | 669792 |
| 6 | 185000000000 | 210000000000 | 159640 | 376458 | 662208 | 819375 |
| 7 | 215000000000 | 240000000000 | 250318 | 531542 | 950625 | 1271875 |

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
| t0_wall_ns | 1793745118243076000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 242 |
| health_reads | 726 |
| max_in_flight | 8 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 29.4 | 104.73 | 34753172 | 42795008 | 242 | 242 |
| hitapi-2 | 29.36 | 105.97 | 34855605 | 40337408 | 242 | 242 |
| hitapi-3 | 29.41 | 104.14 | 34122726 | 42205184 | 242 | 242 |
| hitgen | 53.47 | 180.59 | 191280762 | 201326592 | 242 | 242 |
| postgres | 10.74 | 33.09 | 626359710 | 1706524672 | 242 | 242 |
| sampler | 6.34 | 8.49 | 29461410 | 30408704 | 242 | 242 |
| valkey-server | 23.66 | 76.26 | 11687140 | 13549568 | 242 | 242 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 0 | 0 | 242 |
| quarantined_keys | 0 | 0 | 242 |
| tracked_buckets | 9893 | 9893 | 242 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 8 | 0 | 242 |
| 1 | listener_drops | 0 | 0 | 242 |
| 1 | log_drops | 0 | 0 | 242 |
| 1 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 242 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 242 |
| 1 | rl_cleanup_entries_total{action="removed"} | 348 | 348 | 242 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 242 |
| 1 | rl_cleanup_lag_seconds | 4.2088 | 0.130837 | 242 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_passes_total{status="complete"} | 50 | 50 | 242 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 242 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 242 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 242 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 242 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 242 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 242 |
| 1 | rl_listener_drops_total | 0 | 0 | 242 |
| 1 | rl_live_markers | 0 | 0 | 242 |
| 1 | rl_pending_refunds | 0 | 0 | 242 |
| 1 | rl_quarantined_keys | 0 | 0 | 242 |
| 1 | rl_tracked_buckets | 9893 | 9893 | 242 |
| 2 | in_flight | 7 | 2 | 242 |
| 2 | listener_drops | 0 | 0 | 242 |
| 2 | log_drops | 0 | 0 | 242 |
| 2 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 242 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 242 |
| 2 | rl_cleanup_entries_total{action="removed"} | 390 | 390 | 242 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 242 |
| 2 | rl_cleanup_lag_seconds | 4.50392 | 1.18379 | 242 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_passes_total{status="complete"} | 50 | 50 | 242 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 242 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 242 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 242 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 242 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 242 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 242 |
| 2 | rl_listener_drops_total | 0 | 0 | 242 |
| 2 | rl_live_markers | 0 | 0 | 242 |
| 2 | rl_pending_refunds | 0 | 0 | 242 |
| 2 | rl_quarantined_keys | 0 | 0 | 242 |
| 2 | rl_tracked_buckets | 9893 | 9893 | 242 |
| 3 | in_flight | 8 | 0 | 242 |
| 3 | listener_drops | 0 | 0 | 242 |
| 3 | log_drops | 0 | 0 | 242 |
| 3 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 242 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 242 |
| 3 | rl_cleanup_entries_total{action="removed"} | 1839 | 1839 | 242 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 242 |
| 3 | rl_cleanup_lag_seconds | 4.4352 | 3.2084 | 242 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_passes_total{status="complete"} | 50 | 50 | 242 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 242 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 242 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 242 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 242 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 242 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 242 |
| 3 | rl_listener_drops_total | 0 | 0 | 242 |
| 3 | rl_live_markers | 0 | 0 | 242 |
| 3 | rl_pending_refunds | 0 | 0 | 242 |
| 3 | rl_quarantined_keys | 0 | 0 | 242 |
| 3 | rl_tracked_buckets | 9893 | 9893 | 242 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 38734 | null |
| hitapi | 38740 | 1 |
| hitapi | 38741 | 2 |
| hitapi | 38742 | 3 |
| sampler | 38743 | null |
| hitgen | 38744 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | /Users/srirondla/distributed-systems/ratelimiter/runs/20261103-223157-profile-ramp-1/schedule.jsonl |
| schedule_sha256 | 4e1d9cb0a7271c4fef421897538971d34830235c9c4f21b70f5879beb357e569 |
| schedule_sha256_context | 4e1d9cb0a7271c4fef421897538971d34830235c9c4f21b70f5879beb357e569 |
| schedule_sha256_matches | true |
| schedule_entries | 680262 |
| planned_per_step | [3006,5960,11994,24053,47949,95593,191403,300304] |
| instances | 3 |
| requests_per_instance.1 | 226754 |
| requests_per_instance.2 | 226754 |
| requests_per_instance.3 | 226754 |
| served_per_instance.1 | 461878 |
| served_per_instance.2 | 461961 |
| served_per_instance.3 | 461868 |
| instances_used | 3 |
| users | 9975 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 103612 |
| busiest_user.instances | [1,2,3] |

## Baseline comparison

Not applicable to this run, or not built yet.

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

Not applicable to this run, or not built yet.

## Evidence validity (§B8)

| field | value |
|---|---|
| valid | true |

### evidence_validity.checks

| name | ok | value | detail |
|---|---|---|---|
| log_drops | true | 0 | log_drops summed over every stop and pre_kill line of every api-N.jsonl |
| listener_drops | true | {"sum":0,"stop_lines":3,"pre_kill_lines":0,"pre_kill_unread":0,"segments_without_count":0} | HAR-13: listener_drops summed over every stop and pre_kill line is 0, every pre_kill read succeeded (read_ok), and every process segment holds one of the two, not necessarily as its last line |
| fixtures_off | true | {"applies":false,"health_reads":726,"fixtures_on":726,"clock_not_store":0,"faults":0} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 0 | store outage segments armed: faultctl store down or pause, and store refusals that are timed (a duration_ms above 0) or unbounded (neither count nor duration_ms) |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":false,"windows":0} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | true | {"verdict":"pass","invalid_reasons":[]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
