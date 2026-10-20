// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package idle

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// IdleClassMinScore is the §6.3 idle threshold (idle_score ≥ 80 → 闲置).
//
// HeadlineIdleScoreRule (CoS): IdleScore is the user-facing headline;
// RawIdleScore always keeps the weighted computation.
//   - 闲置 / 闲置(关机): IdleScore = RawIdleScore (ScoreHidden=false)
//   - 间歇使用 / 定时使用 / 活跃 / 跳过 / 数据不足: IdleScore left unused,
//     ScoreHidden=true → table and JSON consumers render SCORE as "-"
//     so a raw 98 never sits next to 间歇使用.
const IdleClassMinScore = 80

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := p / 100 * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func stats(vals []float64) (avg, p95, max float64) {
	if len(vals) == 0 {
		return 0, 0, 0
	}
	sum := 0.0
	cp := append([]float64{}, vals...)
	sort.Float64s(cp)
	for _, v := range cp {
		sum += v
		if v > max {
			max = v
		}
	}
	return sum / float64(len(cp)), percentile(cp, 95), max
}

func subScore(p95, threshold float64) float64 {
	if threshold <= 0 {
		return 0
	}
	return clamp01(1 - p95/threshold)
}

// ScoreAndClassify turns features into a Result. Pure function — no I/O.
func ScoreAndClassify(f VmFeatures, cfg Config, now time.Time, loc *time.Location) Result {
	r := Result{
		Name:    firstNonEmpty(f.Vm.Title, f.Vm.Name),
		Uuid:    f.Vm.Uuid,
		Status:  f.Vm.Status,
		CpuAvg:  f.CpuAvg,
		CpuP95:  f.CpuP95,
		NetP95:  f.NetP95,
		DiskP95: f.DiskP95,
		IopsP95: f.IopsP95,
		ConnMax: f.ConnMax,
	}
	if !f.LastActive.IsZero() {
		r.LastActive = f.LastActive.In(loc).Format("2006-01-02 15:04")
	}

	// TOO_NEW
	if !f.Vm.CreateDate.IsZero() && now.Sub(f.Vm.CreateDate) < cfg.MinAge {
		r.Class = ClassSkipped
		r.Reasons = append(r.Reasons, fmt.Sprintf("TOO_NEW: 创建未满 %s，跳过", cfg.MinAge))
		r.Suggestion = suggestionKeepVerify
		r.Confidence = ConfLow
		applyHeadlineScore(&r)
		return r
	}

	// SHUTOFF_LONG — only with a real lastOffTime age, or CAS zombie list.
	if f.Vm.Status == "shutOff" {
		if !f.Vm.LastOff.IsZero() {
			offDays := now.Sub(f.Vm.LastOff).Hours() / 24
			r.OffDays = offDays
			if offDays >= cfg.OffDays {
				r.Class = ClassShutoffIdle
				r.RawIdleScore = 100
				r.IdleScore = 100
				r.Confidence = ConfHigh
				r.ConfidenceValue = 0.9
				r.Reasons = append(r.Reasons, fmt.Sprintf(
					"SHUTOFF_LONG: 已关机 %.0f 天 (lastOffTime %s)",
					offDays, f.Vm.LastOff.In(loc).Format("2006-01-02 15:04 MST")))
				r.Suggestion = "release/回收"
				return r
			}
			// shut off but younger than OffDays — not idle-by-age
		} else if f.Vm.OnZombieList {
			r.Class = ClassShutoffIdle
			r.RawIdleScore = 95
			r.IdleScore = 95
			r.Confidence = ConfMedium
			r.ConfidenceValue = 0.7
			r.Reasons = append(r.Reasons,
				fmt.Sprintf("ZOMBIE_LIST: oneBtn/zombieVm 命中 (closeTimeout≥%.0fd)，无独立 lastOffTime", cfg.OffDays))
			r.Suggestion = "release/回收"
			r.DataGaps = append(r.DataGaps, "lastOffTime=n/a")
			return r
		} else {
			// Unknown age, not on zombie list: never 闲置(关机)/release.
			r.Class = ClassSkipped
			r.Confidence = ConfLow
			r.ConfidenceValue = 0.3
			r.Reasons = append(r.Reasons, "SHUTOFF_AGE_UNKNOWN: 已关机但无 lastOffTime，且不在 zombieVm 列表 — 请核实")
			r.Suggestion = suggestionKeepVerify
			r.DataGaps = append(r.DataGaps, "lastOffTime=n/a")
			applyHeadlineScore(&r)
			return r
		}
	}

	if f.Coverage < cfg.MinCoverage {
		r.Class = ClassInsufficient
		r.Confidence = ConfLow
		r.ConfidenceValue = f.Coverage
		r.Reasons = append(r.Reasons, fmt.Sprintf(
			"NO_DATA: 小时覆盖率 %.0f%% < %.0f%%", f.Coverage*100, cfg.MinCoverage*100))
		r.Suggestion = suggestionKeepVerify
		r.DataGaps = append(r.DataGaps, fmt.Sprintf("coverage=%.2f", f.Coverage))
		applyHeadlineScore(&r)
		return r
	}

	// Weighted idle score
	type part struct {
		w, s float64
		ok   bool
	}
	parts := []part{}
	if cfg.Signals["cpu"] && f.HasCpu {
		parts = append(parts, part{cfg.WeightCPU, subScore(f.CpuP95, cfg.CpuP95), true})
		if f.CpuAvg < cfg.CpuAvg {
			r.Reasons = append(r.Reasons, fmt.Sprintf(
				"LOW_CPU: %.0f 天 CPU 均值 %.1f%% (<%.0f%%, cloudpods SCALE_DOWN 默认)，小时峰值 p95 %.1f%%",
				cfg.Lookback.Hours()/24, f.CpuAvg, cfg.CpuAvg, f.CpuP95))
		}
	} else if cfg.Signals["cpu"] {
		r.DataGaps = append(r.DataGaps, "cpu=n/a")
		r.Reasons = append(r.Reasons, "NO_CPU_SERIES: 缺少 CPU 小时序列，不按 0% 计")
	}
	if f.DroppedBuckets > 0 {
		r.DataGaps = append(r.DataGaps, fmt.Sprintf("dropped_buckets=%d", f.DroppedBuckets))
	}
	if cfg.Signals["net"] && f.HasNet {
		parts = append(parts, part{cfg.WeightNet, subScore(f.NetP95, cfg.NetP95KBps), true})
		if f.NetP95 < cfg.NetP95KBps {
			r.Reasons = append(r.Reasons, fmt.Sprintf(
				"QUIET_NET: 网络 p95 %.1f KB/s，最大 %.1f KB/s", f.NetP95, f.NetMax))
		}
	} else if cfg.Signals["net"] {
		r.DataGaps = append(r.DataGaps, "net=n/a")
	}
	if cfg.Signals["disk"] && f.HasDisk {
		parts = append(parts, part{cfg.WeightDisk, subScore(f.DiskP95, cfg.DiskP95KBps), true})
		if f.DiskP95 < cfg.DiskP95KBps {
			r.Reasons = append(r.Reasons, fmt.Sprintf(
				"QUIET_DISK: 磁盘吞吐 p95 %.1f KB/s，最大 %.1f KB/s", f.DiskP95, f.DiskMax))
		}
	} else if cfg.Signals["disk"] {
		r.DataGaps = append(r.DataGaps, "disk=n/a")
	}
	if cfg.Signals["iops"] && f.HasIops {
		parts = append(parts, part{cfg.WeightIops, subScore(f.IopsP95, cfg.IopsP95), true})
	} else if cfg.Signals["iops"] {
		r.DataGaps = append(r.DataGaps, "iops=n/a")
	}
	if cfg.Signals["conn"] && f.HasConn {
		parts = append(parts, part{cfg.WeightConn, subScore(f.ConnMax, 5), true})
		if f.ConnMax <= 0 {
			r.Reasons = append(r.Reasons, "NO_CONN: flowLink 窗口内最大 0")
		}
	} else if cfg.Signals["conn"] {
		r.DataGaps = append(r.DataGaps, "flowLink=n/a")
	}

	var wSum, ws float64
	scores := []float64{}
	for _, p := range parts {
		if !p.ok || p.w <= 0 {
			continue
		}
		wSum += p.w
		ws += p.w * p.s
		scores = append(scores, p.s)
	}
	if wSum > 0 {
		r.RawIdleScore = 100 * ws / wSum
		r.IdleScore = r.RawIdleScore
	}

	// Recurring-spike veto
	veto := recurringSpikeVeto(f, cfg, loc)
	r.Veto = veto

	pat := classifyPattern(f, cfg, loc)
	r.ActiveRatio = pat.RTotal
	r.InWinRatio = pat.RIn
	r.OutWinRatio = pat.ROut
	r.DetectedWindow = pat.DetectedWindow
	r.Class = pat.Class
	if len(pat.Reason) > 0 {
		r.Reasons = append(r.Reasons, pat.Reason)
	}

	if veto {
		r.Reasons = append(r.Reasons, "VETO: 检测到跨多天的周期性高负载尖峰，不作闲置判定")
		if r.Class == ClassIdle || r.Class == ClassShutoffIdle {
			r.Class = ClassIntermittent
		}
	}

	// Missing CPU series: never emit confident 闲置 (would be faking 0% CPU).
	if cfg.Signals["cpu"] && !f.HasCpu && (r.Class == ClassIdle || r.Class == "") {
		r.Class = ClassInsufficient
		r.Reasons = append(r.Reasons, "NO_CPU_SERIES: 无 CPU 数据，不做闲置判定")
	}

	// Score thresholds refine class when pattern said idle (spec §6.3):
	// ≥80 idle, 50–80 intermittent, <50 active.
	if !veto && r.Class == ClassIdle {
		if r.RawIdleScore < 50 {
			r.Class = ClassActive
		} else if r.RawIdleScore < IdleClassMinScore {
			r.Class = ClassIntermittent
		}
	}
	if r.Class == "" {
		if r.RawIdleScore >= IdleClassMinScore {
			r.Class = ClassIdle
		} else if r.RawIdleScore >= 50 {
			r.Class = ClassIntermittent
		} else {
			r.Class = ClassActive
		}
	}

	// Concrete burst reason for intermittent (overrides generic pattern text).
	if r.Class == ClassIntermittent {
		if burst := describeBurst(f, loc); burst != "" {
			r.Reasons = append(r.Reasons, "BURST: "+burst)
			// Prefer burst line as the primary reason for table display:
			// move BURST to front after pattern reason already appended.
			if len(r.Reasons) >= 2 {
				last := r.Reasons[len(r.Reasons)-1]
				// keep LOW_CPU etc., but ensure BURST is early in the list
				r.Reasons = append([]string{last}, r.Reasons[:len(r.Reasons)-1]...)
			}
		}
	}

	r.Suggestion = suggestionFor(r.Class, pat.DetectedWindow, cfg)
	applyHeadlineScore(&r)
	r.ConfidenceValue = confidence(f, cfg, scores, now)
	r.Confidence = confLabel(r.ConfidenceValue)
	return r
}

