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
	"strings"
	"time"
)

type timeWindow struct {
	Start, End time.Duration // from midnight
}

type patternStats struct {
	Class          Class
	RTotal         float64
	RIn            float64
	ROut           float64
	DWork          float64
	DetectedWindow string
	Reason         string
	Ac24, Ac168    float64
	HolidayGap     string // data_gaps entry when --holiday-file is unusable
}

func parseWorkHours(s string) ([]timeWindow, error) {
	parts := strings.Split(s, ",")
	out := []timeWindow{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kv := strings.Split(p, "-")
		if len(kv) != 2 {
			return nil, fmt.Errorf("bad work-hours segment %q", p)
		}
		st, err := parseHHMM(kv[0])
		if err != nil {
			return nil, err
		}
		en, err := parseHHMM(kv[1])
		if err != nil {
			return nil, err
		}
		out = append(out, timeWindow{Start: st, End: en})
	}
	return out, nil
}

func parseHHMM(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var h, m int
	_, err := fmt.Sscanf(s, "%d:%d", &h, &m)
	if err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("bad time %q", s)
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, nil
}

func parseWorkdays(s string) map[time.Weekday]bool {
	s = strings.ToLower(strings.TrimSpace(s))
	out := map[time.Weekday]bool{}
	if s == "mon-fri" || s == "weekday" {
		for d := time.Monday; d <= time.Friday; d++ {
			out[d] = true
		}
		return out
	}
	names := map[string]time.Weekday{
		"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
	}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if d, ok := names[p]; ok {
			out[d] = true
		}
	}
	return out
}

func inWorkWindow(t time.Time, wins []timeWindow, overlap float64) bool {
	// Hour bucket [t, t+1h). Count as in-window if overlap fraction ≥ threshold.
	bucketStart := time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
	bucketEnd := bucketStart + time.Hour
	var overlapDur time.Duration
	for _, w := range wins {
		lo := maxDur(bucketStart, w.Start)
		hi := minDur(bucketEnd, w.End)
		if hi > lo {
			overlapDur += hi - lo
		}
	}
	return float64(overlapDur) >= overlap*float64(time.Hour)
}

