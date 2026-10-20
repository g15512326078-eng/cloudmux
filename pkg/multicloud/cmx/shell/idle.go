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

package shell

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas/idle"
	casprovider "yunion.io/x/cloudmux/pkg/multicloud/cas/provider"
)

type InstanceIdleListOptions struct {
	Lookback       string  `help:"Lookback window" default:"14d"`
	Tz             string  `help:"Display timezone" default:"Asia/Shanghai"`
	CasTz          string  `help:"CAS report bucket timezone" default:"Asia/Shanghai"`
	WorkHours      string  `help:"Business hours" default:"08:30-11:30,13:00-18:00"`
	Workdays       string  `help:"Workdays" default:"mon-fri"`
	HolidayFile    string  `help:"Optional holiday JSON (date→holiday|workday)"`
	OffDays        float64 `help:"Shut-off days for 闲置(关机)" default:"30"`
	MinAge         string  `help:"Skip VMs younger than this" default:"14d"`
	MinCoverage    float64 `help:"Min hourly coverage 0-1" default:"0.7"`
	CpuAvg         float64 `help:"CPU avg threshold %" default:"5"`
	CpuP95         float64 `help:"CPU hourly p95 %" default:"10"`
	NetP95         float64 `help:"Net p95 KB/s" default:"10"`
	DiskP95        float64 `help:"Disk p95 KB/s" default:"50"`
	IopsP95        float64 `help:"IOPS p95" default:"5"`
	ActiveCpu      float64 `help:"Active CPU %" default:"10"`
	ActiveNet      float64 `help:"Active net KB/s" default:"50"`
	ActiveDisk     float64 `help:"Active disk KB/s" default:"200"`
	ActiveIops     float64 `help:"Active IOPS" default:"20"`
	BaselineMult   float64 `help:"Adaptive baseline multiplier" default:"3"`
	PatternMinProb float64 `help:"Min hour probability for window" default:"0.5"`
	ScheduleMargin string  `help:"Schedule suggestion margin" default:"30m"`
	NoPrefilter    bool    `help:"Disable oneBtn prefilter"`
	Output         string  `help:"table|json" default:"table"`
	Fixture        string  `help:"JSON fixture path (offline; no credentials needed)"`
}

func init() {
	cmd := NewCommand("instance-idle")
	NewCO[InstanceIdleListOptions](cmd).RawRun("list", "Detect idle/scheduled/intermittent CAS VMs (read-only; peaks from report rate field — avg/max UNVERIFIED, see idle/README D9)", func(cli ICloudProvider, args *InstanceIdleListOptions) error {
		cfg, err := idleOptionsToConfig(args)
		if err != nil {
			return err
		}
		var rows []idle.Result
		if len(args.Fixture) > 0 {
			rows, err = analyzeIdleFixture(args.Fixture, cfg)
		} else {
			if cli == nil || cli.GetProvider() == nil {
				return errors.Wrap(cloudprovider.ErrMissingParameter, "live scan needs credentials (or pass --fixture)")
			}
			p, ok := cli.GetProvider().(*casprovider.SCasProvider)
			if !ok {
				return errors.Wrap(cloudprovider.ErrNotSupported, "instance-idle-list requires CAS provider")
			}
			rows, err = idle.AnalyzeLive(p.GetClient().ReadOnlyClone(), cfg, time.Now())
		}
		if err != nil {
			return err
		}
		switch args.Output {
		case "json":
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(rows)
		default:
			fmt.Print(idle.FormatTable(rows))
			return nil
		}
	})
}

func idleOptionsToConfig(args *InstanceIdleListOptions) (idle.Config, error) {
	cfg := idle.DefaultConfig()
	var err error
	if cfg.Lookback, err = parseIdleDur(args.Lookback, cfg.Lookback); err != nil {
		return cfg, err
	}
	if cfg.MinAge, err = parseIdleDur(args.MinAge, cfg.MinAge); err != nil {
		return cfg, err
	}
	if cfg.ScheduleMargin, err = parseIdleDur(args.ScheduleMargin, cfg.ScheduleMargin); err != nil {
		return cfg, err
	}
	cfg.Timezone = args.Tz
	cfg.CasTimezone = args.CasTz
	cfg.WorkHours = args.WorkHours
	cfg.Workdays = args.Workdays
	cfg.HolidayFile = args.HolidayFile
	cfg.OffDays = args.OffDays
	cfg.MinCoverage = args.MinCoverage
	cfg.CpuAvg = args.CpuAvg
	cfg.CpuP95 = args.CpuP95
	cfg.NetP95KBps = args.NetP95
	cfg.DiskP95KBps = args.DiskP95
	cfg.IopsP95 = args.IopsP95
	cfg.ActiveCPU = args.ActiveCpu
	cfg.ActiveNetKBps = args.ActiveNet
	cfg.ActiveDiskKBps = args.ActiveDisk
	cfg.ActiveIops = args.ActiveIops
	cfg.BaselineMult = args.BaselineMult
	cfg.PatternMinProb = args.PatternMinProb
	cfg.NoPrefilter = args.NoPrefilter
	return cfg, nil
}

func parseIdleDur(s string, def time.Duration) (time.Duration, error) {
	if len(s) == 0 {
		return def, nil
	}
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}

type idleFixtureFile struct {
	Now string `json:"now"`
	VMs []struct {
		Vm   idle.VmInventory   `json:"vm"`
		CPU  []idle.HourlyPoint `json:"cpu"`
		Net  []idle.HourlyPoint `json:"net"`
		Disk []idle.HourlyPoint `json:"disk"`
		Iops []idle.HourlyPoint `json:"iops"`
		Conn []idle.HourlyPoint `json:"conn"`
	} `json:"vms"`
}

func analyzeIdleFixture(path string, cfg idle.Config) ([]idle.Result, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f idleFixtureFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	now := time.Now()
	if len(f.Now) > 0 {
		if t, e := time.Parse(time.RFC3339, f.Now); e == nil {
			now = t
		}
	}
	rows := []idle.Result{}
	for _, v := range f.VMs {
		b := idle.HourlyBundle{CPU: v.CPU, Net: v.Net, Disk: v.Disk, Iops: v.Iops, Conn: v.Conn}
		r, err := idle.AnalyzeOne(v.Vm, b, cfg, now)
		if err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}
