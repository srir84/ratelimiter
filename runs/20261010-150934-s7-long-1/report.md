# Run report 20261010-150934-s7-long-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261010-150934-s7-long-1 |
| epoch_id | 5170c43ba57b7bd83bbcbad6b62bfa05 |
| run_kind | bench |
| run_label | benchmark |
| baseline | false |
| outcome | ok |
| start_ns | 1791644974161430000 |
| end_ns | 1791645575514008000 |
| scenario.name | s7-long |
| scenario.path | scenarios/s7-long.toml |
| scenario.sha256 | 9f5015b39cc555d31f623effbd7ad56e5df57b8d6922a94634872e7c30d37308 |
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
| store_settings.aclfile | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-150934-s7-long-1/store/acl.conf |
| store_settings.aof-use-rdb-preamble | yes |
| store_settings.appendfsync | always |
| store_settings.appendonly | yes |
| store_settings.auto-aof-rewrite-min-size | 1073741824 |
| store_settings.auto-aof-rewrite-percentage | 100 |
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
| schedule_sha256 | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| thresholds_sha256 | 72d28477ef7ae4449e733e62886072a949a18700100b295b1b857edc75b9bc33 |
| faults | [] |
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
| api | postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64 | true | 512 | 200 | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null |
| instance | null | null | null | null | 1000000000 | 1152 | 1152 | 5000000000 | 128 | null | null | null | null | null | null | null | null | null | null | null | null | null |
| policy | null | null | null | null | null | null | null | null | null | 20 | 5000000000 | store | closed | 50000 | 1000 | 100000 | 5000000000 | 1000000000 | 10 | 60000000000 | 30000000000 | null |
| store | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | 127.0.0.1:6390 |

### context.rewrites

| at_s | start_ns | end_ns | status |
|---|---|---|---|
| 180 | 1791645154694050000 | 1791645154920548000 | ok |
| 420 | 1791645394639892000 | 1791645394771501000 | ok |

### context.children

| name | role | pid | exit | exited_before_stop | grace_ns | killed | instance |
|---|---|---|---|---|---|---|---|
| hitgen | hitgen | 54974 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 54970 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 54971 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 54972 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 54973 | exit 0 | false | 10000000000 | false | null |

### context.workload

| field | value |
|---|---|
| scenario_file | scenarios/s7-long.toml |
| step_bounds_ns | [0,50000000000,100000000000,150000000000,200000000000,250000000000,300000000000,350000000000,400000000000,450000000000,500000000000,550000000000,600000000000] |
| users | 10000 |
| instances | 3 |
| verdict | none |
| spike_step | null |
| forced_aof_rewrites_s | [180.0,420.0] |
| stop_early | false |

#### context.workload.steps

| rate_per_s | duration_s |
|---|---|
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |
| 70 | 50 |

## Load (PRF-1)

