# Run report 20261010-153749-s6-state-open-1

## Context (PRF-5)

| field | value |
|---|---|
| run_id | 20261010-153749-s6-state-open-1 |
| epoch_id | 197f745d95f5273798769648bbf667c6 |
| run_kind | test |
| run_label | test-build scenario, not a benchmark |
| baseline | false |
| outcome | ok |
| start_ns | 1791646669735054000 |
| end_ns | 1791646825343477000 |
| scenario.name | s6-state-open |
| scenario.path | scenarios/s6-state-open.toml |
| scenario.sha256 | e9a48fbae438ccbe930ee143ead7c18347c6de5346216c498f1f2bd1bcc6d46d |
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
| store_settings.aclfile | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-153749-s6-state-open-1/store/acl.conf |
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
| schedule_sha256 | b26b7a5c7c20688710a7354ed3976e7bdb8a16a0ca6fce6d837490fe5f1a6a45 |
| thresholds_sha256 | null |
| rewrites | [] |
| label | test-build scenario, not a benchmark |
| policy.capacity | 20 |
| policy.cleanup_interval_ns | 5000000000 |
| policy.clock_source | store |
| policy.failure_mode | open |
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
| api | postgres://rl@127.0.0.1:5433/hits?statement_timeout=2500&pool_max_conns=64 | true | 512 | 300 | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null |
| instance | null | null | null | null | 1000000000 | 1152 | 1152 | 5000000000 | 128 | null | null | null | null | null | null | null | null | null | null | null | null | null |
| policy | null | null | null | null | null | null | null | null | null | 20 | 5000000000 | store | open | 50000 | 1000 | 100000 | 5000000000 | 1000000000 | 10 | 60000000000 | 30000000000 | null |
| store | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | null | 127.0.0.1:6390 |

### context.faults

| at_s | cmd | instance | start_ns | end_ns | outage_start_ns | outage_ns | targets |
|---|---|---|---|---|---|---|---|
| 40 | faultctl tamper del-k --key u000001 | null | 1791646710244331000 | 1791646710279415000 | null | null | [] |
| 45 | faultctl tamper corrupt-field --key u000002 --field u --value tampered | null | 1791646715244760000 | 1791646715267158000 | null | null | [] |
| 50 | faultctl tamper del-ix --key u000003 | null | 1791646720245590000 | 1791646720277221000 | null | null | [] |
| 70 | {"target":"store","mode":"refuse","match":"acquire","duration_ms":5000} | 2 | 1791646740242840000 | 1791646740244265000 | null | null | [{"instance":2,"status":200}] |
| 110 | faultctl tamper del-hwm | null | 1791646780243570000 | 1791646781411689000 | null | null | [] |

### context.children

| name | role | pid | exit | exited_before_stop | grace_ns | killed | instance |
|---|---|---|---|---|---|---|---|
| hitgen | hitgen | 60786 | exit 0 | true | 35000000000 | false | null |
| hitapi-1 | hitapi | 60781 | exit 0 | false | 45000000000 | false | 1 |
| hitapi-2 | hitapi | 60782 | exit 0 | false | 45000000000 | false | 2 |
| hitapi-3 | hitapi | 60783 | exit 0 | false | 45000000000 | false | 3 |
| sampler | sampler | 60785 | exit 0 | false | 10000000000 | false | null |

### context.workload

| field | value |
|---|---|
| scenario_file | scenarios/s6-state-open.toml |
| step_bounds_ns | [0,120000000000] |
| users | 100 |
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
| total.retries_429 | 18699 |
| total.unsent | 0 |
| total.sched_lag_ns.count | 36066 |
| total.sched_lag_ns.p50 | 575508 |
| total.sched_lag_ns.p95 | 1171458 |
| total.sched_lag_ns.p99 | 2416789 |
| total.got_conn_wait_ns_p99 | 49542 |
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
| 0 | 300 | 120 | 0 | 120000000000 | full | 36066 | 36066 | 36066 | 36066 | 0 | 18699 | 0 | 300.55 | 300.55 | 36066 | 575508 | 1171458 | 2416789 | 49542 | false | 19989 | 26563 | 0 | 0 |

## Results (PRF-2)

