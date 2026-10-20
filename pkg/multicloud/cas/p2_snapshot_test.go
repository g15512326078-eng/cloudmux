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

package cas

import (
	"context"
	"strings"
	"testing"
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestInstanceSnapshots(t *testing.T) {
	f, _, _, vm := newP2Env(t)
	snaps, err := vm.GetInstanceSnapshots()
	if err != nil || len(snaps) != 0 {
		t.Fatalf("empty: %v %v", snaps, err)
	}
	for _, name := range []string{"s1", "s2", "s3"} {
		s, err := vm.CreateInstanceSnapshot(context.Background(), name, "desc")
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if s.GetGlobalId() != "uuid-9/"+name {
			t.Errorf("id %s", s.GetGlobalId())
		}
	}
	checkFields(t, "snapshot", f.lastBody("POST vm/snapshot"), map[string]interface{}{"vmId": 9, "name": "s3", "desc": "desc", "snapMem": false})
	// branch: s4 under s1 makes s1 have two children (array form)
	f.lock.Lock()
	f.snaps = append(f.snaps, p2Snap{name: "s4", parent: "s1"})
	f.lock.Unlock()
	snaps, err = vm.GetInstanceSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range snaps {
		names = append(names, s.GetName())
	}
	if strings.Join(names, ",") != "s1,s2,s3,s4" {
		t.Errorf("flattened = %v", names)
	}
	if !snaps[0].GetCreatedAt().Equal(time.Date(2020, 12, 2, 11, 34, 59, 0, time.UTC)) {
		t.Errorf("createdAt %v", snaps[0].GetCreatedAt())
	}
	if _, err := vm.CreateInstanceSnapshot(context.Background(), "s2", ""); errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Errorf("dup: %v", err)
	}
	if _, err := vm.GetInstanceSnapshot("uuid-other/s1"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("other vm: %v", err)
	}
	if err := vm.ResetToInstanceSnapshot(context.Background(), "uuid-9/s2"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	checkFields(t, "resume", f.lastBody("PUT vm/snapshot/resume"), map[string]interface{}{"vmId": 9, "name": "s2"})
	if err := vm.ResetToInstanceSnapshot(context.Background(), "nope"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("reset missing: %v", err)
	}
	s2, err := vm.GetInstanceSnapshot("uuid-9/s2")
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s2.Delete(); err != nil {
		t.Errorf("delete again: %v", err)
	}
	dels := 0
	for _, w := range f.writes() {
		if strings.HasPrefix(w, "DELETE ") {
			dels++
			if w != "DELETE /cas/casrs/vm/snapshot/9/s2" {
				t.Errorf("delete hit %s", w)
			}
		}
	}
	if dels != 2 {
		t.Errorf("deletes = %d", dels)
	}
}

func TestP2WritesBlockedWhenReadOnly(t *testing.T) {
	f, region, host, vm := newP2Env(t)
	region.cli.cpcfg.ReadOnly = true
	f.vols["data1.qcow2"] = &p2Vol{name: "data1.qcow2", format: "qcow2", sizeMB: 1}
	pool := localPool(t, host)
	disks, _ := vm.GetIDisks()
	free := volumeDisk(pool, &SStorageVolume{Name: "data1.qcow2", Format: "qcow2", Size: 1})
	for name, fn := range map[string]func() error{
		"CreateIDisk": func() error {
			_, err := pool.CreateIDisk(&cloudprovider.DiskCreateConfig{Name: "n", SizeGb: 1})
			return err
		},
		"AttachDisk": func() error { return vm.AttachDisk(context.Background(), "host/7/defaultpool/data1.qcow2") },
		"Resize":     func() error { return disks[0].Resize(context.Background(), 40960) },
		"Delete":     func() error { return free.Delete(context.Background()) },
		"Snapshot": func() error {
			_, err := vm.CreateInstanceSnapshot(context.Background(), "s", "")
			return err
		},
	} {
		if err := fn(); errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
			t.Errorf("%s: %v, want ErrAccountReadOnly", name, err)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("writes reached server: %v", w)
	}
}
