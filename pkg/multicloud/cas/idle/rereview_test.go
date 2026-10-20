// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package idle

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Re-review @ ff55981a: R1 (net/disk peaks), R2 (release gate), N-b, N-c.

var rrNow = time.Date(2026, 3, 16, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

func sundayBackup(loc *time.Location, net, disk float64) HourlyBundle {
	return synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
		if day.Weekday() == time.Sunday && h >= 2 && h < 4 {
			return 0.5, net, disk, 1
		}
		return 0.4, 0.1, 0.1, 0
	})
}

func hasGap(r Result, g string) bool {
	for _, x := range r.DataGaps {
		if x == g {
			return true
		}
	}
	return false
}

func TestR1WeeklyNetOnlyBackupNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	f := ExtractFeatures(baseVM("running"), sundayBackup(loc, 5*1024, 0.1), cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIntermittent {
		t.Fatalf("net-only weekly backup: class=%s reasons=%v", r.Class, r.Reasons)
	}
	joined := strings.Join(r.Reasons, ";")
	if !strings.Contains(joined, "BURST: 每周日 02-04 网络 5 MB/s") {
		t.Fatalf("want net burst reason, got %v", r.Reasons)
	}
	if r.Suggestion != suggestionKeepVerify || !r.ScoreHidden {
		t.Fatalf("suggestion=%q hidden=%v", r.Suggestion, r.ScoreHidden)
	}
}

func TestR1WeeklyDiskOnlyBackupNotIdle(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	f := ExtractFeatures(baseVM("running"), sundayBackup(loc, 0.1, 20*1024), cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIntermittent {
		t.Fatalf("disk-only weekly backup: class=%s reasons=%v", r.Class, r.Reasons)
	}
	joined := strings.Join(r.Reasons, ";")
	if !strings.Contains(joined, "BURST: 每周日 02-04 磁盘 20 MB/s") {
		t.Fatalf("want disk burst reason, got %v", r.Reasons)
	}
	if r.Suggestion != suggestionKeepVerify {
		t.Fatalf("suggestion=%q", r.Suggestion)
	}
}

func TestR1SingleNetBurstNamesSignal(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		if d == 8 && (h == 3 || h == 4) {
			return 0.5, 5 * 1024, 0.1, 1
		}
		return 0.3, 0.1, 0.1, 0
	})
	r := ScoreAndClassify(ExtractFeatures(baseVM("running"), b, cfg, loc), cfg, rrNow, loc)
	if r.Class != ClassIntermittent {
		t.Fatalf("class=%s reasons=%v", r.Class, r.Reasons)
	}
	if j := strings.Join(r.Reasons, ";"); !strings.Contains(j, "单次 2 小时突发 峰值 网络 5 MB/s") {
		t.Fatalf("reasons=%v", r.Reasons)
	}
}

func TestR1DiskSpikeVetoSameHour(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		if (d == 0 || d == 3 || d == 6) && h == 1 {
			return 0.5, 0.1, 6 * 1024, 1
		}
		return 0.4, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if !r.Veto {
		t.Fatalf("disk ≥ SpikeDiskKBps on 3 days at 01:00 must veto; class=%s", r.Class)
	}
	if r.Class == ClassIdle {
		t.Fatal("veto left 闲置")
	}
}

func TestR1ThresholdsConfigurable(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	// Raise both net gates above the 5 MB/s backup: it is then below every
	// gate and the VM may be 闲置 again — proves the gates come from Config.
	cfg.IdlePeakNetKBps = 10 * 1024
	cfg.SpikeNetKBps = 10 * 1024
	f := ExtractFeatures(baseVM("running"), sundayBackup(loc, 5*1024, 0.1), cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIdle {
		t.Fatalf("with raised net gate want 闲置, got %s %v", r.Class, r.Reasons)
	}
	// Lowering the disk gate below a 300 KB/s hour flips it back.
	cfg = DefaultConfig()
	cfg.IdlePeakDiskKBps = 256
	f = ExtractFeatures(baseVM("running"), sundayBackup(loc, 0.1, 300), cfg, loc)
	r = ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIntermittent {
		t.Fatalf("with lowered disk gate want 间歇使用, got %s", r.Class)
	}
	if j := strings.Join(r.Reasons, ";"); !strings.Contains(j, "磁盘 300 KB/s") {
		t.Fatalf("reasons=%v", r.Reasons)
	}
}

func TestFormatKBps(t *testing.T) {
	cases := map[float64]string{300: "300 KB/s", 1024: "1 MB/s", 5120: "5 MB/s", 1536: "1.5 MB/s", 20480: "20 MB/s"}
	for in, want := range cases {
		if got := formatKBps(in); got != want {
			t.Errorf("formatKBps(%v)=%q want %q", in, got, want)
		}
	}
}

func TestNbDailyCronLabelledDaily(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		if h == 3 {
			return 60, 0.1, 0.1, 1
		}
		return 0.4, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	got := describeBurst(f, cfg, loc)
	if got != "每天 03-04 负载 60%" {
		t.Fatalf("daily cron label=%q", got)
	}
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if j := strings.Join(r.Reasons, ";"); strings.Contains(j, "每周") {
		t.Fatalf("daily cron must not read 每周X: %v", r.Reasons)
	}
}

func TestNbWorkdayEveningLabel(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) {
		day := time.Date(2026, 3, 2+d, 0, 0, 0, 0, loc)
		if day.Weekday() >= time.Monday && day.Weekday() <= time.Friday && h >= 19 && h < 23 {
			return 40, 0.1, 0.1, 1
		}
		return 0.4, 0.1, 0.1, 0
	})
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	if got := describeBurst(f, cfg, loc); got != "工作日 19-23 负载 40%" {
		t.Fatalf("workday evening label=%q", got)
	}
}

