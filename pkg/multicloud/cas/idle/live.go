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
	"net/url"
	"strconv"
	"strings"
	"time"

	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/timeutils"

	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

// AnalyzeLive walks inventory + reports (read-only) and scores every VM.
// cli must already be a ReadOnlyClone. Richer signals (oper logs, snapshots,
// processList) are best-effort and never fail the scan.
func AnalyzeLive(cli *cas.SCasClient, cfg Config, now time.Time) ([]Result, error) {
	if cli == nil {
		return nil, errors.Wrap(errors.ErrEmpty, "nil CAS client")
	}
	cli = cli.ReadOnlyClone()
	if cfg.Lookback == 0 {
		cfg = DefaultConfig()
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	casLoc, err := time.LoadLocation(cfg.CasTimezone)
	if err != nil {
		casLoc = loc
	}

	ins, err := cli.GetInstances(url.Values{})
	if err != nil {
		return nil, errors.Wrap(err, "vmList")
	}

	// Prefilter: monthTopCpu ≥ PrefilterCPU → mark ACTIVE early.
	preBusy := map[string]bool{}
	if !cfg.NoPrefilter {
		if details, e := cli.GetVmResourceDetails(); e == nil {
			for _, d := range details {
				if d.MonthTopCpu >= cfg.PrefilterCPU {
					key := d.Uuid
					if key == "" {
						key = strconv.FormatInt(d.Id, 10)
					}
					preBusy[key] = true
				}
			}
		} else {
			log.Debugf("cas idle: vmResourceDetail skipped: %v", e)
		}
	}

	zombieUUIDs := map[string]bool{}
	if zoms, e := cli.GetZombieVms(int(cfg.OffDays)); e == nil {
		for _, z := range zoms {
			if z.Uuid != "" {
				zombieUUIDs[z.Uuid] = true
			}
		}
	} else {
		log.Debugf("cas idle: zombieVm skipped: %v", e)
	}

	start := now.Add(-cfg.Lookback)
	rows := make([]Result, 0, len(ins))
	for i := range ins {
		vm := inventoryFromInstance(&ins[i], loc)
		if zombieUUIDs[vm.Uuid] {
			vm.OnZombieList = true
		}
		// Fetch real lastOffTime for shut-off VMs when list row lacks it.
		if vm.Status == "shutOff" && vm.LastOff.IsZero() && vm.Id > 0 {
			if t, e := cli.GetVmLastOffTime(vm.Id); e == nil && !t.IsZero() {
				vm.LastOff = t
			} else if e != nil {
				log.Debugf("cas idle: summary/new lastOffTime domain %d: %v", vm.Id, e)
			}
		}

		if preBusy[vm.Uuid] || preBusy[strconv.FormatInt(vm.Id, 10)] {
			r := Result{
				Name: firstNonEmpty(vm.Title, vm.Name), Uuid: vm.Uuid, Status: vm.Status,
				Class: ClassActive, IdleScore: 0, Confidence: ConfMedium, ConfidenceValue: 0.6,
				Suggestion: "keep",
				Reasons:    []string{fmt.Sprintf("PREFILTER: oneBtn/vmResourceDetail monthTopCpu ≥ %.0f%%", cfg.PrefilterCPU)},
			}
			rows = append(rows, r)
			continue
		}

		b, ferr := fetchHourlyBundle(cli, vm.Id, start, now, casLoc, cfg)
		if ferr != nil {
			log.Debugf("cas idle: reports for domain %d: %v", vm.Id, ferr)
		}
		// lifecycle best-effort (read-only GETs)
		applyLifecycleSignals(cli, &vm, cfg, now)

		f := ExtractFeatures(vm, b, cfg, loc)
		r := ScoreAndClassify(f, cfg, now, loc)
		rows = append(rows, r)
	}
	return rows, nil
}

func inventoryFromInstance(ins *cas.SInstance, loc *time.Location) VmInventory {
	vm := VmInventory{
		Id:         ins.Id,
		Uuid:       ins.Uuid,
		Name:       ins.Name,
		Title:      firstNonEmpty(ins.Title, ins.Name),
		Status:     ins.VmStatus,
		HostId:     ins.HostId,
		HostPoolId: ins.HostPoolId,
		CpuRate:    ins.CpuRate,
		MemRate:    ins.MemRate,
	}
	if t, e := timeutils.ParseTimeStr(ins.CreateDate); e == nil {
		vm.CreateDate = t
	}
	if ins.LastOffTime > 0 {
		// Reuse monitor epoch helper via local magnitude rule (seconds vs ms).
		if ins.LastOffTime < 1_000_000_000_000 {
			vm.LastOff = time.Unix(ins.LastOffTime, 0)
		} else {
			vm.LastOff = time.UnixMilli(ins.LastOffTime)
		}
	}
	return vm
}

func fetchHourlyBundle(cli *cas.SCasClient, domainId int64, start, end time.Time, casLoc *time.Location, cfg Config) (HourlyBundle, error) {
	b := HourlyBundle{}
	if domainId <= 0 {
		return b, errors.Wrap(errors.ErrEmpty, "domainId")
	}
	parse := func(kind string, flow int) ([]HourlyPoint, error) {
		raw, err := cli.GetReportVmSeries(kind, domainId, flow, start, end)
		if err != nil {
			return nil, err
		}
		pts := make([]HourlyPoint, 0, len(raw))
		for _, ev := range raw {
			t, ok := parseBucketTime(ev.Name, casLoc)
			if !ok {
				b.DroppedBuckets++
				continue
			}
			pts = append(pts, HourlyPoint{Time: t, Value: ev.Rate})
		}
		return pts, nil
	}

	if cfg.Signals["cpu"] {
		if pts, err := parse("cpuMem", cas.ReportFlowCPU); err == nil {
			b.CPU = pts
		} else {
			return b, err
		}
	}
	if cfg.Signals["net"] {
		rd, err1 := parse("netSp", cas.ReportFlowNetRead)
		wr, err2 := parse("netSp", cas.ReportFlowNetWrite)
		if err1 == nil && err2 == nil {
			b.Net = sumSeries(rd, wr)
			if !cfg.ReportNetIsKBps {
				// UNVERIFIED: if reports are already B/s, convert to KB/s for scorer
				for i := range b.Net {
					b.Net[i].Value /= 1024
				}
			}
		}
	}
	if cfg.Signals["disk"] {
		rd, err1 := parse("disk", cas.ReportFlowDiskRead)
		wr, err2 := parse("disk", cas.ReportFlowDiskWrite)
		if err1 == nil && err2 == nil {
			b.Disk = sumSeries(rd, wr)
			if !cfg.ReportDiskIsKBps {
				for i := range b.Disk {
					b.Disk[i].Value /= 1024
				}
			}
		}
	}
	if cfg.Signals["iops"] {
		// reports/vm/io — UNVERIFIED whether rate is combined IOPS or throughput
		if pts, err := parse("io", 0); err == nil {
			b.Iops = pts
		}
	}
	return b, nil
}

// parseBucketTime accepts "2006-01-02 15", "2006-01-02 15:00", epoch s/ms.
func parseBucketTime(name string, loc *time.Location) (time.Time, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.Time{}, false
	}
	if n, err := strconv.ParseInt(name, 10, 64); err == nil {
		if n > 1e12 { // ms
			return time.UnixMilli(n).In(loc), true
		}
		return time.Unix(n, 0).In(loc), true
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, name, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func sumSeries(a, b []HourlyPoint) []HourlyPoint {
	m := map[int64]float64{}
	times := []time.Time{}
	add := func(pts []HourlyPoint) {
		for _, p := range pts {
			h := p.Time.UTC().Truncate(time.Hour)
			k := h.Unix()
			if _, ok := m[k]; !ok {
				times = append(times, h)
			}
			m[k] += p.Value
		}
	}
	add(a)
	add(b)
	out := make([]HourlyPoint, 0, len(times))
	for _, t := range times {
		out = append(out, HourlyPoint{Time: t, Value: m[t.Unix()]})
	}
	return out
}

// applyLifecycleSignals optionally pulls snapshot / oper-log recency.
// Failures are ignored. Never fetches VNC or backup Password fields into logs
// (redact already covers Password); we simply do not call those endpoints here.
func applyLifecycleSignals(cli *cas.SCasClient, vm *VmInventory, cfg Config, now time.Time) {
	if !cfg.Signals["lifecycle"] || vm.Id <= 0 {
		return
	}
	// snapshot list: GET vm/snapshot/{id} — presence of recent creationTime
	// is recorded via log only today; ScoreAndClassify reads Neg* from
	// VmFeatures when we extend it. For now this is a no-op hook so the
	// read-only path is exercised in tests once fixtures cover it.
	_ = cli
	_ = now
}