| field | value |
|---|---|
| requests | 54765 |
| by_err.ERR-1 | 19443 |
| by_err.ERR-2 | 0 |
| by_err.ERR-3 | 546 |
| by_err.ERR-4 | 26563 |
| by_err.ERR-5 | 0 |
| by_err.ERR-6 | 0 |
| by_err.ERR-7 | 0 |
| by_err.ERR-8 | 0 |
| by_err.ERR-9 | 0 |
| by_err.ERR-9a | 0 |
| by_err.ERR-10 | 0 |
| by_err.ERR-11 | 7597 |
| by_err.ERR-12 | 0 |
| by_err.ERR-12a | 0 |
| by_err.ERR-12b | 0 |
| by_err.ERR-12c | 0 |
| by_err.ERR-12d | 0 |
| by_err.ERR-13 | 0 |
| by_err.ERR-14 | 0 |
| by_err.ERR-15 | 616 |
| by_err.ERR-16 | 0 |
| unmatched | {} |
| transport_errors.total | 0 |
| transport_errors.by_kind | {} |
| decisions.allowed | 20041 |
| decisions.degraded | 564 |
| releases | 616 |
| refunds.applied | 594 |
| refunds.already_applied | 0 |
| refunds.receipt_expired | 0 |
| refunds.receipt_invalid | 0 |
| refunds.unresolved | 0 |
| unresolved_reasons | {} |
| db_rows | 19989 |
| prf2.new_rows | 19989 |
| prf2.duplicate_rows_avoided | 0 |
| prf2.degraded_admissions | 546 |
| prf2.quota_rejections | 26563 |
| prf2.capacity_refusals | 0 |
| prf2.integrity_failures | 7597 |
| prf2.unavailable | 0 |
| prf2.unknown_acquires | 0 |
| prf2.unknown_application_outcomes | 0 |
| prf2.server_failures | 616 |
| prf2.transport_errors | 0 |
| prf2.expired_refunds | 0 |
| prf2.unresolved_refunds | 0 |
| prf2.db_rows | 19989 |

### results.attempt_outcomes

| name | 200 | 429 rate_limited | 503 limiter_state_integrity_error | 503 service_aborted_after_acquire |
|---|---|---|---|---|
| initial | 18210 | 9958 | 7331 | 567 |
| duplicate | null | null | null | null |
| retry | 1779 | 16605 | 266 | 49 |

### results.unexpected_errors

| field | value |
|---|---|
| rule | attempt-1 requests (§B9): a transport error or timeout (no HTTP response), or a 5xx other than service_aborted_after_acquire whose wall-clock span, from its write to its response, overlaps no fault window [start_ns, end_ns + P]; steps count their observation window: the step minus its first 5 s, by planned_ns |
| total.attempts | 36066 |
| total.unexpected | 4579 |
| total.rate | 0.126962 |
| total.transport_errors | 0 |
| total.server_errors | 7898 |
| total.excluded_service_aborted_after_acquire | 567 |
| total.excluded_fault_window | 2752 |

#### results.unexpected_errors.fault_windows

| start_ns | end_ns | source |
|---|---|---|
| 1791646710244331000 | 1791646715279415000 | faultctl tamper del-k --key u000001 |
| 1791646715244760000 | 1791646720267158000 | faultctl tamper corrupt-field --key u000002 --field u --value tampered |
| 1791646720245590000 | 1791646725277221000 | faultctl tamper del-ix --key u000003 |
| 1791646740242840000 | 1791646745244265000 | {"target":"store","mode":"refuse","match":"acquire","duration_ms":5000} |
| 1791646780243570000 | 1791646786411689000 | faultctl tamper del-hwm |

#### results.unexpected_errors.steps

| step | window_start_ns | window_end_ns | attempts | unexpected | rate | transport_errors | server_errors | excluded_service_aborted_after_acquire | excluded_fault_window |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 5000000000 | 120000000000 | 34572 | 4579 | 0.132448 | 0 | 7869 | 538 | 2752 |

### results.steps

| step | requests | new_rows | allowed | limited |
|---|---|---|---|---|
| 0 | 54765 | 19989 | 20041 | 26563 |

## Timing (PRF-3)