| field | value |
|---|---|
| total.scheduled | 42004 |
| total.planned | 42004 |
| total.sent | 42004 |
| total.initial | 42004 |
| total.duplicates | 0 |
| total.retries_429 | 1014 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 42004 |
| total.sched_lag_ns.p50 | 609326 |
| total.sched_lag_ns.p95 | 1140173 |
| total.sched_lag_ns.p99 | 2126284 |
| total.got_conn_wait_ns_p99 | 446250 |
| stopped_after_step | null |
| excluded_step | null |
| summary.planned | 42004 |
| summary.launched | 42004 |
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
| 0 | 70 | 50 | 0 | 50000000000 | full | 3485 | 3485 | 3485 | 3485 | 0 | 14 | 0 | 69.7 | 69.7 | 3485 | 624110 | 1143534 | 2279528 | 316584 | false | 3412 | 14 | 0 | 0 |
| 1 | 70 | 50 | 50000000000 | 100000000000 | full | 3532 | 3532 | 3532 | 3532 | 0 | 110 | 0 | 70.64 | 70.64 | 3532 | 606606 | 1164771 | 2008557 | 521167 | false | 3461 | 121 | 0 | 0 |
| 2 | 70 | 50 | 100000000000 | 150000000000 | full | 3458 | 3458 | 3458 | 3458 | 0 | 272 | 0 | 69.16 | 69.16 | 3458 | 602154 | 1122109 | 1636476 | 203542 | false | 3351 | 316 | 0 | 0 |
| 3 | 70 | 50 | 150000000000 | 200000000000 | full | 3448 | 3448 | 3448 | 3448 | 0 | 195 | 0 | 68.96 | 68.96 | 3448 | 616854 | 1121955 | 1798690 | 379458 | false | 3342 | 216 | 0 | 0 |
| 4 | 70 | 50 | 200000000000 | 250000000000 | full | 3492 | 3492 | 3492 | 3492 | 0 | 0 | 0 | 69.84 | 69.84 | 3492 | 608163 | 1142469 | 2632626 | 664208 | false | 3421 | 0 | 0 | 0 |
| 5 | 70 | 50 | 250000000000 | 300000000000 | full | 3548 | 3548 | 3548 | 3548 | 0 | 0 | 0 | 70.96 | 70.96 | 3548 | 616541 | 1153025 | 2246034 | 612500 | false | 3462 | 0 | 0 | 0 |
| 6 | 70 | 50 | 300000000000 | 350000000000 | full | 3539 | 3539 | 3539 | 3539 | 0 | 0 | 0 | 70.78 | 70.78 | 3539 | 607128 | 1160337 | 2094659 | 768375 | false | 3479 | 0 | 0 | 0 |
| 7 | 70 | 50 | 350000000000 | 400000000000 | full | 3514 | 3514 | 3514 | 3514 | 0 | 0 | 0 | 70.28 | 70.28 | 3514 | 614963 | 1173462 | 2136618 | 409125 | false | 3450 | 0 | 0 | 0 |
| 8 | 70 | 50 | 400000000000 | 450000000000 | full | 3450 | 3450 | 3450 | 3450 | 0 | 7 | 0 | 69 | 69 | 3450 | 603397 | 1148713 | 2421553 | 167625 | false | 3380 | 9 | 0 | 0 |
| 9 | 70 | 50 | 450000000000 | 500000000000 | full | 3623 | 3623 | 3623 | 3623 | 0 | 320 | 0 | 72.46 | 72.46 | 3623 | 600425 | 1158362 | 2597375 | 399500 | false | 3491 | 378 | 0 | 0 |
| 10 | 70 | 50 | 500000000000 | 550000000000 | full | 3453 | 3453 | 3453 | 3453 | 0 | 51 | 0 | 69.06 | 69.06 | 3453 | 604408 | 1103682 | 1742252 | 416250 | false | 3380 | 61 | 0 | 0 |
| 11 | 70 | 50 | 550000000000 | 600000000000 | full | 3462 | 3462 | 3462 | 3462 | 0 | 45 | 0 | 69.24 | 69.24 | 3462 | 606437 | 1116719 | 2275934 | 375291 | false | 3393 | 48 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 43018 |
| by_err.ERR-1 | 41022 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 0 |
| by_err.ERR-4 | 1163 |
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
| by_err.ERR-15 | 833 |
| by_err.ERR-16 | 0 |
| unmatched | {} |
| transport_errors.total | 0 |
| transport_errors.by_kind | {} |
| decisions.allowed | 41855 |
| decisions.degraded | 0 |
| releases | 833 |
| refunds.applied | 833 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 41022 |
| prf2.new_rows | 41022 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 0 |
| prf2.quota_rejections | 1163 |
| prf2.capacity_refusals | 0 |
| prf2.integrity_failures | 0 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 833 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 41022 |

### results.attempt_outcomes

