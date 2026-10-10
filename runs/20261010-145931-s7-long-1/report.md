# Run report 20261010-145931-s7-long-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261010-145931-s7-long-1 |
| epoch_id | a2c99c68a534618db95a38f152e31ef3 |
| run_kind | bench |
| run_label | benchmark |
| baseline | true |
| outcome | ok |
| start_ns | 1791644371575315000 |
| end_ns | 1791644972183105000 |
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
| store_settings.aclfile | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-145931-s7-long-1/store/acl.conf |
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
| thresholds_sha256 | null |
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
| limiter_enabled | false |
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
| api | postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64 | false | 512 | 200 | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null |
| instance | null | null | null | null | 1000000000 | 1152 | 1152 | 5000000000 | 128 | null | null | null | null | null | null | null | null | null | null | null | null | null |
| policy | null | null | null | null | null | null | null | null | null | 20 | 5000000000 | store | closed | 50000 | 1000 | 100000 | 5000000000 | 1000000000 | 10 | 60000000000 | 30000000000 | null |
| store | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | 127.0.0.1:6390 |

### context.rewrites

| at_s | start_ns | end_ns | status |
|---|---|---|---|
| 180 | 1791644552105857000 | 1791644552228378000 | ok |
| 420 | 1791644792109576000 | 1791644792228504000 | ok |

### context.children

| name | role | pid | exit | exited_before_stop | grace_ns | killed | instance |
|---|---|---|---|---|---|---|---|
| hitgen | hitgen | 53619 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 53615 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 53616 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 53617 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 53618 | exit 0 | false | 10000000000 | false | null |

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
| total.retries_429 | 0 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 42004 |
| total.sched_lag_ns.p50 | 605051 |
| total.sched_lag_ns.p95 | 1115079 |
| total.sched_lag_ns.p99 | 1982742 |
| total.got_conn_wait_ns_p99 | 49791 |
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
| 0 | 70 | 50 | 0 | 50000000000 | full | 3485 | 3485 | 3485 | 3485 | 0 | 0 | 0 | 69.7 | 69.7 | 3485 | 603253 | 1098045 | 1597727 | 51500 | false | 3412 | 0 | 0 | 0 |
| 1 | 70 | 50 | 50000000000 | 100000000000 | full | 3532 | 3532 | 3532 | 3532 | 0 | 0 | 0 | 70.64 | 70.64 | 3532 | 600869 | 1095609 | 1537478 | 47625 | false | 3472 | 0 | 0 | 0 |
| 2 | 70 | 50 | 100000000000 | 150000000000 | full | 3458 | 3458 | 3458 | 3458 | 0 | 0 | 0 | 69.16 | 69.16 | 3458 | 580703 | 1087154 | 1439740 | 43167 | false | 3395 | 0 | 0 | 0 |
| 3 | 70 | 50 | 150000000000 | 200000000000 | full | 3448 | 3448 | 3448 | 3448 | 0 | 0 | 0 | 68.96 | 68.96 | 3448 | 604532 | 1125870 | 2961892 | 56708 | false | 3363 | 0 | 0 | 0 |
| 4 | 70 | 50 | 200000000000 | 250000000000 | full | 3492 | 3492 | 3492 | 3492 | 0 | 0 | 0 | 69.84 | 69.84 | 3492 | 598523 | 1090481 | 1512588 | 51125 | false | 3421 | 0 | 0 | 0 |
| 5 | 70 | 50 | 250000000000 | 300000000000 | full | 3548 | 3548 | 3548 | 3548 | 0 | 0 | 0 | 70.96 | 70.96 | 3548 | 602706 | 1105186 | 1556004 | 47000 | false | 3462 | 0 | 0 | 0 |
| 6 | 70 | 50 | 300000000000 | 350000000000 | full | 3539 | 3539 | 3539 | 3539 | 0 | 0 | 0 | 70.78 | 70.78 | 3539 | 616643 | 1125095 | 2254808 | 47750 | false | 3479 | 0 | 0 | 0 |
| 7 | 70 | 50 | 350000000000 | 400000000000 | full | 3514 | 3514 | 3514 | 3514 | 0 | 0 | 0 | 70.28 | 70.28 | 3514 | 610897 | 1144893 | 2126869 | 56667 | false | 3450 | 0 | 0 | 0 |
| 8 | 70 | 50 | 400000000000 | 450000000000 | full | 3450 | 3450 | 3450 | 3450 | 0 | 0 | 0 | 69 | 69 | 3450 | 592564 | 1099726 | 1790408 | 34250 | false | 3382 | 0 | 0 | 0 |
| 9 | 70 | 50 | 450000000000 | 500000000000 | full | 3623 | 3623 | 3623 | 3623 | 0 | 0 | 0 | 72.46 | 72.46 | 3623 | 621612 | 1212114 | 2695916 | 53917 | false | 3548 | 0 | 0 | 0 |
| 10 | 70 | 50 | 500000000000 | 550000000000 | full | 3453 | 3453 | 3453 | 3453 | 0 | 0 | 0 | 69.06 | 69.06 | 3453 | 612408 | 1112302 | 2139822 | 39209 | false | 3390 | 0 | 0 | 0 |
| 11 | 70 | 50 | 550000000000 | 600000000000 | full | 3462 | 3462 | 3462 | 3462 | 0 | 0 | 0 | 69.24 | 69.24 | 3462 | 614595 | 1152747 | 2272344 | 43292 | false | 3396 | 0 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 42004 |
| by_err.ERR-1 | 41170 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 0 |
| by_err.ERR-4 | 0 |
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
| by_err.ERR-15 | 834 |
| by_err.ERR-16 | 0 |
| unmatched | {} |
| transport_errors.total | 0 |
| transport_errors.by_kind | {} |
| decisions.allowed | 0 |
| decisions.degraded | 0 |
| releases | 0 |
| refunds.applied | 0 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 41170 |
| prf2.new_rows | 41170 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 0 |
| prf2.quota_rejections | 0 |
| prf2.capacity_refusals | 0 |
| prf2.integrity_failures | 0 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 834 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 41170 |

