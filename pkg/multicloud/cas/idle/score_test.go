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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// synthHours builds lookback days of hourly points starting at Monday 00:00.
func synthHours(loc *time.Location, days int, fill func(day int, hour int) (cpu, net, disk, iops float64)) HourlyBundle {
	// 2026-03-02 is a Monday
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, loc)
	b := HourlyBundle{}
	for d := 0; d < days; d++ {
		for h := 0; h < 24; h++ {
			ts := start.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour)
			cpu, net, disk, iops := fill(d, h)
			b.CPU = append(b.CPU, HourlyPoint{Time: ts, Value: cpu})
			b.Net = append(b.Net, HourlyPoint{Time: ts, Value: net})
			b.Disk = append(b.Disk, HourlyPoint{Time: ts, Value: disk})
			b.Iops = append(b.Iops, HourlyPoint{Time: ts, Value: iops})
		}
	}
	return b
}

func baseVM(status string) VmInventory {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	return VmInventory{
		Uuid: "u-test", Name: "vm-test", Title: "vm-test", Status: status,
		CreateDate: time.Date(2025, 1, 1, 0, 0, 0, 0, loc),
	}
}

func TestTrulyIdleVM(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		return 0.5, 0.1, 0.1, 0.1
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassIdle {
		t.Fatalf("class=%s score=%.1f reasons=%v", r.Class, r.IdleScore, r.Reasons)
	}
	if r.IdleScore < 80 {
		t.Errorf("score %.1f", r.IdleScore)
	}
}

func TestWorkHoursScheduledNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	// Busy only Mon-Fri 09-11 and 13-17; quiet otherwise
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
		if day.Weekday() >= time.Monday && day.Weekday() <= time.Friday {
			if (h >= 9 && h < 11) || (h >= 13 && h < 17) {
				return 35, 200, 300, 30
			}
		}
		return 0.4, 0.2, 0.2, 0.2
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassScheduled {
		t.Fatalf("want scheduled, got %s score=%.1f in=%.2f out=%.2f reasons=%v",
			r.Class, r.IdleScore, r.InWinRatio, r.OutWinRatio, r.Reasons)
	}
	if r.Class == ClassIdle {
		t.Fatal("must NOT be idle")
	}
	if len(r.DetectedWindow) == 0 {
		t.Error("expected detected window")
	}
}

func TestIntermittentVM(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		// A few random busy hours on day 3 and 10 only
		if (d == 3 || d == 10) && h == 15 {
			return 40, 100, 100, 10
		}
		return 1, 1, 1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassIntermittent {
		t.Fatalf("burst hours want intermittent, got %s score=%.1f reasons=%v", r.Class, r.IdleScore, r.Reasons)
	}
}

func TestActiveVM(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		return 40, 100, 100, 20
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassActive {
		t.Fatalf("class=%s score=%.1f", r.Class, r.IdleScore)
	}
}

func TestShutoffLong(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	vm := baseVM("shutOff")
	vm.LastOff = time.Date(2026, 1, 1, 10, 0, 0, 0, loc)
	f := VmFeatures{Vm: vm, Coverage: 1}
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassShutoffIdle {
		t.Fatalf("class=%s", r.Class)
	}
	if r.OffDays < 30 {
		t.Errorf("off_days=%v", r.OffDays)
	}
}

func TestLowCoverageInsufficient(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := HourlyBundle{}
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, loc)
	for i := 0; i < 24; i++ { // only 1 day of 14
		b.CPU = append(b.CPU, HourlyPoint{Time: start.Add(time.Duration(i) * time.Hour), Value: 1})
	}
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassInsufficient {
		t.Fatalf("class=%s coverage=%.2f", r.Class, f.Coverage)
	}
	if r.Confidence != ConfLow {
		t.Errorf("confidence %s", r.Confidence)
	}
}

func TestSpikeVeto(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		// mostly idle but CPU 60% at noon on days 0,2,4 (≥3 days) → veto
		if (d == 0 || d == 2 || d == 4) && h == 12 {
			return 60, 5, 5, 1
		}
		return 0.5, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if !r.Veto {
		t.Fatalf("expected veto, class=%s spike=%v", r.Class, f.spikeDays())
	}
	if r.Class == ClassIdle {
		t.Fatal("veto must not leave class as idle")
	}
}