func TestR2TrulyIdleFullCoverageRelease(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) { return 0.5, 0.1, 0.1, 0.1 })
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	if !hasFullSignalCoverage(f, cfg) {
		t.Fatalf("expected full coverage: cov=%.2f", f.Coverage)
	}
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIdle || r.Confidence != ConfHigh || r.Suggestion != suggestionRelease {
		t.Fatalf("class=%s conf=%s(%.2f) suggestion=%q", r.Class, r.Confidence, r.ConfidenceValue, r.Suggestion)
	}
}

func assertVerify(t *testing.T, r Result) {
	t.Helper()
	if r.Suggestion != suggestionVerify {
		t.Fatalf("want %q, got %q (class=%s conf=%s %.2f reasons=%v)",
			suggestionVerify, r.Suggestion, r.Class, r.Confidence, r.ConfidenceValue, r.Reasons)
	}
	if !strings.Contains(strings.Join(r.Reasons, ";"), "RELEASE_GATE:") {
		t.Fatalf("want RELEASE_GATE reason: %v", r.Reasons)
	}
}

func TestR2CpuOnlyIdleGetsVerify(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) { return 0.5, 0, 0, 0 })
	b.Net, b.Disk, b.Iops = nil, nil, nil
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIdle {
		t.Fatalf("class=%s", r.Class)
	}
	assertVerify(t, r)
	for _, g := range []string{"net=n/a", "disk=n/a", "iops=n/a"} {
		if !hasGap(r, g) {
			t.Errorf("missing gap %s in %v", g, r.DataGaps)
		}
	}
}

func TestR2HighConfidenceButNotFullGetsVerify(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) { return 0.5, 0.1, 0.1, 0.1 })
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	f.DroppedBuckets = 2
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if r.Class != ClassIdle || r.Confidence != ConfHigh {
		t.Fatalf("precondition: class=%s conf=%s", r.Class, r.Confidence)
	}
	assertVerify(t, r)
}

func TestR2ZombieListOnlyGetsVerify(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	vm := baseVM("shutOff")
	vm.OnZombieList = true
	r := ScoreAndClassify(VmFeatures{Vm: vm, Coverage: 1}, cfg, rrNow, loc)
	if r.Class != ClassShutoffIdle {
		t.Fatalf("class=%s", r.Class)
	}
	assertVerify(t, r)
}

func TestR2ShutoffRealLastOffRelease(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	vm := baseVM("shutOff")
	vm.LastOff = time.Date(2026, 1, 1, 10, 0, 0, 0, loc)
	r := ScoreAndClassify(VmFeatures{Vm: vm, Coverage: 1}, cfg, rrNow, loc)
	if r.Class != ClassShutoffIdle || r.Confidence != ConfHigh || r.Suggestion != suggestionRelease {
		t.Fatalf("class=%s conf=%s suggestion=%q", r.Class, r.Confidence, r.Suggestion)
	}
}

func TestNcStubGapsLowerConfidence(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) { return 0.5, 0.1, 0.1, 0.1 })
	f := ExtractFeatures(baseVM("running"), b, cfg, loc)
	r := ScoreAndClassify(f, cfg, rrNow, loc)
	if !hasGap(r, "lifecycle=n/a") || !hasGap(r, "proc=n/a") {
		t.Fatalf("data_gaps=%v", r.DataGaps)
	}
	noStub := cfg
	noStub.LifecycleStubFactor = 1
	r1 := ScoreAndClassify(f, noStub, rrNow, loc)
	if !(r.ConfidenceValue < r1.ConfidenceValue) {
		t.Fatalf("lifecycle stub must lower confidence: %.3f vs %.3f", r.ConfidenceValue, r1.ConfidenceValue)
	}
}

func TestNcHolidayFileWarningInDataGaps(t *testing.T) {
	loc := shLoc(t)
	cfg := DefaultConfig()
	cfg.HolidayFile = filepath.Join(t.TempDir(), "missing.json")
	b := synthHours(loc, 14, func(d, h int) (float64, float64, float64, float64) { return 0.5, 0.1, 0.1, 0.1 })
	r := ScoreAndClassify(ExtractFeatures(baseVM("running"), b, cfg, loc), cfg, rrNow, loc)
	if !hasGap(r, "holiday_file=unreadable") {
		t.Fatalf("data_gaps=%v", r.DataGaps)
	}
}