// applyHeadlineScore implements HeadlineIdleScoreRule (see IdleClassMinScore).
func applyHeadlineScore(r *Result) {
	switch r.Class {
	case ClassIdle, ClassShutoffIdle:
		r.IdleScore = r.RawIdleScore
		r.ScoreHidden = false
	default:
		// Never show a raw ≥ IdleClassMinScore next to a non-idle class.
		r.ScoreHidden = true
		r.IdleScore = -1
	}
}

func firstNonEmpty(a, b string) string {
	if len(a) > 0 {
		return a
	}
	return b
}

func suggestionFor(c Class, window string, cfg Config) string {
	switch c {
	case ClassIdle, ClassShutoffIdle:
		return "release/回收"
	case ClassScheduled:
		if len(window) > 0 {
			return fmt.Sprintf("schedule on/off around %s (margin %s; print-only)", window, cfg.ScheduleMargin)
		}
		return "schedule on/off around work hours (print-only)"
	case ClassIntermittent:
		return suggestionKeepVerify // never downsize/release
	case ClassSkipped, ClassInsufficient:
		return suggestionKeepVerify
	default:
		return "keep"
	}
}

func confLabel(v float64) ConfidenceLabel {
	if v >= 0.75 {
		return ConfHigh
	}
	if v >= 0.5 {
		return ConfMedium
	}
	return ConfLow
}