func TestHolidayJSON(t *testing.T) {
	loc := shLoc(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "holidays.json")
	// 2026-03-07 is Saturday; mark as makeup workday. 2026-03-09 Monday as holiday.
	data := map[string]string{
		"2026-03-07": "workday",
		"2026-03-09": "holiday",
	}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.HolidayFile = path
	cal, gap := loadHolidayCalendar(path)
	if gap != "" {
		t.Fatalf("unexpected holiday gap %q", gap)
	}
	sat := time.Date(2026, 3, 7, 10, 0, 0, 0, loc)
	mon := time.Date(2026, 3, 9, 10, 0, 0, 0, loc)
	if w, ok := cal.IsWorkday(sat); !ok || !w {
		t.Errorf("sat makeup workday: w=%v ok=%v", w, ok)
	}
	if w, ok := cal.IsWorkday(mon); !ok || w {
		t.Errorf("mon holiday: w=%v ok=%v", w, ok)
	}
	// Pattern: busy only on the makeup-workday Saturday work window → still scheduled-ish
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
		if day.Format("2006-01-02") == "2026-03-07" && h >= 9 && h < 17 {
			return 30, 100, 100, 10
		}
		if day.Weekday() >= time.Monday && day.Weekday() <= time.Friday && day.Format("2006-01-02") != "2026-03-09" {
			if (h >= 9 && h < 11) || (h >= 13 && h < 17) {
				return 30, 100, 100, 10
			}
		}
		return 0.3, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class == ClassIdle {
		t.Fatalf("holiday-aware busy VM must not be idle: %v", r)
	}
}

func TestResultJSONRoundTrip(t *testing.T) {
	r := Result{Name: "a", Uuid: "u", Class: ClassIdle, IdleScore: 90, Reasons: []string{"LOW_CPU: x"}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err := json.Unmarshal(raw, &back); err != nil || back.Class != ClassIdle {
		t.Fatalf("%s %v", raw, err)
	}
}

func TestWeeklySundayBatchNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	// Weekly Sunday 02:00-04:00 at 90% CPU
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
		if day.Weekday() == time.Sunday && h >= 2 && h < 4 {
			return 90, 5, 5, 1
		}
		return 0.4, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	assertIntermittentBurst(t, r, "every Sun", "02-04", 90)
}

func TestMonthlySixHourBurstNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		if d == 5 && h >= 1 && h < 7 {
			return 95, 10, 10, 2
		}
		return 0.3, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	assertIntermittentBurst(t, r, "monthly", "6h", 95)
}

func TestSingleTwoHourBurstNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		if d == 8 && (h == 3 || h == 4) {
			return 95, 8, 8, 1
		}
		return 0.3, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	assertIntermittentBurst(t, r, "single", "2h", 95)
}

func TestMissingCPUSeriesNotFakedIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	start := time.Date(2026, 3, 2, 0, 0, 0, 0, loc)
	b := HourlyBundle{}
	for d := 0; d < 14; d++ {
		for h := 0; h < 24; h++ {
			ts := start.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour)
			b.Net = append(b.Net, HourlyPoint{Time: ts, Value: 0.1})
			b.Disk = append(b.Disk, HourlyPoint{Time: ts, Value: 0.1})
		}
	}
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	if f.HasCpu {
		t.Fatal("HasCpu should be false")
	}
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class == ClassIdle {
		t.Fatalf("missing CPU must not be idle: %v gaps=%v", r.Reasons, r.DataGaps)
	}
	found := false
	for _, g := range r.DataGaps {
		if g == "cpu=n/a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected cpu=n/a in data_gaps: %v", r.DataGaps)
	}
	for _, reason := range r.Reasons {
		if len(reason) >= 7 && reason[:7] == "LOW_CPU" {
			t.Fatalf("must not claim LOW_CPU with faked 0%%: %v", r.Reasons)
		}
	}
}

func TestShutoffUnknownAgeNoRelease(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	vm := baseVM("shutOff")
	// LastOff zero, not on zombie list
	f := VmFeatures{Vm: vm, Coverage: 1}
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class == ClassShutoffIdle {
		t.Fatalf("unknown age must not be idle-shutoff: %v", r)
	}
	if r.Suggestion == "release" {
		t.Fatalf("suggestion=%q", r.Suggestion)
	}
	if r.Suggestion != suggestionKeepVerify {
		t.Errorf("suggestion=%q want %q", r.Suggestion, suggestionKeepVerify)
	}
	if r.Confidence != ConfLow {
		t.Errorf("confidence %s", r.Confidence)
	}
}

