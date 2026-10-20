# CAS idle-VM detection (`cmx instance-idle-list`)

Read-only scoring of CAS VMs into 闲置 / 定时使用 / 间歇使用 / 活跃.

## Score columns

| Field | Meaning |
|---|---|
| `idle_score` (SCORE) | Headline score in the table. Shown only for 闲置 / 闲置(关机). Other classes set `score_hidden=true` and render as `-` so a raw ~98 never sits next to 间歇使用. Rule: `IdleClassMinScore` in `score.go`. |
| `raw_idle_score` | Always the weighted 0–100 computation, regardless of class. **JSON only** (not a table column). |

间歇使用 suggestion is always `保留/核实` (print-only) — never downsize or release.

## Peak gates (R1)

Strict 闲置 needs R_total ≤ 2%, no day with >2 active hours, **and** every window peak below its gate. Any one hour at/above a gate → 间歇使用 at most. Same units as the other thresholds (net/disk in KB/s).

| Config field | cmx flag | Default |
|---|---|---|
| `IdlePeakCPU` | `--idle-peak-cpu` | 20 % |
| `IdlePeakNetKBps` | `--idle-peak-net` | 1024 KB/s (1 MB/s) |
| `IdlePeakDiskKBps` | `--idle-peak-disk` | 5120 KB/s (5 MB/s) |
| `SpikeCPU` / `SpikeNetKBps` / `SpikeDiskKBps` | `--spike-cpu/-net/-disk` | 50 % / 1 MB/s / 5 MB/s — same clock hour on ≥3 days → VETO |

The BURST reason names the dominant signal (largest peak/gate ratio): `每周日 02-04 负载 90%`, `每周日 02-04 网络 5 MB/s`, `每周日 02-04 磁盘 20 MB/s`, `每天 03-04 负载 60%` (≥5 days on ≥5 weekdays), `工作日 19-23 …` (Mon–Fri only), `每月一次 6 小时突发 峰值 95%`, `单次 2 小时突发 峰值 网络 5 MB/s`.

## Release gate (R2)

`release/回收` is printed only when confidence is **高** and either:

- shut off with a real `lastOffTime` age ≥ `--off-days`, or
- running with **full signal coverage** (`hasFullSignalCoverage` in `score.go`): cpu, net, disk and iops all enabled and present, `dropped_buckets=0`, hourly coverage ≥ `ReleaseMinCoverage` (0.95).

Otherwise an idle class prints `核实` plus a `RELEASE_GATE:` reason. zombieVm-list-only VMs (closeTimeout unit unverified) and CPU-only VMs always get `核实`.

## data_gaps

Missing series (`cpu/net/disk/iops=n/a`), `dropped_buckets=N`, `lastOffTime=n/a`, the uncollected stubs `lifecycle=n/a` and `proc=n/a` (lifecycle multiplies confidence by `LifecycleStubFactor`, 0.95), and `holiday_file=unreadable|invalid` when `--holiday-file` cannot be used (also logged).

## D9 — peaks from a single report rate field

Hourly CPU/net/disk points come from `reports/vm/*` `EChartValue.rate`. Whether that value is the hourly **average** or hourly **max** is **UNVERIFIED**. `CpuMax`, burst 峰值, and p95 are therefore peaks of that single series, not a confirmed ClickHouse `maxMerge`. Calibrate on a live CAS before treating them as true maxima.

## Live-CAS checklist

1. Units of `vm/{uuid}/monitor` and `reports/vm/{cpuMem,netSp,disk,io}` rates.
2. **D9:** is report `rate` avg or max? Compare a known burst hour with the CAS UI curve.
3. Bucket label format / timezone (count `dropped_buckets`).
4. `flowLink` semantics before enabling the conn signal.
5. `lastOffTime` on `summary/new` / vmList for shut-off VMs; `zombieVm` `closeTimeout` unit.
6. Zero-fill vs omission for shut-off periods in reports.
7. Wrong-password account create must fail (`NewCasClient` Probe).

## Offline fixture

```bash
cmx instance-idle-list --fixture pkg/multicloud/cas/idle/testdata/idle_demo.json
# Prefer env CLOUDMUX_SECRET for live runs; fixture needs no credentials.
```

Offline mode is chosen from the parsed `instance-idle-list --fixture` flag only (`WantsOfflineFixture`), not from any argv token. The demo includes `net-backup` (Sun 02-04 net 5 MB/s) and `disk-backup` (Sun 02-04 disk 20 MB/s), both 间歇使用.