### results.attempt_outcomes

| name | 200 | 503 service_aborted_after_acquire |
|---|---|---|
| initial | 41170 | 834 |
| duplicate | null | null |
| retry | null | null |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| fault_windows | [] |
| total.attempts | 42004 |
| total.unexpected | 0 |
| total.rate | 0 |
| total.transport_errors | 0 |
| total.server_errors | 834 |
| total.excluded_service_aborted_after_acquire | 834 |
| total.excluded_fault_window | 0 |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 50000000000 | 3138 | 0 | 0 | 0 | 71 | 71 | 0 |
| 1 | 55000000000 | 100000000000 | 3173 | 0 | 0 | 0 | 51 | 51 | 0 |
| 2 | 105000000000 | 150000000000 | 3079 | 0 | 0 | 0 | 56 | 56 | 0 |
| 3 | 155000000000 | 200000000000 | 3102 | 0 | 0 | 0 | 80 | 80 | 0 |
| 4 | 205000000000 | 250000000000 | 3116 | 0 | 0 | 0 | 65 | 65 | 0 |
| 5 | 255000000000 | 300000000000 | 3163 | 0 | 0 | 0 | 78 | 78 | 0 |
| 6 | 305000000000 | 350000000000 | 3175 | 0 | 0 | 0 | 58 | 58 | 0 |
| 7 | 355000000000 | 400000000000 | 3161 | 0 | 0 | 0 | 57 | 57 | 0 |
| 8 | 405000000000 | 450000000000 | 3119 | 0 | 0 | 0 | 60 | 60 | 0 |
| 9 | 455000000000 | 500000000000 | 3236 | 0 | 0 | 0 | 67 | 67 | 0 |
| 10 | 505000000000 | 550000000000 | 3087 | 0 | 0 | 0 | 55 | 55 | 0 |
| 11 | 555000000000 | 600000000000 | 3125 | 0 | 0 | 0 | 60 | 60 | 0 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 3485 | 3412 | 0 | 0 |
| 1 | 3532 | 3472 | 0 | 0 |
| 2 | 3458 | 3395 | 0 | 0 |
| 3 | 3448 | 3363 | 0 | 0 |
| 4 | 3491 | 3420 | 0 | 0 |
| 5 | 3549 | 3463 | 0 | 0 |
| 6 | 3539 | 3479 | 0 | 0 |
| 7 | 3513 | 3449 | 0 | 0 |
| 8 | 3450 | 3382 | 0 | 0 |
| 9 | 3624 | 3549 | 0 | 0 |
| 10 | 3453 | 3390 | 0 | 0 |
| 11 | 3462 | 3396 | 0 | 0 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 41170 |
| db_ns.p50 | 805833 |
| db_ns.p95 | 1643209 |
| db_ns.p99 | 3037542 |
| api_total_ns.count | 42004 |
| api_total_ns.p50 | 822083 |
| api_total_ns.p95 | 1670916 |
| api_total_ns.p99 | 3058333 |
| rewrite_p99_ns | 2688667 |
| rewrite_latency_ns.count | 15 |
| rewrite_latency_ns.p50 | 417542 |
| rewrite_latency_ns.p95 | 2688667 |
| rewrite_latency_ns.p99 | 2688667 |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 0 | null | null | null |
| limited | 0 | null | null | null |
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
| all | 42004 | 1324625 | 2836292 | 10020291 |
| success | 41170 | 1344875 | 2845500 | 10050375 |
| limited | 0 | null | null | null |
| error | 834 | 539792 | 1406458 | 7486333 |
| retries | 0 | null | null | null |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 50000000000 | 3138 | 1324708 | 2549459 | 9730167 |
| 1 | 55000000000 | 100000000000 | 3173 | 1410208 | 2659500 | 11156750 |
| 2 | 105000000000 | 150000000000 | 3079 | 1389292 | 2620208 | 9438416 |
| 3 | 155000000000 | 200000000000 | 3102 | 1317708 | 2728833 | 11239625 |
| 4 | 205000000000 | 250000000000 | 3116 | 1413334 | 2588833 | 9827209 |
| 5 | 255000000000 | 300000000000 | 3163 | 1409167 | 2550792 | 8398959 |
| 6 | 305000000000 | 350000000000 | 3175 | 1292250 | 3150875 | 12404416 |
| 7 | 355000000000 | 400000000000 | 3161 | 1396166 | 3489125 | 9724542 |
| 8 | 405000000000 | 450000000000 | 3119 | 1253459 | 2927000 | 9688250 |
| 9 | 455000000000 | 500000000000 | 3236 | 1216958 | 3088958 | 8383792 |
| 10 | 505000000000 | 550000000000 | 3087 | 1304208 | 2787125 | 8969208 |
| 11 | 555000000000 | 600000000000 | 3125 | 1171625 | 3268208 | 8041500 |