| field | value |
|---|---|
| db_ns.count | 19989 |
| db_ns.p50 | 670666 |
| db_ns.p95 | 5476959 |
| db_ns.p99 | 11227000 |
| api_total_ns.count | 54765 |
| api_total_ns.p50 | 10017167 |
| api_total_ns.p95 | 371742209 |
| api_total_ns.p99 | 511163750 |
| rewrite_windows | [] |
| rewrite_p99_ns | null |
| rewrite_latency_ns | null |
| rule | limiter_ns by acquire result (every result other than allowed, limited and degraded is an error); db_ns where the DB work ran (db_ns > 0); refund_ns = final_mono_ns − release_mono_ns; end_to_end_ns from gen.jsonl, attempt 1 with an HTTP response, by status (success 200, limited 429, error other), retries apart; steps: the §B9 latency population over each observation window; rewrite: attempt-1 responses whose wall-clock span overlaps a rewrite window (TRF-S7) |

### timing.limiter_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| allowed | 20041 | 10827292 | 411282084 | 518383208 |
| limited | 26563 | 9561542 | 356857334 | 500783667 |
| degraded | 564 | 7333 | 13917 | 20834 |
| error | 7597 | 3073708 | 350500167 | 503172375 |

### timing.refund_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 594 | 13377500 | 538945875 | 552216541 |
| applied | 594 | 13377500 | 538945875 | 552216541 |
| already_applied | 0 | null | null | null |
| receipt_expired | 0 | null | null | null |
| receipt_invalid | 0 | null | null | null |
| unresolved | 0 | null | null | null |

### timing.end_to_end_ns

| name | count | p50 | p95 | p99 |
|---|---|---|---|---|
| all | 36066 | 10549333 | 409788542 | 521506875 |
| success | 18210 | 11839209 | 423041250 | 525446958 |
| limited | 9958 | 9904667 | 417191250 | 517556708 |
| error | 7898 | 5706209 | 364324167 | 507158417 |
| retries | 18699 | 10430667 | 346379708 | 475922750 |

### timing.steps

| step | window_start_ns | window_end_ns | latency_ns.count | latency_ns.p50 | latency_ns.p95 | latency_ns.p99 |
|---|---|---|---|---|---|---|
| 0 | 5000000000 | 120000000000 | 34572 | 10616042 | 416532083 | 522350792 |

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
| t0_wall_ns | 1791646670241739000 |
| t0_rule | hitgen's t0 on the wall clock: the time of its sending log line (hitgen.stderr), written just before it takes t0; it places planned_ns and sent_ns on the wall clock for the fault and rewrite windows and arrival_ns on hitgen's steps |

## Resources (PRF-4)

| field | value |
|---|---|
| samples | 155 |
| health_reads | 360 |
| max_in_flight | 78 |

### resources.processes

| name | cpu_pct_mean | cpu_pct_max | rss_bytes_mean | rss_bytes_max | cpu_readings | rss_readings |
|---|---|---|---|---|---|---|
| hitapi-1 | 5.93 | 10.38 | 40563507 | 42680320 | 155 | 155 |
| hitapi-2 | 7.41 | 11 | 38665147 | 41271296 | 120 | 120 |
| hitapi-3 | 5.9 | 10.92 | 39425611 | 41091072 | 155 | 155 |
| hitgen | 17.23 | 27.01 | 46509357 | 50135040 | 119 | 119 |
| postgres | 3.61 | 8.37 | 749579521 | 971423744 | 155 | 155 |
| sampler | 8.72 | 11.18 | 28426979 | 28934144 | 155 | 155 |
| valkey-server | 8.32 | 16.71 | 10162942 | 10354688 | 155 | 155 |

### resources.store_state

| name | max | last | readings |
|---|---|---|---|
| live_markers | 377 | 332 | 145 |
| quarantined_keys | 3 | 3 | 145 |
| tracked_buckets | 100 | 100 | 145 |

### resources.instance_series

