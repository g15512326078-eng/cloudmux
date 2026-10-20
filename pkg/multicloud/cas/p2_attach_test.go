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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestAttachDetachDisk(t *testing.T) {
	f, _, _, vm := newP2Env(t)
	f.vols["data1.qcow2"] = &p2Vol{name: "data1.qcow2", format: "qcow2", sizeMB: 4096}
	id := "host/7/defaultpool/data1.qcow2"
	if err := vm.AttachDisk(context.Background(), id); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	checkFields(t, "addDevice", f.lastBody("PUT vm/addDevice"), map[string]interface{}{"id": 9})
	st, _ := f.lastBody("PUT vm/addDevice").Get("storage")
	checkFields(t, "addDevice.storage", st, map[string]interface{}{
		"device": "disk", "fileType": "file", "targetBus": "virtio", "cacheType": "directsync",
		"format": "qcow2", "pathType": 0, "path": p2PoolPath + "/data1.qcow2",
	})
	disks, _ := vm.GetIDisks()
	if len(disks) != 2 || disks[1].GetGlobalId() != id || disks[1].GetDiskType() != "data" {
		t.Fatalf("after attach disks = %v", disks)
	}
	// already attached: no second addDevice
	if err := vm.AttachDisk(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n := len(f.bodies["PUT vm/addDevice"]); n != 1 {
		t.Errorf("addDevice calls = %d", n)
	}

	if err := vm.DetachDisk(context.Background(), id); err != nil {
		t.Fatalf("DetachDisk: %v", err)
	}
	st, _ = f.lastBody("PUT vm/delDevice").Get("storage")
	checkFields(t, "delDevice.storage", st, map[string]interface{}{"deviceName": "vdb", "path": p2PoolPath + "/data1.qcow2"})
	if f.vols["data1.qcow2"] == nil {
		t.Fatal("detach deleted the volume")
	}
	for _, w := range f.writes() {
		if strings.HasPrefix(w, "DELETE ") {
			t.Errorf("detach issued %s", w)
		}
	}
	// detaching again is a no-op; the system disk is refused
	if err := vm.DetachDisk(context.Background(), id); err != nil {
		t.Errorf("second detach: %v", err)
	}
	if err := vm.DetachDisk(context.Background(), "host/7/defaultpool/vm9-sys.qcow2"); errors.Cause(err) != cloudprovider.ErrNotSupported {
		t.Errorf("sys detach: %v", err)
	}
	if n := len(f.bodies["PUT vm/delDevice"]); n != 1 {
		t.Errorf("delDevice calls = %d", n)
	}
}

func TestAttachDiskRefusals(t *testing.T) {
	f, _, _, vm := newP2Env(t)
	f.vols["busy.qcow2"] = &p2Vol{name: "busy.qcow2", format: "qcow2", sizeMB: 1, users: "other"}
	f.vols["os.iso"] = &p2Vol{name: "os.iso", format: "iso", sizeMB: 1}
	cases := map[string]error{
		"host/7/defaultpool/busy.qcow2":    cloudprovider.ErrInvalidStatus,
		"host/7/defaultpool/os.iso":        cloudprovider.ErrInputParameter,
		"host/7/defaultpool/missing.qcow2": cloudprovider.ErrNotFound,
		"host/8/defaultpool/x.qcow2":       cloudprovider.ErrNotFound, // other host's pool
		"uuid-9/vdb":                       cloudprovider.ErrInputParameter,
	}
	for id, want := range cases {
		if err := vm.AttachDisk(context.Background(), id); errors.Cause(err) != want {
			t.Errorf("AttachDisk(%s) = %v, want %v", id, err, want)
		}
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("refusals wrote: %v", w)
	}
}

func TestAttachDiskTaskFailure(t *testing.T) {
	f, _, _, vm := newP2Env(t)
	f.vols["data1.qcow2"] = &p2Vol{name: "data1.qcow2", format: "qcow2", sizeMB: 1}
	f.failMsg["PUT vm/addDevice"] = true
	err := vm.AttachDisk(context.Background(), "host/7/defaultpool/data1.qcow2")
	if err == nil || !strings.Contains(err.Error(), "fake failure") {
		t.Errorf("AttachDisk failure: %v", err)
	}
}

func TestCreateVMWithDataDisks(t *testing.T) {
	f, region, host, _ := newP2Env(t)
	// deploy is routed to the lifecycle shape: reply a task with targetId 9
	mux := http.NewServeMux()
	mux.HandleFunc(API_PREFIX+"/vm/deploy", func(w http.ResponseWriter, r *http.Request) {
		f.lock.Lock()
		f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
		f.nextMsg++
		id := f.nextMsg
		f.lock.Unlock()
		reply(w, dict{"msgId": id, "completed": false, "targetId": 9})
	})
	mux.Handle("/", f)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	region.cli = cli

	vm, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{
		Name: "vm9", ExternalImageId: "11", Cpu: 1, MemoryMB: 1024,
		SysDisk:   cloudprovider.SDiskInfo{StorageExternalId: "host/7/defaultpool"},
		DataDisks: []cloudprovider.SDiskInfo{{SizeGB: 10}, {SizeGB: 20, StorageExternalId: "host/7/defaultpool"}},
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	adds := f.bodies["PUT vm/addDevice"]
	if len(adds) != 2 {
		t.Fatalf("addDevice calls = %d", len(adds))
	}
	for i, size := range []int64{10240, 20480} {
		st, _ := adds[i].Get("storage")
		checkFields(t, fmt.Sprintf("data%d", i+1), st, map[string]interface{}{
			"device": "disk", "pathType": 2, "poolName": "defaultpool", "fileName": fmt.Sprintf("vm9-data%d", i+1),
			"size": size, "format": "qcow2", "mode": 2, "targetBus": "virtio",
		})
	}
	disks, err := vm.GetIDisks()
	if err != nil || len(disks) != 3 {
		t.Fatalf("disks after create = %d %v", len(disks), err)
	}
	if disks[2].GetDiskSizeMB() != 20480 || disks[2].GetGlobalId() != "host/7/defaultpool/vm9-data2" {
		t.Errorf("data2 = %s %d", disks[2].GetGlobalId(), disks[2].GetDiskSizeMB())
	}
	for _, w := range f.writes() {
		if strings.HasPrefix(w, "DELETE ") {
			t.Errorf("create issued %s", w)
		}
	}
}

func TestCreateVMDataDiskFailureRollsBack(t *testing.T) {
	f, region, host, _ := newP2Env(t)
	mux := http.NewServeMux()
	mux.HandleFunc(API_PREFIX+"/vm/deploy", func(w http.ResponseWriter, r *http.Request) {
		f.lock.Lock()
		f.nextMsg++
		id := f.nextMsg
		f.lock.Unlock()
		reply(w, dict{"msgId": id, "completed": false, "targetId": 9})
	})
	mux.Handle("/", f)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	region.cli = cli
	f.failMsg["PUT vm/addDevice"] = true
	_, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{
		Name: "vm9", ExternalImageId: "11", Cpu: 1, MemoryMB: 1024,
		SysDisk:   cloudprovider.SDiskInfo{StorageExternalId: "host/7/defaultpool"},
		DataDisks: []cloudprovider.SDiskInfo{{SizeGB: 10}},
	})
	if err == nil || !strings.Contains(err.Error(), "uuid-9") || !strings.Contains(err.Error(), "recycle") {
		t.Errorf("err = %v", err)
	}
	foundDelete := false
	for _, w := range f.writes() {
		if strings.Contains(w, "vm/delete/uuid/uuid-9") && strings.Contains(w, "type=3") {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Errorf("expected rollback DeleteVM type=3: %v", f.writes())
	}
}

func TestCreateVMDataDiskCollisionRefused(t *testing.T) {
	f, region, host, _ := newP2Env(t)
	f.vols["vm9-data1"] = &p2Vol{name: "vm9-data1", format: "qcow2", sizeMB: 1}
	mux := http.NewServeMux()
	mux.HandleFunc(API_PREFIX+"/vm/deploy", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("deploy must not run when data-disk file exists")
		w.WriteHeader(500)
	})
	mux.Handle("/", f)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	region.cli = cli
	_, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{
		Name: "vm9", ExternalImageId: "11", Cpu: 1, MemoryMB: 1024,
		SysDisk:   cloudprovider.SDiskInfo{StorageExternalId: "host/7/defaultpool"},
		DataDisks: []cloudprovider.SDiskInfo{{SizeGB: 10}},
	})
	if errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Errorf("collision: %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Errorf("writes before deploy: %v", w)
	}
}

func TestCreateVMDataDiskValidation(t *testing.T) {
	cli := &SCasClient{CasClientConfig: NewCasClientConfig("http://127.0.0.1:1", "u", "p")}
	host := &SHost{Id: 1, zone: &SZone{region: &SRegion{cli: cli}}}
	for _, dd := range [][]cloudprovider.SDiskInfo{{{SizeGB: 0, StorageExternalId: "host/1/p"}}, {{SizeGB: 10}}} {
		_, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{
			Name: "x", ExternalImageId: "1", Cpu: 1, MemoryMB: 512, DataDisks: dd,
		})
		if c := errors.Cause(err); c != cloudprovider.ErrInputParameter && c != cloudprovider.ErrMissingParameter {
			t.Errorf("DataDisks %+v: %v", dd, err)
		}
	}
}
