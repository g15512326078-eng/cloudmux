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
	"math"
	"time"
)

// HourlyBundle is the per-VM hourly series used for feature extraction.
type HourlyBundle struct {
	CPU  []HourlyPoint // prefer hourly max when available
	Mem  []HourlyPoint
	Net  []HourlyPoint // rx+tx KB/s
	Disk []HourlyPoint // r+w KB/s
	Iops []HourlyPoint
	Conn []HourlyPoint // flowLink; may be empty
}

// ExtractFeatures builds VmFeatures from inventory + hourly series.
func ExtractFeatures(vm VmInventory, b HourlyBundle, cfg Config, loc *time.Location) VmFeatures {
	f := VmFeatures{Vm: vm}
	cpuVals := values(b.CPU)
	f.CpuAvg, f.CpuP95, f.CpuMax = stats(cpuVals)
	// For CPU avg matching cloudpods SCALE_DOWN, also accept mean of points
	if len(cpuVals) > 0 {
		sum := 0.0
		for _, v := range cpuVals {
			sum += v
		}
		f.CpuAvg = sum / float64(len(cpuVals))
	}

	if len(b.Net) > 0 {
		f.HasNet = true
		f.NetP95, f.NetMax = p95max(values(b.Net))
	}
	if len(b.Disk) > 0 {
		f.HasDisk = true
		f.DiskP95, f.DiskMax = p95max(values(b.Disk))
	}
	if len(b.Iops) > 0 {
		f.HasIops = true
		f.IopsP95, _ = p95max(values(b.Iops))
		_, f.IopsMax = p95max(values(b.Iops))
	}
	if len(b.Conn) > 0 {
		f.HasConn = true
		_, f.ConnMax = p95max(values(b.Conn))
	} else {
		f.ConnMax = -1
	}

	// Align hours on CPU series (primary); fall back to net/disk
	base := b.CPU
	if len(base) == 0 {
		base = b.Net
	}
	if len(base) == 0 {
		base = b.Disk
	}
	index := map[int64]int{} // unix hour → idx
	for _, p := range base {
		h := p.Time.UTC().Truncate(time.Hour)
		index[h.Unix()] = len(f.Hours)
		f.Hours = append(f.Hours, h)
		f.Active = append(f.Active, false)
	}
	lookup := func(series []HourlyPoint, t time.Time) (float64, bool) {
		for _, p := range series {
			if p.Time.UTC().Truncate(time.Hour).Equal(t) {
				return p.Value, true
			}
		}
		return 0, false
	}

	// baseline p20 of CPU for adaptive threshold
	baseCPU := percentile(sortedCopy(cpuVals), 20)

	spikeDays := map[string]bool{}
	for i, h := range f.Hours {
		cpu, _ := lookup(b.CPU, h)
		net, _ := lookup(b.Net, h)
		disk, _ := lookup(b.Disk, h)
		iops, _ := lookup(b.Iops, h)
		conn, hasConn := lookup(b.Conn, h)

		active := cpu >= cfg.ActiveCPU ||
			net >= cfg.ActiveNetKBps ||
			disk >= cfg.ActiveDiskKBps ||
			iops >= cfg.ActiveIops ||
			(hasConn && conn >= cfg.ActiveConn)
		if cfg.BaselineMult > 0 && baseCPU > 0 {
			if cpu < cfg.BaselineMult*baseCPU && net < cfg.ActiveNetKBps && disk < cfg.ActiveDiskKBps {
				// below adaptive baseline and quiet net/disk → not active
				if cpu < cfg.ActiveCPU {
					active = active && (net >= cfg.ActiveNetKBps || disk >= cfg.ActiveDiskKBps || iops >= cfg.ActiveIops)
				}
			}
		}
		f.Active[i] = active
		if active {
			f.LastActive = h
		}
		// spike veto day markers
		if cpu >= 50 || net >= 1024 { // 1 MB/s
			spikeDays[h.In(loc).Format("2006-01-02")] = true
		}
		_ = index
	}
	days := make([]string, 0, len(spikeDays))
	for d := range spikeDays {
		days = append(days, d)
	}
	f.SetSpikeDays(days)

	expected := int(cfg.Lookback.Hours())
	if expected <= 0 {
		expected = 14 * 24
	}
	if expected > 0 {
		f.Coverage = math.Min(1, float64(len(f.Hours))/float64(expected))
	}
	return f
}

func values(pts []HourlyPoint) []float64 {
	out := make([]float64, len(pts))
	for i := range pts {
		out[i] = pts[i].Value
	}
	return out
}

func sortedCopy(v []float64) []float64 {
	out := append([]float64{}, v...)
	// inline sort
	for i := 1; i < len(out); i++ {
		j := i
		for j > 0 && out[j-1] > out[j] {
			out[j-1], out[j] = out[j], out[j-1]
			j--
		}
	}
	return out
}

func p95max(vals []float64) (p95, max float64) {
	_, p95, max = stats(vals)
	return
}
