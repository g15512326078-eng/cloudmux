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
package cas

import (
	"testing"
	"time"
)

// Normalize each s/ms value first, then pick the newest.
func TestMonitorTimestampMixedUnitsPicksNewest(t *testing.T) {
	older := time.Date(2020, 9, 13, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2020, 12, 8, 0, 0, 0, 0, time.UTC)
	// Two NICs: the older one reports ms (larger raw number), the newer s.
	nets := []SVmMonitorNet{
		{Time: older.UnixMilli()},
		{Time: newer.Unix()},
	}
	if got := monitorTimestamp(0, nets, nil); !got.Equal(newer) {
		t.Fatalf("nets: got %s want %s", got.UTC(), newer)
	}
	// seconds value newer than a ms value
	nets = []SVmMonitorNet{{Time: older.UnixMilli()}}
	disks := []SVmMonitorDisk{{Time: newer.Unix()}}
	if got := monitorTimestamp(0, nets, disks); !got.Equal(newer) {
		t.Fatalf("net ms + disk s: got %s want %s", got.UTC(), newer)
	}
	if got := monitorTimestamp(newer.Unix(), nets, disks); !got.Equal(newer) {
		t.Fatalf("explicit sample: got %s", got.UTC())
	}
}
