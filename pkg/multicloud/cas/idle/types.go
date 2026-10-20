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
	"time"
)

// Class is the idle/usage verdict for one VM.
type Class string

const (
	ClassIdle         Class = "闲置"
	ClassScheduled    Class = "定时使用"
	ClassIntermittent Class = "间歇使用"
	ClassActive       Class = "活跃"
	ClassInsufficient Class = "数据不足"
	ClassShutoffIdle  Class = "闲置(关机)"
	ClassSkipped      Class = "跳过"
)

// ConfidenceLabel is 高/中/低.
type ConfidenceLabel string

const (
	ConfHigh   ConfidenceLabel = "高"
	ConfMedium ConfidenceLabel = "中"
	ConfLow    ConfidenceLabel = "低"
)

// HourlyPoint is one hour bucket for one metric series.
//
// D9 / live-CAS: Value comes from the single reports EChartValue.rate
// field. Whether that rate is the hourly average or the hourly max is
// UNVERIFIED — peaks (CpuMax, burst 峰值) are therefore peaks of that
// single series, not a confirmed maxMerge. Calibrate on a live CAS before
// treating p95/peak as true hourly maxima.
type HourlyPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

// Series is a named hourly series (cpu, mem, net_rx, ...).
type Series struct {
	Name   string
	Points []HourlyPoint
}

// VmInventory is the static inventory row used by the analyzer.
type VmInventory struct {
	Id           int64     `json:"id"`
	Uuid         string    `json:"uuid"`
	Name         string    `json:"name"`
	Title        string    `json:"title"`
	Status       string    `json:"status"` // running / shutOff / ...
	HostId       int64     `json:"host_id"`
	HostPoolId   int64     `json:"host_pool_id"`
	CpuRate      float64   `json:"cpu_rate"`
	MemRate      float64   `json:"mem_rate"`
	UptimeMin    int64     `json:"uptime_min"` // -1 unknown
	CreateDate   time.Time `json:"create_date"`
	LastOff      time.Time `json:"last_off"` // zero if unknown
	Castools     string    `json:"castools"`
	OnZombieList bool      `json:"on_zombie_list"` // from oneBtn/zombieVm; not a fabricated lastOff
}

// VmFeatures holds extracted hourly features for scoring/classification.
type VmFeatures struct {
	Vm VmInventory

	CpuAvg  float64
	CpuP95  float64 // of hourly max/rate
	CpuMax  float64
	NetP95  float64 // KB/s rx+tx
	NetMax  float64
	DiskP95 float64 // KB/s r+w
	DiskMax float64
	IopsP95 float64
	IopsMax float64
	ConnMax float64 // flowLink if present; -1 if n/a

	Coverage float64 // 0-1 of expected hourly buckets
	HasCpu   bool
	HasConn  bool
	HasIops  bool
	HasNet   bool
	HasDisk  bool

	DroppedBuckets int // unparseable report bucket labels

	// Active[i] / HourCPU[i] align with Hours[i] (HourCPU is 0 when no CPU point).
	Hours   []time.Time
	Active  []bool
	HourCPU []float64

	LastActive time.Time

	// spike marks calendar days that participate in a same-hour spike
	// seen on ≥3 distinct days (spec §6.2).
	spike       map[string]bool
	spikeByHour map[int]map[string]bool // hour-of-day → days
}

// Reason is one machine-coded explanation line.
type Reason struct {
	Code string
	Text string // Chinese, includes the code prefix
}