func TestZombieListWithoutFabricatedLastOff(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	vm := baseVM("shutOff")
	vm.OnZombieList = true
	f := VmFeatures{Vm: vm, Coverage: 1}
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	r := ScoreAndClassify(f, cfg, now, loc)
	if r.Class != ClassShutoffIdle {
		t.Fatalf("class=%s", r.Class)
	}
	joined := strings.Join(r.Reasons, ";")
	if !strings.Contains(joined, "ZOMBIE_LIST") {
		t.Fatalf("want ZOMBIE_LIST reason, got %v", r.Reasons)
	}
	if strings.Contains(joined, "shut off 31") || strings.Contains(joined, "31 days") {
		t.Fatalf("must not fabricate day count from zombie list: %v", r.Reasons)
	}
}

func assertIntermittentBurst(t *testing.T, r Result, phrasePart, windowPart string, peak float64) {
	t.Helper()
	if r.Class != ClassIntermittent {
		t.Fatalf("want intermittent, got %s raw=%.1f reasons=%v", r.Class, r.RawIdleScore, r.Reasons)
	}
	if !r.ScoreHidden || r.IdleScore >= 0 {
		t.Fatalf("headline must be hidden for intermittent: hidden=%v score=%.1f", r.ScoreHidden, r.IdleScore)
	}
	if r.RawIdleScore < IdleClassMinScore {
		// batch VMs are quiet most hours so raw is high; require preserved raw
		t.Logf("raw_idle_score=%.1f (may be high)", r.RawIdleScore)
	}
	if r.RawIdleScore <= 0 {
		t.Fatalf("raw_idle_score must be preserved, got %.1f", r.RawIdleScore)
	}
	if r.Suggestion != suggestionKeepVerify {
		t.Fatalf("suggestion=%q want %q", r.Suggestion, suggestionKeepVerify)
	}
	if suggestionForbiddenForNonIdle(r.Suggestion) {
		t.Fatalf("forbidden suggestion %q", r.Suggestion)
	}
	joined := strings.Join(r.Reasons, ";")
	if !strings.Contains(joined, "BURST:") {
		t.Fatalf("want BURST reason, got %v", r.Reasons)
	}
	if !strings.Contains(joined, phrasePart) {
		t.Fatalf("want %q in reasons %v", phrasePart, r.Reasons)
	}
	if !strings.Contains(joined, windowPart) {
		t.Fatalf("want window %q in reasons %v", windowPart, r.Reasons)
	}
	peakStr := fmt.Sprintf("%.0f", peak)
	if !strings.Contains(joined, peakStr) {
		t.Fatalf("want peak %s in reasons %v", peakStr, r.Reasons)
	}
}

func TestNonIdleNeverDownsizeOrRelease(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, loc)
	cases := []struct {
		name string
		fill func(d, h int) (float64, float64, float64, float64)
		want Class
	}{
		{"scheduled", func(d, h int) (float64, float64, float64, float64) {
			day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
			if day.Weekday() >= time.Monday && day.Weekday() <= time.Friday {
				if (h >= 9 && h < 11) || (h >= 13 && h < 17) {
					return 35, 200, 300, 30
				}
			}
			return 0.4, 0.2, 0.2, 0.2
		}, ClassScheduled},
		{"active", func(d, h int) (float64, float64, float64, float64) {
			return 40, 100, 100, 20
		}, ClassActive},
		{"burst", func(d, h int) (float64, float64, float64, float64) {
			if d == 8 && (h == 3 || h == 4) {
				return 95, 8, 8, 1
			}
			return 0.3, 0.1, 0.1, 0
		}, ClassIntermittent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := synthHours(loc, 14, tc.fill)
			f := ExtractFeatures(baseVM("running"), b, cfg, loc)
			r := ScoreAndClassify(f, cfg, now, loc)
			if r.Class != tc.want {
				t.Fatalf("class=%s want %s", r.Class, tc.want)
			}
			if suggestionForbiddenForNonIdle(r.Suggestion) {
				t.Fatalf("class %s suggestion %q forbids downsize/release", r.Class, r.Suggestion)
			}
			if r.Class != ClassIdle && r.Class != ClassShutoffIdle {
				if !r.ScoreHidden {
					t.Fatalf("non-idle class %s must hide headline score", r.Class)
				}
			}
		})
	}
}