| instance | series | max | last | readings |
|---|---|---|---|---|
| 1 | in_flight | 78 | 0 | 120 |
| 1 | listener_drops | 0 | 0 | 120 |
| 1 | log_drops | 0 | 0 | 120 |
| 1 | rl_cleanup_entries_total{action="moved"} | 61 | 61 | 119 |
| 1 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 119 |
| 1 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 119 |
| 1 | rl_cleanup_entries_total{action="skipped"} | 0 | 0 | 119 |
| 1 | rl_cleanup_lag_seconds | 4.31931 | 0.256007 | 119 |
| 1 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 119 |
| 1 | rl_cleanup_passes_total{status="complete"} | 22 | 22 | 119 |
| 1 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 119 |
| 1 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 119 |
| 1 | rl_cleanup_passes_total{status="state_integrity_error"} | 2 | 2 | 119 |
| 1 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 119 |
| 1 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 119 |
| 1 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 119 |
| 1 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 119 |
| 1 | rl_cleanup_stops_total{result="state_integrity_error"} | 2 | 2 | 119 |
| 1 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 119 |
| 1 | rl_clock_lag_microseconds | 0 | 0 | 119 |
| 1 | rl_listener_drops_total | 0 | 0 | 119 |
| 1 | rl_live_markers | 376 | 332 | 119 |
| 1 | rl_pending_refunds | 2 | 1 | 119 |
| 1 | rl_quarantined_keys | 3 | 3 | 119 |
| 1 | rl_tracked_buckets | 100 | 100 | 119 |
| 2 | in_flight | 75 | 0 | 120 |
| 2 | listener_drops | 0 | 0 | 120 |
| 2 | log_drops | 0 | 0 | 120 |
| 2 | rl_cleanup_entries_total{action="moved"} | 100 | 100 | 120 |
| 2 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 120 |
| 2 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 120 |
| 2 | rl_cleanup_entries_total{action="skipped"} | 33 | 33 | 120 |
| 2 | rl_cleanup_lag_seconds | 4.32604 | 3.46037 | 120 |
| 2 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 120 |
| 2 | rl_cleanup_passes_total{status="complete"} | 22 | 22 | 120 |
| 2 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 120 |
| 2 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 120 |
| 2 | rl_cleanup_passes_total{status="state_integrity_error"} | 2 | 2 | 120 |
| 2 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 120 |
| 2 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 120 |
| 2 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 120 |
| 2 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 120 |
| 2 | rl_cleanup_stops_total{result="state_integrity_error"} | 2 | 2 | 120 |
| 2 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 120 |
| 2 | rl_clock_lag_microseconds | 0 | 0 | 120 |
| 2 | rl_listener_drops_total | 0 | 0 | 120 |
| 2 | rl_live_markers | 376 | 332 | 120 |
| 2 | rl_pending_refunds | 1 | 0 | 120 |
| 2 | rl_quarantined_keys | 3 | 3 | 120 |
| 2 | rl_tracked_buckets | 100 | 100 | 120 |
| 3 | in_flight | 74 | 0 | 120 |
| 3 | listener_drops | 0 | 0 | 120 |
| 3 | log_drops | 0 | 0 | 120 |
| 3 | rl_cleanup_entries_total{action="moved"} | 44 | 44 | 120 |
| 3 | rl_cleanup_entries_total{action="quarantined"} | 0 | 0 | 120 |
| 3 | rl_cleanup_entries_total{action="removed"} | 0 | 0 | 120 |
| 3 | rl_cleanup_entries_total{action="skipped"} | 16 | 16 | 120 |
| 3 | rl_cleanup_lag_seconds | 5.56085 | 0.78053 | 120 |
| 3 | rl_cleanup_passes_total{status="clock_error"} | 0 | 0 | 120 |
| 3 | rl_cleanup_passes_total{status="complete"} | 22 | 22 | 120 |
| 3 | rl_cleanup_passes_total{status="config_error"} | 0 | 0 | 120 |
| 3 | rl_cleanup_passes_total{status="outcome_unknown"} | 0 | 0 | 120 |
| 3 | rl_cleanup_passes_total{status="state_integrity_error"} | 2 | 2 | 120 |
| 3 | rl_cleanup_passes_total{status="unavailable"} | 0 | 0 | 120 |
| 3 | rl_cleanup_stops_total{result="clock_error"} | 0 | 0 | 120 |
| 3 | rl_cleanup_stops_total{result="config_error"} | 0 | 0 | 120 |
| 3 | rl_cleanup_stops_total{result="outcome_unknown"} | 0 | 0 | 120 |
| 3 | rl_cleanup_stops_total{result="state_integrity_error"} | 2 | 2 | 120 |
| 3 | rl_cleanup_stops_total{result="unavailable"} | 0 | 0 | 120 |
| 3 | rl_clock_lag_microseconds | 0 | 0 | 120 |
| 3 | rl_listener_drops_total | 0 | 0 | 120 |
| 3 | rl_live_markers | 376 | 332 | 120 |
| 3 | rl_pending_refunds | 3 | 3 | 120 |
| 3 | rl_quarantined_keys | 3 | 3 | 120 |
| 3 | rl_tracked_buckets | 100 | 100 | 120 |