### timing.rewrite_windows

| start_ns | end_ns | source |
|---|---|---|
| 1791644552105857000 | 1791644552228378000 | context |
| 1791644792109576000 | 1791644792228504000 | context |
| 1791644552115107000 | 1791644552115107000 | samples |
| 1791644792117606000 | 1791644792117606000 | samples |

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
| t0_wall_ns | 1791644372101153000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 600 |
| health_reads | 1797 |
| max_in_flight | 2 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 2.26 | 3.61 | 31065759 | 31522816 | 599 | 599 |
| hitapi-2 | 2.23 | 3.53 | 31510370 | 32112640 | 599 | 599 |
| hitapi-3 | 2.24 | 3.93 | 31160672 | 32063488 | 599 | 599 |
| hitgen | 10.86 | 14.53 | 34004868 | 34373632 | 599 | 599 |
| postgres | 3.1 | 4.76 | 240425233 | 292241408 | 600 | 600 |
| sampler | 9.29 | 11.65 | 29950525 | 31244288 | 600 | 600 |
| valkey-server | 0.38 | 0.93 | 9014586 | 9060352 | 600 | 600 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 0 | 0 | 599 |
| quarantined_keys | 0 | 0 | 599 |
| tracked_buckets | 0 | 0 | 599 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 1 | 0 | 599 |
| 1 | listener_drops | 0 | 0 | 599 |
| 1 | log_drops | 0 | 0 | 599 |
| 1 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 599 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 599 |
| 1 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 599 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 599 |
| 1 | rl_cleanup_lag_seconds | 0 | 0 | 599 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_passes_total{status="complete"} | 126 | 126 | 599 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 599 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 599 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 599 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 599 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 599 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 599 |
| 1 | rl_listener_drops_total | 0 | 0 | 599 |
| 1 | rl_live_markers | 0 | 0 | 599 |
| 1 | rl_pending_refunds | 0 | 0 | 599 |
| 1 | rl_quarantined_keys | 0 | 0 | 599 |
| 1 | rl_tracked_buckets | 0 | 0 | 599 |
| 2 | in_flight | 1 | 0 | 599 |
| 2 | listener_drops | 0 | 0 | 599 |
| 2 | log_drops | 0 | 0 | 599 |
| 2 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 599 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 599 |
| 2 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 599 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 599 |
| 2 | rl_cleanup_lag_seconds | 0 | 0 | 599 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_passes_total{status="complete"} | 125 | 125 | 599 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 599 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 599 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 599 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 599 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 599 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 599 |
| 2 | rl_listener_drops_total | 0 | 0 | 599 |
| 2 | rl_live_markers | 0 | 0 | 599 |
| 2 | rl_pending_refunds | 0 | 0 | 599 |
| 2 | rl_quarantined_keys | 0 | 0 | 599 |
| 2 | rl_tracked_buckets | 0 | 0 | 599 |
| 3 | in_flight | 2 | 0 | 599 |
| 3 | listener_drops | 0 | 0 | 599 |
| 3 | log_drops | 0 | 0 | 599 |
| 3 | rl_cleanup_entries_total{action="moved"} | 0 | 0 | 599 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 599 |
| 3 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 599 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 599 |
| 3 | rl_cleanup_lag_seconds | 0 | 0 | 599 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_passes_total{status="complete"} | 126 | 126 | 599 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 599 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 599 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 599 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 0 | 0 | 599 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 599 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 599 |
| 3 | rl_listener_drops_total | 0 | 0 | 599 |
| 3 | rl_live_markers | 0 | 0 | 599 |
| 3 | rl_pending_refunds | 0 | 0 | 599 |
| 3 | rl_quarantined_keys | 0 | 0 | 599 |
| 3 | rl_tracked_buckets | 0 | 0 | 599 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 53610 | null |
| hitapi | 53615 | 1 |
| hitapi | 53616 | 2 |
| hitapi | 53617 | 3 |
| sampler | 53618 | null |
| hitgen | 53619 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-145931-s7-long-1/schedule.jsonl |
| schedule_sha256 | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| schedule_sha256_context | 61911ed137d3d97ce388fd9dfbfb8fd09c21dc52c8dc967c3ac5c36d449526f9 |
| schedule_sha256_matches | true |
| schedule_entries | 42004 |
| planned_per_step | [3485,3532,3458,3448,3492,3548,3539,3514,3450,3623,3453,3462] |
| instances | 3 |
| requests_per_instance.1 | 14002 |
| requests_per_instance.2 | 14001 |
| requests_per_instance.3 | 14001 |
| served_per_instance.1 | 14002 |
| served_per_instance.2 | 14001 |
| served_per_instance.3 | 14001 |
| instances_used | 3 |
| users | 7789 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 4260 |
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
| listener_drops | true | {"sum":0,"stop_lines":3,"pre_kill_lines":0,"pre_kill_unread":0,"segments_without_count":0} | HAR-13: listener_drops summed over every stop and pre_kill line is 0, every pre_kill read succeeded (read_ok), and every process ended with one of the two |
| fixtures_off | true | {"applies":true,"health_reads":1797,"fixtures_on":0,"clock_not_store":0,"faults":0} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 0 | store outage segments armed: faultctl store down or pause, and timed store refusals |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":true,"windows":4} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | true | {"verdict":"pass","invalid_reasons":[]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
