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

// Package cas implements a read-only (P0) cloudmux provider for H3C CAS
// (CVM/CVK) virtualization, talking to the CAS REST API under
// {AccessUrl}/cas/casrs/ with HTTP Digest authentication.
//
// This is not the H3C public/CloudOS provider (pkg/multicloud/h3c) and has
// nothing to do with the Keystone CAS SSO driver.
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

type recycleFake struct {
	t        *testing.T
	hits     []string
	list     []dict
	msgPolls map[int64]int
}

func (f *recycleFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
	path := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	parts := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "vm" && parts[1] == "powerOff":
		reply(w, dict{"msgId": 5001, "completed": false, "targetId": 9})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "vm/delete/uuid/"):
		reply(w, dict{"msgId": 5002, "completed": false, "targetId": 9})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "message":
		var id int64
		fmt.Sscanf(parts[1], "%d", &id)
		if f.msgPolls == nil {
			f.msgPolls = map[int64]int{}
		}
		f.msgPolls[id]++
		completed := f.msgPolls[id] >= 2
		if completed && id == 5002 {
			for i := range f.list {
				if u, _ := f.list[i]["uuid"].(string); u == "uuid-9" {
					f.list[i]["delTime"] = "2020-10-29T22:54:10+08:00"
				}
			}
		}
		reply(w, dict{"msgId": id, "completed": completed, "result": 0, "targetId": 9})
	case r.Method == http.MethodGet && path == "vm/vmList":
		reply(w, dict{"domain": append([]dict{}, f.list...)})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "vm/detail/uuid/"):
		uuid := strings.TrimPrefix(path, "vm/detail/uuid/")
		for _, row := range f.list {
			if u, _ := row["uuid"].(string); u == uuid {
				reply(w, dict{"id": row["id"], "uuid": uuid, "name": row["name"], "status": "shutOff", "storage": []dict{}, "network": []dict{}})
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet && path == "hostpool/all":
		reply(w, dict{"hostPool": []dict{{"id": 1, "name": "hp1"}}})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func newRecycleClient(t *testing.T, f *recycleFake) *SCasClient {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 10 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	return cli
}

func TestIsRecycledHelpers(t *testing.T) {
	if (&SInstance{}).isRecycled() || (&SInstance{DelTime: "  "}).isRecycled() {
		t.Fatal("empty/whitespace delTime must not count as recycled")
	}
	if !(&SInstance{DelTime: "2020-10-29T22:54:10+08:00"}).isRecycled() {
		t.Fatal("non-empty delTime must count as recycled")
	}
	if (&SInstance{Type: VM_TYPE_VMWARE}).isLiveGuest() || (&SInstance{DelTime: "x"}).isLiveGuest() {
		t.Fatal("VMware/recycled must not be live")
	}
}

func TestDeleteVMWaitsUntilRecycledInList(t *testing.T) {
	f := &recycleFake{t: t, list: []dict{{
		"id": 9, "uuid": "uuid-9", "name": "vm9", "hostId": 1, "hostPoolId": 1,
		"cpu": 1, "memory": 512, "vmStatus": "shutOff", "system": 1, "type": 0,
	}}}
	cli := newRecycleClient(t, f)
	vm := &SInstance{Id: 9, Uuid: "uuid-9", VmStatus: "running", region: &SRegion{cli: cli}}
	if err := vm.DeleteVM(context.Background()); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	live, err := cli.GetInstances(nil)
	if err != nil || len(live) != 0 {
		t.Fatalf("GetInstances after delete = %v, %v", live, err)
	}
	if _, err := cli.GetInstance("uuid-9"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Fatalf("GetInstance(recycled) = %v, want ErrNotFound", err)
	}
	got := strings.Join(f.hits, "\n")
	if !strings.Contains(got, "isWipeVolume=false&type=3") || !strings.Contains(got, "vm/vmList") {
		t.Errorf("hits missing recycle delete or vmList wait: %s", got)
	}
}

func TestGetInstancesFiltersDelTime(t *testing.T) {
	f := &recycleFake{t: t, list: []dict{
		{"id": 1, "uuid": "live-1", "name": "a", "hostId": 1, "hostPoolId": 1, "cpu": 1, "memory": 512, "vmStatus": "running", "system": 1, "type": 0},
		{"id": 2, "uuid": "recycled-2", "name": "b", "hostId": 1, "hostPoolId": 1, "cpu": 1, "memory": 512, "vmStatus": "shutOff", "system": 1, "type": 0, "delTime": "2020-10-29T22:54:10+08:00"},
		{"id": 3, "uuid": "vmware-3", "name": "c", "hostId": 1, "hostPoolId": 1, "cpu": 1, "memory": 512, "vmStatus": "running", "system": 1, "type": 1},
	}}
	cli := newRecycleClient(t, f)
	vms, err := cli.GetInstances(nil)
	if err != nil || len(vms) != 1 || vms[0].Uuid != "live-1" {
		t.Fatalf("GetInstances = %+v, %v", vms, err)
	}
}