### resources.pids

| role | pid | instance |
|---|---|---|
| valkey-server | 60776 | null |
| hitapi | 60781 | 1 |
| hitapi | 60782 | 2 |
| hitapi | 60783 | 3 |
| sampler | 60785 | null |
| hitgen | 60786 | null |

## Traffic (TRF-1, TRF-4)

| field | value |
|---|---|
| schedule_path | /Users/srirondla/distributed-systems/ratelimiter/runs/20261010-153749-s6-state-open-1/schedule.jsonl |
| schedule_sha256 | b26b7a5c7c20688710a7354ed3976e7bdb8a16a0ca6fce6d837490fe5f1a6a45 |
| schedule_sha256_context | b26b7a5c7c20688710a7354ed3976e7bdb8a16a0ca6fce6d837490fe5f1a6a45 |
| schedule_sha256_matches | true |
| schedule_entries | 36066 |
| planned_per_step | [36066] |
| instances | 3 |
| requests_per_instance.1 | 12022 |
| requests_per_instance.2 | 12022 |
| requests_per_instance.3 | 12022 |
| served_per_instance.1 | 18535 |
| served_per_instance.2 | 18040 |
| served_per_instance.3 | 18190 |
| instances_used | 3 |
| users | 100 |
| busiest_user.user | u000000 |
| busiest_user.first_attempts | 8317 |
| busiest_user.instances | [1,2,3] |

## Baseline comparison

Not applicable to this run, or not built yet.

## Correctness (BUD-11, BUD-11a)

| field | value |
|---|---|
| verdict | invalid |
| known_credits | 594 |
| unknown_credits | 0 |
| listener_drops | 4 |
| pre_kill_lines | 0 |
| pre_kill_read_ok_false | 0 |
| invalid_reasons | ["api-1.jsonl: line 24624: release without final event or segment pre_kill","api-3.jsonl: line 24459: release without final event or segment pre_kill","api-3.jsonl: line 24475: release without final event or segment pre_kill","api-3.jsonl: line 24466: release without final event or segment pre_kill","listener_drops"] |
| violation | null |

## Capacity (T-RPT-2)

Not applicable to this run, or not built yet.

## Evidence validity (§B8)

| field | value |
|---|---|
| valid | false |
| validate.passed | false |
| validate.failed_checks | ["evidence_validity.listener_drops","evidence_validity.correctness","evidence_validity.valid"] |
| validate.output | validate.stderr |

### evidence_validity.checks

| name | ok | value | detail |
|---|---|---|---|
| log_drops | true | 0 | log_drops summed over every stop and pre_kill line of every api-N.jsonl |
| listener_drops | false | {"sum":4,"stop_lines":3,"pre_kill_lines":0,"pre_kill_unread":0,"segments_without_count":0} | HAR-13: listener_drops summed over every stop and pre_kill line is 0, every pre_kill read succeeded (read_ok), and every process ended with one of the two |
| fixtures_off | true | {"applies":false,"health_reads":360,"fixtures_on":360,"clock_not_store":0,"faults":5} | benchmark runs: every /health read shows fixtures false on the store clock, at least one was read, and no fault ran |
| outage | true | {"max_outage_ns":null,"unmeasured":0} | every store outage measured (context.json outage_ns of each faultctl store down or pause that ran) and at most 5 s |
| outage_segments | true | 1 | store outage segments armed: faultctl store down or pause, and timed store refusals |
| gen_summary | true | 0 | gen.jsonl ends with its summary line, whose log_drops is 0 |
| rewrite_windows | true | {"applies":false,"windows":0} | a scenario with forced_aof_rewrites_s observed at least one AOF rewrite window |
| correctness | false | {"verdict":"invalid","invalid_reasons":["api-1.jsonl: line 24624: release without final event or segment pre_kill","api-3.jsonl: line 24459: release without final event or segment pre_kill","api-3.jsonl: line 24475: release without final event or segment pre_kill","api-3.jsonl: line 24466: release without final event or segment pre_kill","listener_drops"]} | HAR-13: refmodel.check_bud11's BUD-11a verdict (correctness) is pass or violation, not invalid: an invalid verdict means the run's refund evidence is incomplete (invalid_reasons say why; rerun) |