func maxDur(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

type holidayCalendar interface {
	IsWorkday(d time.Time) (workday bool, known bool)
}

func classifyPattern(f VmFeatures, cfg Config, loc *time.Location) patternStats {
	wins, err := parseWorkHours(cfg.WorkHours)
	if err != nil {
		wins, _ = parseWorkHours("08:30-11:30,13:00-18:00")
	}
	workdays := parseWorkdays(cfg.Workdays)
	cal, holidayGap := loadHolidayCalendar(cfg.HolidayFile)

	var covered, activeN int
	var inCovered, inActive, outCovered, outActive int
	workdaySeen := map[string]bool{}
	workdayActive := map[string]bool{}
	profile := make([]int, 24) // active counts per hour on workdays
	profileN := make([]int, 24)

	series := make([]float64, len(f.Active)) // 1/0 for acf
	for i, h := range f.Hours {
		if i >= len(f.Active) {
			break
		}
		covered++
		local := h.In(loc)
		act := f.Active[i]
		if act {
			activeN++
			series[i] = 1
		}
		dateKey := local.Format("2006-01-02")
		isWork := workdays[local.Weekday()]
		if cal != nil {
			if w, known := cal.IsWorkday(local); known {
				isWork = w
			}
		}
		inWin := isWork && inWorkWindow(local, wins, cfg.WindowOverlap)
		if inWin {
			inCovered++
			profileN[local.Hour()]++
			if act {
				inActive++
				workdayActive[dateKey] = true
				profile[local.Hour()]++
			}
			workdaySeen[dateKey] = true
		} else {
			outCovered++
			if act {
				outActive++
			}
		}
	}

	ps := patternStats{HolidayGap: holidayGap}
	if covered > 0 {
		ps.RTotal = float64(activeN) / float64(covered)
	}
	if inCovered > 0 {
		ps.RIn = float64(inActive) / float64(inCovered)
	}
	if outCovered > 0 {
		ps.ROut = float64(outActive) / float64(outCovered)
	}
	if len(workdaySeen) > 0 {
		a := 0
		for d := range workdaySeen {
			if workdayActive[d] {
				a++
			}
		}
		ps.DWork = float64(a) / float64(len(workdaySeen))
	}
	ps.Ac24 = autocorr(series, 24)
	ps.Ac168 = autocorr(series, 168)

	// Detected window from profile
	ps.DetectedWindow = detectWindow(profile, profileN, cfg.PatternMinProb, wins)

	// Classification order per research §7.3
	// Peak gates cover CPU, net and disk (R1): a weekly net-only or
	// disk-only backup must not pass as 闲置.
	peakSig := peakGateHit(f, cfg)
	idleOK := ps.RTotal <= 0.02 && peakSig == ""
	// also: no day with >2 active hours
	dayActiveHours := map[string]int{}
	for i, h := range f.Hours {
		if i < len(f.Active) && f.Active[i] {
			dayActiveHours[h.In(loc).Format("2006-01-02")]++
		}
	}
	busyDay := false
	for _, n := range dayActiveHours {
		if n > 2 {
			busyDay = true
			break
		}
	}
	if idleOK && !busyDay {
		ps.Class = ClassIdle
		ps.Reason = fmt.Sprintf("PATTERN_IDLE: 活跃小时占比 %.1f%%，无超过 2 小时的活跃日", ps.RTotal*100)
		return ps
	}
	if ps.RIn >= 0.30 && ps.ROut <= 0.05 && ps.DWork >= 0.6 && (ps.Ac24 >= 0.3 || ps.Ac168 >= 0.3) {
		ps.Class = ClassScheduled
		if len(ps.DetectedWindow) == 0 {
			ps.DetectedWindow = "工作日 " + cfg.WorkHours
		}
		ps.Reason = fmt.Sprintf("PATTERN_SCHEDULED: 窗内活跃 %.0f%%，窗外 %.0f%%，工作日覆盖 %.0f%%，窗口 %s",
			ps.RIn*100, ps.ROut*100, ps.DWork*100, ps.DetectedWindow)
		return ps
	}
	// custom detected window
	if len(ps.DetectedWindow) > 0 && ps.ROut <= 0.05 && (ps.Ac24 >= 0.3 || ps.Ac168 >= 0.3) {
		ps.Class = ClassScheduled
		ps.Reason = fmt.Sprintf("PATTERN_SCHEDULED_CUSTOM: 检测到时段 %s", ps.DetectedWindow)
		return ps
	}
	if ps.RTotal >= 0.5 || ps.ROut > 0.2 {
		ps.Class = ClassActive
		ps.Reason = fmt.Sprintf("PATTERN_ACTIVE: 活跃占比 %.0f%%，窗外 %.0f%%", ps.RTotal*100, ps.ROut*100)
		return ps
	}
	if ps.RTotal > 0.02 {
		ps.Class = ClassIntermittent
		ps.Reason = fmt.Sprintf("PATTERN_INTERMITTENT: 活跃占比 %.1f%%，无稳定周期", ps.RTotal*100)
		return ps
	}
	// Spec §7.3: ClassIdle only from the strict rule above. Anything that
	// reached here with low R_total failed idle because of a busy day or
	// peak (batch/burst) → intermittent, never idle.
	ps.Class = ClassIntermittent
	ps.Reason = fmt.Sprintf(
		"PATTERN_INTERMITTENT: 低总活跃(%.1f%%)但有忙日或高峰(%s)，非闲置",
		ps.RTotal*100, peakSummary(f))
	return ps
}

// peakSummary renders window peaks with units, e.g. "CPU 3% 网络 5.0 MB/s 磁盘 0.0 MB/s".
func peakSummary(f VmFeatures) string {
	parts := []string{}
	if f.HasCpu {
		parts = append(parts, fmt.Sprintf("CPU %.0f%%", f.CpuMax))
	}
	if f.HasNet {
		parts = append(parts, "网络 "+formatKBps(f.NetMax))
	}
	if f.HasDisk {
		parts = append(parts, "磁盘 "+formatKBps(f.DiskMax))
	}
	return strings.Join(parts, " ")
}

func detectWindow(profile, profileN []int, minProb float64, fallback []timeWindow) string {
	prob := make([]float64, 24)
	for h := 0; h < 24; h++ {
		if profileN[h] > 0 {
			prob[h] = float64(profile[h]) / float64(profileN[h])
		}
	}
	type seg struct{ a, b int }
	segs := []seg{}
	i := 0
	for i < 24 {
		if prob[i] >= minProb {
			j := i
			for j+1 < 24 && (prob[j+1] >= minProb || (j+1-i <= 8 && j+1 < 24 && prob[j+1] < minProb && j+2 < 24 && prob[j+2] >= minProb)) {
				// allow 1-hour gap (lunch)
				if prob[j+1] < minProb {
					j += 2
					if j >= 24 {
						j = 23
						break
					}
					continue
				}
				j++
			}
			segs = append(segs, seg{i, j})
			i = j + 1
			continue
		}
		i++
	}
	if len(segs) == 0 {
		return ""
	}
	parts := []string{}
	for _, s := range segs {
		parts = append(parts, fmt.Sprintf("%02d:00-%02d:00", s.a, s.b+1))
	}
	return "工作日 " + strings.Join(parts, ",")
}

func autocorr(x []float64, lag int) float64 {
	n := len(x)
	if lag <= 0 || n <= lag {
		return 0
	}
	mean := 0.0
	for _, v := range x {
		mean += v
	}
	mean /= float64(n)
	var num, den float64
	for i := 0; i < n; i++ {
		d := x[i] - mean
		den += d * d
	}
	if den == 0 {
		return 0
	}
	for i := 0; i+lag < n; i++ {
		num += (x[i] - mean) * (x[i+lag] - mean)
	}
	return num / den
}

// peakGateHit returns the first signal whose window peak reaches its strict
// idle gate ("cpu", "net", "disk"), or "" when all peaks are below.
func peakGateHit(f VmFeatures, cfg Config) string {
	if f.HasCpu && f.CpuMax >= cfg.IdlePeakCPU {
		return "cpu"
	}
	if f.HasNet && f.NetMax >= cfg.IdlePeakNetKBps {
		return "net"
	}
	if f.HasDisk && f.DiskMax >= cfg.IdlePeakDiskKBps {
		return "disk"
	}
	return ""
}
