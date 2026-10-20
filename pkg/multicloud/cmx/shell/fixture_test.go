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
package shell

import (
	"strings"
	"testing"

	"yunion.io/x/cloudmux/pkg/multicloud/cas/idle"
)

// N-d: only the selected subcommand's own parsed --fixture flag counts.
func TestWantsOfflineFixture(t *testing.T) {
	if !WantsOfflineFixture(&InstanceIdleListOptions{Fixture: "x.json"}) {
		t.Fatal("instance-idle-list --fixture must be offline")
	}
	if WantsOfflineFixture(&InstanceIdleListOptions{}) {
		t.Fatal("no --fixture must not be offline")
	}
	// Other subcommands never trigger the offline provider, even if a
	// positional argument happens to be the literal "--fixture".
	type otherOpts struct{ ID string }
	if WantsOfflineFixture(&otherOpts{ID: "--fixture"}) {
		t.Fatal("other subcommand options must not be offline")
	}
	if WantsOfflineFixture(nil) {
		t.Fatal("nil options")
	}
}

// Demo fixture regression: the net-only and disk-only weekly backups (R1)
// must be 间歇使用 with a signal-named BURST reason, never 闲置/release.
func TestIdleDemoFixtureClasses(t *testing.T) {
	cfg := idle.DefaultConfig()
	rows, err := analyzeIdleFixture("../../cas/idle/testdata/idle_demo.json", cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		class      idle.Class
		suggestion string
		reason     string
	}{
		"office-web":    {idle.ClassScheduled, "", "PATTERN_SCHEDULED"},
		"forgotten":     {idle.ClassIdle, "release/回收", "LOW_CPU"},
		"old-off":       {idle.ClassShutoffIdle, "release/回收", "SHUTOFF_LONG"},
		"weekly-batch":  {idle.ClassIntermittent, "保留/核实", "BURST: 每周日 02-04 负载 90%"},
		"monthly-burst": {idle.ClassIntermittent, "保留/核实", "BURST: 每月一次 6 小时突发 峰值 95%"},
		"single-burst":  {idle.ClassIntermittent, "保留/核实", "BURST: 单次 2 小时突发 峰值 95%"},
		"net-backup":    {idle.ClassIntermittent, "保留/核实", "BURST: 每周日 02-04 网络 5 MB/s"},
		"disk-backup":   {idle.ClassIntermittent, "保留/核实", "BURST: 每周日 02-04 磁盘 20 MB/s"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows=%d want %d", len(rows), len(want))
	}
	for _, r := range rows {
		w, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected vm %q", r.Name)
		}
		if r.Class != w.class {
			t.Errorf("%s: class=%s want %s (%v)", r.Name, r.Class, w.class, r.Reasons)
		}
		if w.suggestion != "" && r.Suggestion != w.suggestion {
			t.Errorf("%s: suggestion=%q want %q", r.Name, r.Suggestion, w.suggestion)
		}
		if !strings.Contains(strings.Join(r.Reasons, ";"), w.reason) {
			t.Errorf("%s: want reason %q in %v", r.Name, w.reason, r.Reasons)
		}
	}
}
