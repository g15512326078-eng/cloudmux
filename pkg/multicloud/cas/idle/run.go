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
	"time"
)

// AnalyzeOne runs scoring on a pre-built feature set (fixture-friendly).
func AnalyzeOne(vm VmInventory, b HourlyBundle, cfg Config, now time.Time) (Result, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	if cfg.Lookback == 0 {
		cfg = DefaultConfig()
	}
	f := ExtractFeatures(vm, b, cfg, loc)
	return ScoreAndClassify(f, cfg, now, loc), nil
}

// FormatTable renders a simple ASCII table for CLI output.
// SCORE is the headline (hidden as "-" for non-idle classes).
// raw_idle_score is JSON-only (not a table column).
func FormatTable(rows []Result) string {
	if len(rows) == 0 {
		return "(no VMs)\n"
	}
	hdr := "NAME\tUUID\tSTATUS\tCLASS\tSCORE\tCONF\tCPU_AVG\tCPU_P95\tNET_P95\tWINDOW\tSUGGESTION\tREASONS\n"
	body := ""
	for _, r := range rows {
		reason := ""
		if len(r.Reasons) > 0 {
			reason = r.Reasons[0]
		}
		score := formatHeadlineScore(r)
		body += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%.1f\t%.1f\t%.1f\t%s\t%s\t%s\n",
			r.Name, short(r.Uuid, 8), r.Status, r.Class, score, r.Confidence,
			r.CpuAvg, r.CpuP95, r.NetP95, r.DetectedWindow, r.Suggestion, reason)
	}
	return hdr + body
}

func formatHeadlineScore(r Result) string {
	if r.ScoreHidden || r.IdleScore < 0 {
		return "-"
	}
	return fmt.Sprintf("%.0f", r.IdleScore)
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