| name | 200 | 429 rate_limited | 503 service_aborted_after_acquire |
|---|---|---|---|
| initial | 40475 | 711 | 818 |
| duplicate | null | null | null |
| retry | 547 | 452 | 15 |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| fault_windows | [] |
| total.attempts | 42004 |
| total.unexpected | 0 |
| total.rate | 0 |
| total.transport_errors | 0 |
| total.server_errors | 818 |
| total.excluded_service_aborted_after_acquire | 818 |
| total.excluded_fault_window | 0 |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 50000000000 | 3138 | 0 | 0 | 0 | 70 | 70 | 0 |
| 1 | 55000000000 | 100000000000 | 3173 | 0 | 0 | 0 | 48 | 48 | 0 |
| 2 | 105000000000 | 150000000000 | 3079 | 0 | 0 | 0 | 55 | 55 | 0 |
| 3 | 155000000000 | 200000000000 | 3102 | 0 | 0 | 0 | 77 | 77 | 0 |
| 4 | 205000000000 | 250000000000 | 3116 | 0 | 0 | 0 | 65 | 65 | 0 |
| 5 | 255000000000 | 300000000000 | 3163 | 0 | 0 | 0 | 78 | 78 | 0 |
| 6 | 305000000000 | 350000000000 | 3175 | 0 | 0 | 0 | 58 | 58 | 0 |
| 7 | 355000000000 | 400000000000 | 3161 | 0 | 0 | 0 | 57 | 57 | 0 |
| 8 | 405000000000 | 450000000000 | 3119 | 0 | 0 | 0 | 60 | 60 | 0 |
| 9 | 455000000000 | 500000000000 | 3236 | 0 | 0 | 0 | 62 | 62 | 0 |
| 10 | 505000000000 | 550000000000 | 3087 | 0 | 0 | 0 | 54 | 54 | 0 |
| 11 | 555000000000 | 600000000000 | 3125 | 0 | 0 | 0 | 58 | 58 | 0 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 3496 | 3409 | 3482 | 14 |
| 1 | 3645 | 3464 | 3524 | 121 |
| 2 | 3715 | 3343 | 3406 | 309 |
| 3 | 3657 | 3349 | 3434 | 223 |
| 4 | 3498 | 3427 | 3498 | 0 |
| 5 | 3546 | 3460 | 3546 | 0 |
| 6 | 3538 | 3478 | 3538 | 0 |
| 7 | 3514 | 3450 | 3514 | 0 |
| 8 | 3453 | 3380 | 3448 | 5 |
| 9 | 3942 | 3488 | 3562 | 380 |
| 10 | 3510 | 3384 | 3447 | 63 |
| 11 | 3504 | 3390 | 3456 | 48 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 41022 |
| db_ns.p50 | 679584 |
| db_ns.p95 | 4417917 |
| db_ns.p99 | 10154083 |
| api_total_ns.count | 43018 |
| api_total_ns.p50 | 9008250 |
| api_total_ns.p95 | 361213292 |
| api_total_ns.p99 | 510932625 |
| rewrite_p99_ns | 39993500 |
| rewrite_latency_ns.count | 20 |
| rewrite_latency_ns.p50 | 9803666 |
| rewrite_latency_ns.p95 | 38358750 |
| rewrite_latency_ns.p99 | 39993500 |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 41855 | 8223584 | 356967875 | 504106291 |
| limited | 1163 | 1527166 | 230634292 | 475158834 |
| degraded | 0 | null | null | null |
| error | 0 | null | null | null |

### timing.refund_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 833 | 8929625 | 511671000 | 536305125 |
| applied | 833 | 8929625 | 511671000 | 536305125 |
| already_applied | 0 | null | null | null |
| receipt_expired | 0 | null | null | null |
| receipt_invalid | 0 | null | null | null |
| unresolved | 0 | null | null | null |