// Result is the per-VM idle report row.
type Result struct {
	Name   string `json:"name"`
	Uuid   string `json:"uuid"`
	Status string `json:"status"`
	Class  Class  `json:"class"`
	// IdleScore is the headline score shown next to CLASS.
	// See applyHeadlineScore / IdleClassMinScore for the display rule.
	// When ScoreHidden is true, table/JSON consumers should render IdleScore as "-".
	IdleScore       float64         `json:"idle_score"`
	RawIdleScore    float64         `json:"raw_idle_score"`
	ScoreHidden     bool            `json:"score_hidden"`
	Confidence      ConfidenceLabel `json:"confidence"`
	ConfidenceValue float64         `json:"confidence_value"`
	OffDays         float64         `json:"off_days"`
	CpuAvg          float64         `json:"cpu_avg"`
	CpuP95          float64         `json:"cpu_p95"`
	NetP95          float64         `json:"net_p95_kbps"`
	DiskP95         float64         `json:"disk_p95_kbps"`
	IopsP95         float64         `json:"iops_p95"`
	ConnMax         float64         `json:"conn_max"`
	ActiveRatio     float64         `json:"active_ratio"`
	InWinRatio      float64         `json:"in_win_ratio"`
	OutWinRatio     float64         `json:"out_win_ratio"`
	DetectedWindow  string          `json:"detected_window"`
	LastActive      string          `json:"last_active"`
	Suggestion      string          `json:"suggestion"`
	Reasons         []string        `json:"reasons"`
	DataGaps        []string        `json:"data_gaps"`
	Veto            bool            `json:"veto"`
}

// Config holds all tunables. Zero values are filled by DefaultConfig().
type Config struct {
	Lookback       time.Duration
	Timezone       string
	CasTimezone    string // how report bucket labels are interpreted
	WorkHours      string // "08:30-11:30,13:00-18:00"
	Workdays       string // "mon-fri"
	HolidayFile    string // optional JSON path; empty = off
	OffDays        float64
	MinAge         time.Duration
	MinCoverage    float64
	CpuAvg         float64
	CpuP95         float64
	NetP95KBps     float64
	DiskP95KBps    float64
	IopsP95        float64
	ActiveCPU      float64
	ActiveNetKBps  float64
	ActiveDiskKBps float64
	ActiveIops     float64
	ActiveConn     float64
	BaselineMult   float64
	PatternMinProb float64
	ScheduleMargin time.Duration
	WindowOverlap  float64
	NoPrefilter    bool
	PrefilterCPU   float64 // monthTopCpu threshold to mark ACTIVE early
	Signals        map[string]bool

	// Weights for idle_score (research §6.2).
	WeightCPU  float64
	WeightNet  float64
	WeightDisk float64
	WeightIops float64
	WeightConn float64
	WeightPPS  float64
	WeightProc float64

	// Unit assumptions for reports (UNVERIFIED; flip after calibration).
	ReportNetIsKBps  bool // if true, report net rate is KB/s
	ReportDiskIsKBps bool
}

func DefaultConfig() Config {
	return Config{
		Lookback:         14 * 24 * time.Hour,
		Timezone:         "Asia/Shanghai",
		CasTimezone:      "Asia/Shanghai",
		WorkHours:        "08:30-11:30,13:00-18:00",
		Workdays:         "mon-fri",
		OffDays:          30,
		MinAge:           14 * 24 * time.Hour,
		MinCoverage:      0.7,
		CpuAvg:           5,
		CpuP95:           10,
		NetP95KBps:       10,
		DiskP95KBps:      50,
		IopsP95:          5,
		ActiveCPU:        10,
		ActiveNetKBps:    50,
		ActiveDiskKBps:   200,
		ActiveIops:       20,
		ActiveConn:       5,
		BaselineMult:     3,
		PatternMinProb:   0.5,
		ScheduleMargin:   30 * time.Minute,
		WindowOverlap:    0.5,
		PrefilterCPU:     50,
		WeightCPU:        0.30,
		WeightNet:        0.20,
		WeightDisk:       0.15,
		WeightIops:       0.05,
		WeightConn:       0.15,
		WeightPPS:        0.05,
		WeightProc:       0.10,
		ReportNetIsKBps:  true,
		ReportDiskIsKBps: true,
		Signals: map[string]bool{
			"cpu": true, "net": true, "disk": true, "iops": true,
			// conn off until flowLink semantics verified (spec §6.2 / D3).
			"conn": false, "pps": false, "proc": false,
			// lifecycle stub does not count as enabled (D4).
			"lifecycle": false,
		},
	}
}
