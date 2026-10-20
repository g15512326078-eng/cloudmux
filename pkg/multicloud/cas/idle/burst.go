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
	"sort"
	"strings"
	"time"
)

var weekdayCN = [...]string{"日", "一", "二", "三", "四", "五", "六"}

type burstSeg struct {
	day     time.Time // local midnight of the day
	startH  int       // inclusive hour 0-23
	endH    int       // exclusive hour 1-24
	peakCPU float64
	weekday time.Weekday
}

// describeBurst builds a concrete Chinese burst phrase from active hours,
// e.g. "每周日 02-04 负载 90%", "每月一次 6 小时突发 峰值 95%",
// "单次 2 小时突发 峰值 80%". Peak is from HourCPU (D9: single rate field).
func describeBurst(f VmFeatures, loc *time.Location) string {
	segs := activeSegments(f, loc)
	if len(segs) == 0 {
		if f.CpuMax >= 20 {
			return fmt.Sprintf("突发峰值 CPU %.0f%%", f.CpuMax)
		}
		return ""
	}

	// Weekly: same weekday + same [startH,endH) on ≥2 distinct calendar weeks.
	type key struct {
		wd         time.Weekday
		start, end int
	}
	byKey := map[key][]burstSeg{}
	for _, s := range segs {
		k := key{s.weekday, s.startH, s.endH}
		byKey[k] = append(byKey[k], s)
	}
	type cand struct {
		k     key
		peak  float64
		weeks int
	}
	var weekly []cand
	for k, list := range byKey {
		weeks := map[string]bool{}
		peak := 0.0
		for _, s := range list {
			y, w := s.day.ISOWeek()
			weeks[fmt.Sprintf("%d-W%02d", y, w)] = true
			if s.peakCPU > peak {
				peak = s.peakCPU
			}
		}
		if len(weeks) >= 2 {
			weekly = append(weekly, cand{k: k, peak: peak, weeks: len(weeks)})
		}
	}
	if len(weekly) > 0 {
		sort.Slice(weekly, func(i, j int) bool {
			if weekly[i].weeks != weekly[j].weeks {
				return weekly[i].weeks > weekly[j].weeks
			}
			return weekly[i].peak > weekly[j].peak
		})
		w := weekly[0]
		return fmt.Sprintf("每周%s %02d-%02d 负载 %.0f%%",
			weekdayCN[w.k.wd], w.k.start, w.k.end, w.peak)
	}

	// Longest single-day segment
	best := segs[0]
	for _, s := range segs[1:] {
		dur, bestDur := s.endH-s.startH, best.endH-best.startH
		if dur > bestDur || (dur == bestDur && s.peakCPU > best.peakCPU) {
			best = s
		}
	}
	hours := best.endH - best.startH
	peak := best.peakCPU
	if peak < f.CpuMax {
		peak = f.CpuMax
	}
	if hours >= 3 {
		return fmt.Sprintf("每月一次 %d 小时突发 峰值 %.0f%%", hours, peak)
	}
	return fmt.Sprintf("单次 %d 小时突发 峰值 %.0f%%", hours, peak)
}

func activeSegments(f VmFeatures, loc *time.Location) []burstSeg {
	type mark struct {
		t   time.Time
		cpu float64
	}
	active := []mark{}
	for i, h := range f.Hours {
		if i < len(f.Active) && f.Active[i] {
			cpu := 0.0
			if i < len(f.HourCPU) {
				cpu = f.HourCPU[i]
			}
			active = append(active, mark{t: h.In(loc), cpu: cpu})
		}
	}
	if len(active) == 0 {
		return nil
	}
	sort.Slice(active, func(i, j int) bool { return active[i].t.Before(active[j].t) })

	segs := []burstSeg{}
	cur := burstSeg{
		day:     truncDay(active[0].t),
		startH:  active[0].t.Hour(),
		endH:    active[0].t.Hour() + 1,
		peakCPU: active[0].cpu,
		weekday: active[0].t.Weekday(),
	}
	for i := 1; i < len(active); i++ {
		a := active[i]
		sameDay := truncDay(a.t).Equal(cur.day)
		contiguous := sameDay && a.t.Hour() == cur.endH
		if contiguous {
			cur.endH = a.t.Hour() + 1
			if a.cpu > cur.peakCPU {
				cur.peakCPU = a.cpu
			}
			continue
		}
		segs = append(segs, cur)
		cur = burstSeg{
			day: truncDay(a.t), startH: a.t.Hour(), endH: a.t.Hour() + 1,
			peakCPU: a.cpu, weekday: a.t.Weekday(),
		}
	}
	segs = append(segs, cur)
	return segs
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// suggestionKeepVerify is the only suggestion allowed for 间歇使用.
const suggestionKeepVerify = "保留/核实"

func suggestionForbiddenForNonIdle(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "downsize") ||
		strings.Contains(s, "release") ||
		strings.Contains(s, "回收")
}
