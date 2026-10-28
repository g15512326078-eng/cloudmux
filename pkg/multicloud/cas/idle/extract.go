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
// Prefer hourly *max* when the API exposes a separate max; otherwise
// the single reports rate field is used. Whether that field is avg or max
// is UNVERIFIED — CpuMax/p95 are peaks of that series only.
type HourlyBundle struct {
	CPU  []HourlyPoint
	Mem  []HourlyPoint
	Net  []HourlyPoint // rx+tx KB/s
	Disk []HourlyPoint // r+w KB/s
	Iops []HourlyPoint
	Conn []HourlyPoint // flowLink; may be empty

	// DroppedBuckets counts report points whose time label failed to parse.
	DroppedBuckets int
}

// ExtractFeatures builds VmFeatures from inventory + hourly series.
func ExtractFeatures(vm VmInventory, b HourlyBundle, cfg Config, loc *time.Location) VmFeatures {
	f := VmFeatures{Vm: vm, DroppedBuckets: b.DroppedBuckets}

	if len(b.CPU) > 0 {
		f.HasCpu = true
		cpuVals := values(b.CPU)
		f.CpuAvg, f.CpuP95, f.CpuMax = stats(cpuVals)
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
		f.IopsP95, f.IopsMax = p95max(values(b.Iops))
	}
	if len(b.Conn) > 0 {
		f.HasConn = true
		_, f.ConnMax = p95max(values(b.Conn))
	} else {
		f.ConnMax = -1
	}

	// Align on the union of all series hours.
	index := map[int64]int{}
	addHour := func(pts []HourlyPoint) {
		for _, p := range pts {
			h := p.Time.UTC().Truncate(time.Hour)
			k := h.Unix()
			if _, ok := index[k]; ok {
				continue
			}
			index[k] = len(f.Hours)
			f.Hours = append(f.Hours, h)
			f.Active = append(f.Active, false)
			f.HourCPU = append(f.HourCPU, 0)
			f.HourNet = append(f.HourNet, 0)
			f.HourDisk = append(f.HourDisk, 0)
		}
	}
	addHour(b.CPU)
	addHour(b.Net)
	addHour(b.Disk)
	addHour(b.Iops)
	addHour(b.Conn)

	lookup := func(series []HourlyPoint, t time.Time) (float64, bool) {
		for _, p := range series {
			if p.Time.UTC().Truncate(time.Hour).Equal(t) {
				return p.Value, true
			}
		}
		return 0, false
	}

	baseCPU := 0.0
	if f.HasCpu {
		baseCPU = percentile(sortedCopy(values(b.CPU)), 20)
	}

	// Same clock-hour recurring spikes: hour-of-day → set of calendar days.
	spikeByHour := map[int]map[string]bool{}
	for i, h := range f.Hours {
		cpu, hasCpu := lookup(b.CPU, h)
		net, _ := lookup(b.Net, h)
		disk, _ := lookup(b.Disk, h)
		iops, _ := lookup(b.Iops, h)
		conn, hasConn := lookup(b.Conn, h)
		if hasCpu {
			f.HourCPU[i] = cpu
		}
		f.HourNet[i] = net
		f.HourDisk[i] = disk

		active := (hasCpu && cpu >= cfg.ActiveCPU) ||
			net >= cfg.ActiveNetKBps ||
			disk >= cfg.ActiveDiskKBps ||
			iops >= cfg.ActiveIops ||
			(hasConn && conn >= cfg.ActiveConn)
		if cfg.BaselineMult > 0 && baseCPU > 0 && hasCpu {
			if cpu < cfg.BaselineMult*baseCPU && net < cfg.ActiveNetKBps && disk < cfg.ActiveDiskKBps {
				if cpu < cfg.ActiveCPU {
					active = active && (net >= cfg.ActiveNetKBps || disk >= cfg.ActiveDiskKBps || iops >= cfg.ActiveIops)
				}
			}
		}
		f.Active[i] = active
		if active {
			f.LastActive = h
		}
		local := h.In(loc)
		if (hasCpu && cpu >= cfg.SpikeCPU) || net >= cfg.SpikeNetKBps || disk >= cfg.SpikeDiskKBps {
			hod := local.Hour()
			day := local.Format("2006-01-02")
			if spikeByHour[hod] == nil {
				spikeByHour[hod] = map[string]bool{}
			}
			spikeByHour[hod][day] = true
		}
	}

	// Record days that participate in a same-hour spike seen on ≥1 day;
	// veto logic requires ≥3 days for the *same* hour.
	days := []string{}
	seenDay := map[string]bool{}
	for _, dm := range spikeByHour {
		if len(dm) >= 3 {
			for d := range dm {
				if !seenDay[d] {
					seenDay[d] = true
					days = append(days, d)
				}
			}
		}
	}
	f.SetSpikeDays(days)
	f.spikeByHour = spikeByHour

	expected := int(cfg.Lookback.Hours())
	if expected <= 0 {
		expected = 14 * 24
	}
	if expected > 0 {
		f.Coverage = math.Min(1, float64(len(f.Hours))/float64(expected))
	}
	// Missing CPU does not zero-fill: HasCpu stays false and completeness
	// / data_gaps handle it in ScoreAndClassify (no fake 0%).
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
