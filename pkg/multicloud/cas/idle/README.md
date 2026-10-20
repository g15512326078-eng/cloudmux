# CAS idle-VM detection (`cmx instance-idle-list`)

Read-only scoring of CAS VMs into 闲置 / 定时使用 / 间歇使用 / 活跃.

## Score columns

| Field | Meaning |
|---|---|
| `idle_score` (SCORE) | Headline score. Shown only for 闲置 / 闲置(关机). Other classes set `score_hidden=true` and render as `-` so a raw ~98 never sits next to 间歇使用. Rule: `IdleClassMinScore` in `score.go`. |
| `raw_idle_score` (RAW) | Always the weighted 0–100 computation, regardless of class. Present in **both** table (RAW column) and JSON. |

间歇使用 suggestion is always `保留/核实` (print-only) — never downsize or release.

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