func confidence(f VmFeatures, cfg Config, scores []float64, now time.Time) float64 {
	coverage := clamp01(f.Coverage)
	// signal completeness vs full weight set
	// Denominator: enabled signals only (conn/pps/proc/lifecycle stubs that
	// are off add no weight — D3/D4). Missing series reduce `have`.
	full := 0.0
	have := 0.0
	if cfg.Signals["cpu"] {
		full += cfg.WeightCPU
		if f.HasCpu {
			have += cfg.WeightCPU
		}
	}
	if cfg.Signals["net"] {
		full += cfg.WeightNet
		if f.HasNet {
			have += cfg.WeightNet
		}
	}
	if cfg.Signals["disk"] {
		full += cfg.WeightDisk
		if f.HasDisk {
			have += cfg.WeightDisk
		}
	}
	if cfg.Signals["iops"] {
		full += cfg.WeightIops
		if f.HasIops {
			have += cfg.WeightIops
		}
	}
	if cfg.Signals["conn"] {
		full += cfg.WeightConn
		if f.HasConn {
			have += cfg.WeightConn
		}
	}
	completeness := 0.0
	if full > 0 {
		completeness = have / full
	}
	agreement := 1.0
	if len(scores) > 1 {
		mean := 0.0
		for _, s := range scores {
			mean += s
		}
		mean /= float64(len(scores))
		var v float64
		for _, s := range scores {
			d := s - mean
			v += d * d
		}
		std := math.Sqrt(v / float64(len(scores)))
		agreement = clamp01(1 - std)
	}
	age := 1.0
	if !f.Vm.CreateDate.IsZero() && cfg.Lookback > 0 {
		age = clamp01(float64(now.Sub(f.Vm.CreateDate)) / float64(cfg.Lookback))
	}
	return coverage * completeness * agreement * age
}

// recurringSpikeVeto: true when the same clock-hour shows a high-load
// spike (CPU≥50% or net≥1 MB/s) on ≥3 distinct calendar days (spec §6.2).
func recurringSpikeVeto(f VmFeatures, cfg Config, loc *time.Location) bool {
	for _, days := range f.spikeByHour {
		if len(days) >= 3 {
			return true
		}
	}
	return len(f.spike) >= 3
}

func (f VmFeatures) spikeDays() []string {
	out := make([]string, 0, len(f.spike))
	for d := range f.spike {
		out = append(out, d)
	}
	return out
}

// SetSpikeDays records calendar days (YYYY-MM-DD) with a high-load spike.
func (f *VmFeatures) SetSpikeDays(days []string) {
	f.spike = map[string]bool{}
	for _, d := range days {
		f.spike[d] = true
	}
}