### timing.end_to_end_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 42004 | 9610958 | 365975083 | 512440625 |
| success | 40475 | 9663458 | 366050000 | 512837833 |
| limited | 711 | 2214917 | 338407708 | 478530583 |
| error | 818 | 9207500 | 377465625 | 512440625 |
| retries | 1014 | 8203750 | 138868542 | 470915166 |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 50000000000 | 3138 | 9385667 | 354659375 | 486609042 |
| 1 | 55000000000 | 100000000000 | 3173 | 9694834 | 375848417 | 507379042 |
| 2 | 105000000000 | 150000000000 | 3079 | 9463958 | 356106667 | 491985125 |
| 3 | 155000000000 | 200000000000 | 3102 | 9604583 | 380043958 | 522513167 |
| 4 | 205000000000 | 250000000000 | 3116 | 9523584 | 303772667 | 496134833 |
| 5 | 255000000000 | 300000000000 | 3163 | 9739875 | 342641959 | 504785792 |
| 6 | 305000000000 | 350000000000 | 3175 | 9788708 | 453691667 | 1105214584 |
| 7 | 355000000000 | 400000000000 | 3161 | 9799416 | 387031833 | 539300416 |
| 8 | 405000000000 | 450000000000 | 3119 | 9630834 | 311283709 | 434179041 |
| 9 | 455000000000 | 500000000000 | 3236 | 9537584 | 363530958 | 520337625 |
| 10 | 505000000000 | 550000000000 | 3087 | 9443542 | 375167042 | 493745708 |
| 11 | 555000000000 | 600000000000 | 3125 | 9657458 | 379962500 | 511068291 |

### timing.rewrite_windows

| start_ns | end_ns | source |
|---|---|---|
| 1791645154694050000 | 1791645154920548000 | context |
| 1791645394639892000 | 1791645394771501000 | context |
| 1791645154706610000 | 1791645154706610000 | samples |
| 1791645394638206000 | 1791645394638206000 | samples |

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
| t0_wall_ns | 1791644974684460000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 600 |
| health_reads | 1800 |
| max_in_flight | 26 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 3.06 | 4.67 | 34761932 | 35815424 | 600 | 600 |
| hitapi-2 | 3.03 | 4.93 | 34527532 | 36651008 | 600 | 600 |
| hitapi-3 | 3 | 4.82 | 34342447 | 36257792 | 600 | 600 |
| hitgen | 9.7 | 14.16 | 38007029 | 39501824 | 600 | 600 |
| postgres | 2.68 | 6.65 | 572859269 | 809385984 | 600 | 600 |
| sampler | 9.48 | 12.05 | 29358680 | 30146560 | 600 | 600 |
| valkey-server | 4.41 | 8.36 | 11344855 | 11796480 | 600 | 600 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 111 | 84 | 561 |
| quarantined_keys | 0 | 0 | 561 |
| tracked_buckets | 2985 | 1191 | 561 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 26 | 0 | 600 |
| 1 | listener_drops | 0 | 0 | 600 |
| 1 | log_drops | 0 | 0 | 600 |
| 1 | rl_cleanup_entries_total{action="moved"} | 180 | 180 | 600 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 600 |
| 1 | rl_cleanup_entries_total{action="removed"} | 4201 | 4201 | 600 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 602 | 602 | 600 |
| 1 | rl_cleanup_lag_seconds | 5.58821 | 0.67518 | 600 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_passes_total{status="complete"} | 122 | 122 | 600 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 600 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 600 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 600 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 600 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 600 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 600 |
| 1 | rl_listener_drops_total | 0 | 0 | 600 |
| 1 | rl_live_markers | 111 | 84 | 600 |
| 1 | rl_pending_refunds | 1 | 0 | 600 |
| 1 | rl_quarantined_keys | 0 | 0 | 600 |
| 1 | rl_tracked_buckets | 3002 | 1186 | 600 |
| 2 | in_flight | 26 | 0 | 600 |
| 2 | listener_drops | 0 | 0 | 600 |
| 2 | log_drops | 0 | 0 | 600 |
| 2 | rl_cleanup_entries_total{action="moved"} | 188 | 188 | 600 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 600 |
| 2 | rl_cleanup_entries_total{action="removed"} | 4693 | 4693 | 600 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 288 | 288 | 600 |
| 2 | rl_cleanup_lag_seconds | 6.45904 | 3.87553 | 600 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_passes_total{status="complete"} | 121 | 121 | 600 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 600 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 600 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 600 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 600 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 600 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 600 |
| 2 | rl_listener_drops_total | 0 | 0 | 600 |
| 2 | rl_live_markers | 111 | 84 | 600 |
| 2 | rl_pending_refunds | 1 | 0 | 600 |
| 2 | rl_quarantined_keys | 0 | 0 | 600 |
| 2 | rl_tracked_buckets | 3002 | 1186 | 600 |
| 3 | in_flight | 26 | 0 | 600 |
| 3 | listener_drops | 0 | 0 | 600 |
| 3 | log_drops | 0 | 0 | 600 |
| 3 | rl_cleanup_entries_total{action="moved"} | 138 | 138 | 600 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 600 |
| 3 | rl_cleanup_entries_total{action="removed"} | 2469 | 2469 | 600 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 552 | 552 | 600 |
| 3 | rl_cleanup_lag_seconds | 5.16563 | 0.034154 | 600 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_passes_total{status="complete"} | 122 | 122 | 600 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 600 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 600 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 600 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 600 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 600 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 600 |
| 3 | rl_listener_drops_total | 0 | 0 | 600 |
| 3 | rl_live_markers | 111 | 84 | 600 |
| 3 | rl_pending_refunds | 1 | 0 | 600 |
| 3 | rl_quarantined_keys | 0 | 0 | 600 |
| 3 | rl_tracked_buckets | 3002 | 1186 | 600 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 54965 | null |
| hitapi | 54970 | 1 |
| hitapi | 54971 | 2 |
| hitapi | 54972 | 3 |
| sampler | 54973 | null |
| hitgen | 54974 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-150934-s7-long-1/schedule.jsonl |
| schedule_sha256 | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| schedule_sha256_context | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| schedule_sha256_matches | true |
| schedule_entries | 42004 |
| planned_per_step | [3485,3532,3458,3448,3492,3548,3539,3514,3450,3623,3453,3462] |
| instances | 3 |
| requests_per_instance.1 | 14002 |
| requests_per_instance.2 | 14001 |
| requests_per_instance.3 | 14001 |
| served_per_instance.1 | 14336 |
| served_per_instance.2 | 14368 |
| served_per_instance.3 | 14314 |
| instances_used | 3 |
| users | 7789 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 4260 |
| busiest_user.instances | [1,2,3] |

