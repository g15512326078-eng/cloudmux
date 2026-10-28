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
	"strconv"
	"time"
)

var weekdayAbbr = [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

type burstSeg struct {
	day      time.Time // local midnight of the day
	startH   int       // inclusive hour 0-23
	endH     int       // exclusive hour 1-24
	peakCPU  float64
	peakNet  float64 // KB/s
	peakDisk float64 // KB/s
	weekday  time.Weekday
}

// burstPeak is the peak of one segment group and the signal that dominates
// it (largest ratio to its strict idle gate, so a net-only backup is named
// as net, not as a 2% CPU "load").
type burstPeak struct {
	cpu, net, disk float64
}

func (p *burstPeak) add(s burstSeg) {
	p.cpu = math.Max(p.cpu, s.peakCPU)
	p.net = math.Max(p.net, s.peakNet)
	p.disk = math.Max(p.disk, s.peakDisk)
}

func ratio(v, gate float64) float64 {
	if gate <= 0 {
		return 0
	}
	return v / gate
}

// signal returns "cpu", "net" or "disk".
func (p burstPeak) signal(cfg Config) string {
	sig, best := "cpu", ratio(p.cpu, cfg.IdlePeakCPU)
	if r := ratio(p.net, cfg.IdlePeakNetKBps); r > best {
		sig, best = "net", r
	}
	if r := ratio(p.disk, cfg.IdlePeakDiskKBps); r > best {
		sig = "disk"
	}
	return sig
}

// label renders the dominant signal, e.g. "load 90%", "net 5 MB/s", "disk 20 MB/s".
func (p burstPeak) label(cfg Config) string {
	switch p.signal(cfg) {
	case "net":
		return "net " + formatKBps(p.net)
	case "disk":
		return "disk " + formatKBps(p.disk)
	}
	return fmt.Sprintf("load %.0f%%", p.cpu)
}

// peakOnly renders the dominant peak for the monthly/single phrases:
// "95%" for CPU (unchanged wording), "net 5 MB/s" / "disk 20 MB/s" otherwise.
func (p burstPeak) peakOnly(cfg Config) string {
	if p.signal(cfg) == "cpu" {
		return fmt.Sprintf("%.0f%%", p.cpu)
	}
	return p.label(cfg)
}

// formatKBps renders a KB/s value: ≥1024 as MB/s ("5 MB/s", "1.5 MB/s"),
// otherwise "300 KB/s".
func formatKBps(kbps float64) string {
	if kbps >= 1024 {
		mb := strconv.FormatFloat(math.Round(kbps/1024*10)/10, 'f', -1, 64)
		return mb + " MB/s"
	}
	return fmt.Sprintf("%.0f KB/s", kbps)
}

// describeBurst builds a concrete burst phrase from active hours,
// naming the dominant signal. Order: daily/workday pattern, weekly, then
// monthly/single, e.g. "daily 03-04 load 60%", "workdays 19-23 load 40%",
// "every Sun 02-04 net 5 MB/s", "monthly 6h burst peak 95%",
// "single 2h burst peak disk 20 MB/s". Peaks come from the single report
// rate field (avg vs max UNVERIFIED).
func describeBurst(f VmFeatures, cfg Config, loc *time.Location) string {
	segs := activeSegments(f, loc)
	if len(segs) == 0 {
		all := burstPeak{cpu: f.CpuMax, net: f.NetMax, disk: f.DiskMax}
		if peakGateHit(f, cfg) != "" {
			return "burst peak " + all.label(cfg)
		}
		return ""
	}

	type win struct{ start, end int }

	// Daily / workday: same [startH,endH) on ≥5 distinct days spanning
	// ≥5 distinct weekdays. Checked before weekly so a daily cron is
	// not reported as "every Wed".
	byWin := map[win][]burstSeg{}
	for _, s := range segs {
		k := win{s.startH, s.endH}
		byWin[k] = append(byWin[k], s)
	}
	type dcand struct {
		w        win
		days     int
		workOnly bool
		peak     burstPeak
	}
	var daily []dcand
	for w, list := range byWin {
		days := map[string]bool{}
		wds := map[time.Weekday]bool{}
		var pk burstPeak
		for _, s := range list {
			days[s.day.Format("2006-01-02")] = true
			wds[s.weekday] = true
			pk.add(s)
		}
		if len(days) < 5 || len(wds) < 5 {
			continue
		}
		workOnly := !wds[time.Saturday] && !wds[time.Sunday]
		daily = append(daily, dcand{w: w, days: len(days), workOnly: workOnly, peak: pk})
	}
	if len(daily) > 0 {
		sort.Slice(daily, func(i, j int) bool {
			if daily[i].days != daily[j].days {
				return daily[i].days > daily[j].days
			}
			return daily[i].w.start < daily[j].w.start
		})
		d := daily[0]
		prefix := "daily"
		if d.workOnly {
			prefix = "workdays"
		}
		return fmt.Sprintf("%s %02d-%02d %s", prefix, d.w.start, d.w.end, d.peak.label(cfg))
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
		peak  burstPeak
		weeks int
	}
	var weekly []cand
	for k, list := range byKey {
		weeks := map[string]bool{}
		var pk burstPeak
		for _, s := range list {
			y, w := s.day.ISOWeek()
			weeks[fmt.Sprintf("%d-W%02d", y, w)] = true
			pk.add(s)
		}
		if len(weeks) >= 2 {
			weekly = append(weekly, cand{k: k, peak: pk, weeks: len(weeks)})
		}
	}
	if len(weekly) > 0 {
		sort.Slice(weekly, func(i, j int) bool {
			if weekly[i].weeks != weekly[j].weeks {
				return weekly[i].weeks > weekly[j].weeks
			}
			if weekly[i].peak.cpu != weekly[j].peak.cpu {
				return weekly[i].peak.cpu > weekly[j].peak.cpu
			}
			if weekly[i].k.wd != weekly[j].k.wd {
				return weekly[i].k.wd < weekly[j].k.wd
			}
			return weekly[i].k.start < weekly[j].k.start
		})
		w := weekly[0]
		return fmt.Sprintf("every %s %02d-%02d %s",
			weekdayAbbr[w.k.wd], w.k.start, w.k.end, w.peak.label(cfg))
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
	var pk burstPeak
	pk.add(best)
	if pk.signal(cfg) == "cpu" && pk.cpu < f.CpuMax {
		pk.cpu = f.CpuMax
	}
	if hours >= 3 {
		return fmt.Sprintf("monthly %dh burst peak %s", hours, pk.peakOnly(cfg))
	}
	return fmt.Sprintf("single %dh burst peak %s", hours, pk.peakOnly(cfg))
}

func activeSegments(f VmFeatures, loc *time.Location) []burstSeg {
	type mark struct {
		t              time.Time
		cpu, net, disk float64
	}
	at := func(v []float64, i int) float64 {
		if i < len(v) {
			return v[i]
		}
		return 0
	}
	active := []mark{}
	for i, h := range f.Hours {
		if i < len(f.Active) && f.Active[i] {
			active = append(active, mark{
				t: h.In(loc), cpu: at(f.HourCPU, i), net: at(f.HourNet, i), disk: at(f.HourDisk, i),
			})
		}
	}
	if len(active) == 0 {
		return nil
	}
	sort.Slice(active, func(i, j int) bool { return active[i].t.Before(active[j].t) })

	newSeg := func(a mark) burstSeg {
		return burstSeg{
			day: truncDay(a.t), startH: a.t.Hour(), endH: a.t.Hour() + 1,
			peakCPU: a.cpu, peakNet: a.net, peakDisk: a.disk, weekday: a.t.Weekday(),
		}
	}
	segs := []burstSeg{}
	cur := newSeg(active[0])
	for i := 1; i < len(active); i++ {
		a := active[i]
		sameDay := truncDay(a.t).Equal(cur.day)
		if sameDay && a.t.Hour() == cur.endH {
			cur.endH = a.t.Hour() + 1
			cur.peakCPU = math.Max(cur.peakCPU, a.cpu)
			cur.peakNet = math.Max(cur.peakNet, a.net)
			cur.peakDisk = math.Max(cur.peakDisk, a.disk)
			continue
		}
		segs = append(segs, cur)
		cur = newSeg(a)
	}
	segs = append(segs, cur)
	return segs
}

func truncDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// suggestionKeepVerify is the only suggestion allowed for intermittent.
const suggestionKeepVerify = "keep/verify"