## Baseline comparison

| field | value |
|---|---|
| run_id | 20261010-145931-s7-long-1 |
| source | thresholds.json |
| schedule_sha256 | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| same_schedule | true |
| problem | null |

### baseline.steps

| step | p99_ns | baseline_p99_ns | p99_delta_ns | unexpected_rate | baseline_unexpected_rate | unexpected_rate_delta | baseline_ran | baseline_step_exists | baseline_generator_limited |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 486609042 | 9730167 | 476878875 | 0 | 0 | 0 | full | true | false |
| 1 | 507379042 | 11156750 | 496222292 | 0 | 0 | 0 | full | true | false |
| 2 | 491985125 | 9438416 | 482546709 | 0 | 0 | 0 | full | true | false |
| 3 | 522513167 | 11239625 | 511273542 | 0 | 0 | 0 | full | true | false |
| 4 | 496134833 | 9827209 | 486307624 | 0 | 0 | 0 | full | true | false |
| 5 | 504785792 | 8398959 | 496386833 | 0 | 0 | 0 | full | true | false |
| 6 | 1105214584 | 12404416 | 1092810168 | 0 | 0 | 0 | full | true | false |
| 7 | 539300416 | 9724542 | 529575874 | 0 | 0 | 0 | full | true | false |
| 8 | 434179041 | 9688250 | 424490791 | 0 | 0 | 0 | full | true | false |
| 9 | 520337625 | 8383792 | 511953833 | 0 | 0 | 0 | full | true | false |
| 10 | 493745708 | 8969208 | 484776500 | 0 | 0 | 0 | full | true | false |
| 11 | 511068291 | 8041500 | 503026791 | 0 | 0 | 0 | full | true | false |

## Correctness (BUD-11, BUD-11a)

| field | value |
|---|---|
| verdict | pass |
| known_credits | 833 |
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
| fixtures_off | true | {"applies":true,"health_reads":1800,"fixtures_on":0,"clock_not_store":0,"faults":0} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 0 | store outage segments armed: faultctl store down or pause, and timed store refusals |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":true,"windows":4} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | true | {"verdict":"pass","invalid_reasons":[]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
