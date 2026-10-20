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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	impSource  = "cloud.qcow2"
	impSrcPath = "/vms/images/cloud.qcow2"
)

// impFake is a CAS with one host, one dir pool holding cloud.qcow2.
type impFake struct {
	t *testing.T

	lock      sync.Mutex
	hits      []string
	bodies    map[string]string
	volumes   []dict
	vms       []dict
	templates []dict
	polls     map[int64]int
	failTask  map[int64]bool // msgId -> task result 1
	refreshed int

	// failure injection
	failVolumeHTTP bool   // POST storage/volume -> 500
	badBacking     string // overlay reports this backing store
	vmAddCreates   bool   // vm/add task fails but the VM exists
	vmStatus       string // status of the new VM (default shutOff)
	srcHidden      bool   // source not listed until refresh
	poolPath       string // default /vms/images
	failVMAddHTTP  bool   // vm/add -> 409 without creating a VM
	failVMDelete   bool   // DELETE vm/delete/uuid -> 409
}

func newImpFake(t *testing.T) *impFake {
	return &impFake{t: t, bodies: map[string]string{}, polls: map[int64]int{}, failTask: map[int64]bool{},
		volumes: []dict{{"name": impSource, "format": "qcow2", "size": 2252.5, "storagePoolName": "defaultpool"}},
	}
}

func (f *impFake) writes() []string {
	f.lock.Lock()
	defer f.lock.Unlock()
	ret := []string{}
	for _, h := range f.hits {
		if !strings.HasPrefix(h, "GET ") || strings.HasPrefix(h, "GET /cas/casrs/storage/refresh") {
			ret = append(ret, h)
		}
	}
	return ret
}

func (f *impFake) pool() string {
	if len(f.poolPath) > 0 {
		return f.poolPath
	}
	return "/vms/images"
}

func (f *impFake) task(w http.ResponseWriter, id int64) {
	reply(w, dict{"msgId": id, "completed": false})
}

func (f *impFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
	path := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		f.bodies[r.Method+" "+path] = string(body)
	}
	switch {
	case r.Method == "GET" && path == "host/id/1":
		reply(w, dict{"id": 1, "name": "cvk1", "ip": "192.0.2.10", "hostPoolId": 1, "clusterId": 1})
	case r.Method == "GET" && path == "storage/pool":
		reply(w, dict{"storagePool": []dict{
			{"name": "defaultpool", "type": "dir", "path": f.pool()},
			{"name": "blk", "type": "iscsi", "path": "/dev/disk/by-path/x"},
		}})
	case r.Method == "GET" && path == "host/id/1/storage":
		reply(w, dict{"storagePool": []dict{}})
	case r.Method == "GET" && path == "host/id/1/vswitch":
		reply(w, dict{"vSwitch": []dict{{"id": 3, "name": "vswitch0", "mtu": 1500}}})
	case r.Method == "GET" && path == "profile/all":
		reply(w, dict{"portProfile": []dict{{"id": 7, "name": "Default"}}})
	case r.Method == "GET" && path == "vmTemplate/all":
		reply(w, dict{"domain": append([]dict{}, f.templates...)})
	case r.Method == "GET" && path == "vm/vmList":
		reply(w, dict{"domain": append([]dict{}, f.vms...)})
	case r.Method == "GET" && path == "storage/volume":
		out := []dict{}
		for _, v := range f.volumes {
			if v["name"] == impSource && f.srcHidden {
				continue
			}
			if name := q.Get("volumeName"); len(name) == 0 || v["name"] == name {
				out = append(out, v)
			}
		}
		reply(w, dict{"storageVolume": out})
	case r.Method == "GET" && path == "storage/refresh":
		f.refreshed++
		f.srcHidden = false
		reply(w, dict{"state": 0, "errorCode": 0, "message": "ok"})
	case r.Method == "POST" && path == "storage/volume":
		if f.failVolumeHTTP {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, _ := jsonutils.Parse(body)
		name, _ := b.GetString("volName")
		base, _ := b.GetString("baseFile")
		if len(f.badBacking) > 0 {
			base = f.badBacking
		}
		f.volumes = append(f.volumes, dict{"name": name, "format": "qcow2", "size": 2253, "backingStore": base})
		f.task(w, 8001)
	case r.Method == "POST" && path == "vm/add" && f.failVMAddHTTP:
		w.Header().Set("Error-Code", "999")
		w.WriteHeader(http.StatusConflict)
	case r.Method == "POST" && path == "vm/add":
		b, _ := jsonutils.Parse(body)
		name, _ := b.GetString("name")
		st := f.vmStatus
		if len(st) == 0 {
			st = "shutOff"
		}
		f.vms = append(f.vms, dict{"id": 50, "uuid": "uuid-tmp", "name": name, "vmStatus": st, "hostId": 1})
		if f.vmAddCreates {
			f.failTask[8002] = true
		}
		f.task(w, 8002)
	case r.Method == "PUT" && path == "vm/powerOff/50":
		for _, vm := range f.vms {
			vm["vmStatus"] = "shutOff"
		}
		f.task(w, 8005)
	case r.Method == "POST" && path == "vmTemplate/addVmTemplate":
		if !f.failTask[8003] {
			f.templates = append(f.templates, dict{"id": 90, "name": q.Get("domainName")})
		}
		f.task(w, 8003)
	case r.Method == "DELETE" && strings.HasPrefix(path, "vm/delete/uuid/") && f.failVMDelete:
		w.Header().Set("Error-Code", "999")
		w.WriteHeader(http.StatusConflict)
	case r.Method == "DELETE" && strings.HasPrefix(path, "vm/delete/uuid/"):
		for _, vm := range f.vms {
			if vm["uuid"] == strings.TrimPrefix(path, "vm/delete/uuid/") {
				vm["delTime"] = "2026-10-20T12:00:00+08:00"
			}
		}
		f.task(w, 8004)
	case r.Method == "DELETE" && strings.HasPrefix(path, "storage/volume/delete/1/defaultpool/"):
		name := strings.TrimPrefix(path, "storage/volume/delete/1/defaultpool/")
		kept := []dict{}
		for _, v := range f.volumes {
			if v["name"] != name {
				kept = append(kept, v)
			}
		}
		f.volumes = kept
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "GET" && strings.HasPrefix(path, "message/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(path, "message/"), "%d", &id)
		f.polls[id]++
		result := 0
		if f.failTask[id] {
			result = 1
		}
		reply(w, dict{"msgId": id, "completed": f.polls[id] >= 2, "result": result, "failMsg": "injected"})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *impFake) client(t *testing.T) *SCasClient {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	return cli
}

func impOpts() SImageImportOptions {
	return SImageImportOptions{HostId: 1, PoolName: "defaultpool", File: impSource, TemplateName: "ubuntu-2204", VSwitchName: "vswitch0"}
}

// assertSourceUntouched: no write request ever targets the source file;
// it only appears as baseFile in POST storage/volume.
func (f *impFake) assertSourceUntouched(t *testing.T) {
	for _, h := range f.writes() {
		if strings.Contains(h, impSource) {
			t.Errorf("write %q names the source file", h)
		}
	}
	for k, b := range f.bodies {
		if strings.Contains(b, impSource) && k != "POST storage/volume" {
			t.Errorf("%s body names the source: %s", k, b)
		}
	}
	found := false
	for _, v := range f.volumes {
		if v["name"] == impSource {
			found = true
		}
	}
	if !found {
		t.Error("source volume is gone")
	}
}

func TestImportQcow2HappyPath(t *testing.T) {
	f := newImpFake(t)
	res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err != nil {
		t.Fatal(err)
	}
	if res.Template == nil || res.Template.Id != 90 || res.Template.Name != "ubuntu-2204" {
		t.Fatalf("template = %+v", res.Template)
	}
	want := []string{
		"POST /cas/casrs/storage/volume",
		"POST /cas/casrs/vm/add",
		"POST /cas/casrs/vmTemplate/addVmTemplate?",
		"DELETE /cas/casrs/vm/delete/uuid/uuid-tmp?",
	}
	w := f.writes()
	if len(w) != len(want) {
		t.Fatalf("writes = %v", w)
	}
	for i := range want {
		if !strings.HasPrefix(w[i], want[i]) {
			t.Errorf("write %d = %s, want %s", i, w[i], want[i])
		}
	}
	if !strings.Contains(w[2], "opType=1") || strings.Contains(w[2], "opType=2") {
		t.Errorf("addVmTemplate %s", w[2])
	}
	if !strings.Contains(w[3], "type=3") || !strings.Contains(w[3], "isWipeVolume=false") {
		t.Errorf("delete %s", w[3])
	}
	vol, _ := jsonutils.ParseString(f.bodies["POST storage/volume"])
	for k, v := range map[string]string{"baseFile": impSrcPath, "volName": "tplimp-ubuntu-2204-sys.qcow2", "format": "qcow2", "spName": "defaultpool"} {
		if got, _ := vol.GetString(k); got != v {
			t.Errorf("storage/volume %s = %q, want %q", k, got, v)
		}
	}
	if c, _ := vol.Int("capacity"); c != 2253 {
		t.Errorf("capacity = %d, want ceil(2252.5)", c)
	}
	add, _ := jsonutils.ParseString(f.bodies["POST vm/add"])
	for k, v := range map[string]string{"name": "tplimp-ubuntu-2204", "osVersion": DEFAULT_IMPORT_OS_VERSION, "memoryUnit": "MB"} {
		if got, _ := add.GetString(k); got != v {
			t.Errorf("vm/add %s = %q, want %q", k, got, v)
		}
	}
	if ab, _ := add.Int("autoBooting"); ab != 0 {
		t.Errorf("autoBooting = %d", ab)
	}
	var sf string
	if arr, err := add.GetArray("storage"); err != nil || len(arr) != 1 {
		t.Fatalf("storage = %v", arr)
	} else {
		sf, _ = arr[0].GetString("storeFile")
		if sf != "/vms/images/tplimp-ubuntu-2204-sys.qcow2" {
			t.Errorf("storeFile = %s", sf)
		}
	}
	nic, _ := add.GetArray("network")
	if len(nic) != 1 {
		t.Fatalf("network = %v", nic)
	}
	if id, _ := nic[0].Int("vsId"); id != 3 {
		t.Errorf("vsId = %d", id)
	}
	if id, _ := nic[0].Int("profileId"); id != 7 {
		t.Errorf("profileId = %d", id)
	}
	for _, id := range []int64{8001, 8002, 8003, 8004} {
		if f.polls[id] < 2 {
			t.Errorf("task %d polled %d times", id, f.polls[id])
		}
	}
	f.assertSourceUntouched(t)
}

func TestImportQcow2DryRunZeroWrites(t *testing.T) {
	f := newImpFake(t)
	opts := impOpts()
	opts.DryRun = true
	opts.Refresh = true
	res, err := f.client(t).ImportQcow2AsTemplate(opts)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("dry-run writes = %v", w)
	}
	steps := []string{}
	for _, s := range res.Steps {
		if s.Done || !s.Write {
			t.Errorf("dry-run step %+v must be planned, not done", s)
		}
		steps = append(steps, s.Step)
	}
	if got := strings.Join(steps, ","); got != "refresh-pool,create-overlay,create-vm,clone-to-template,delete-temp-vm" {
		t.Errorf("steps = %s", got)
	}
	if !strings.Contains(res.Steps[1].Detail, impSrcPath) {
		t.Errorf("overlay plan %s", res.Steps[1].Detail)
	}
}

func TestImportQcow2ReadOnlyZeroRequests(t *testing.T) {
	f := newImpFake(t)
	cli := f.client(t)
	cli.cpcfg.ReadOnly = true
	_, err := cli.ImportQcow2AsTemplate(impOpts())
	if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Fatalf("err = %v", err)
	}
	if len(f.hits) != 0 {
		t.Fatalf("hits = %v", f.hits)
	}
}

func TestImportQcow2Refusals(t *testing.T) {
	cases := map[string]struct {
		mod  func(f *impFake, o *SImageImportOptions)
		want error
	}{
		"block pool":    {func(f *impFake, o *SImageImportOptions) { o.PoolName = "blk" }, cloudprovider.ErrNotSupported},
		"path outside":  {func(f *impFake, o *SImageImportOptions) { o.File = "/etc/cloud.qcow2" }, cloudprovider.ErrInputParameter},
		"bad name":      {func(f *impFake, o *SImageImportOptions) { o.File = "a b.qcow2" }, cloudprovider.ErrInputParameter},
		"raw source":    {func(f *impFake, o *SImageImportOptions) { f.volumes[0]["format"] = "raw" }, cloudprovider.ErrNotSupported},
		"source in use": {func(f *impFake, o *SImageImportOptions) { f.volumes[0]["users"] = "vm1" }, cloudprovider.ErrInvalidStatus},
		"dup template": {func(f *impFake, o *SImageImportOptions) {
			f.templates = []dict{{"id": 1, "name": "ubuntu-2204"}}
		}, cloudprovider.ErrDuplicateId},
		"dup temp vm": {func(f *impFake, o *SImageImportOptions) {
			f.vms = []dict{{"id": 2, "uuid": "u2", "name": "tplimp-ubuntu-2204"}}
		}, cloudprovider.ErrDuplicateId},
		"overlay exists": {func(f *impFake, o *SImageImportOptions) {
			f.volumes = append(f.volumes, dict{"name": "tplimp-ubuntu-2204-sys.qcow2", "format": "qcow2"})
		}, cloudprovider.ErrDuplicateId},
		"no vswitch":   {func(f *impFake, o *SImageImportOptions) { o.VSwitchName = "nope" }, cloudprovider.ErrNotFound},
		"not listed":   {func(f *impFake, o *SImageImportOptions) { f.srcHidden = true }, cloudprovider.ErrNotFound},
		"low memory":   {func(f *impFake, o *SImageImportOptions) { o.MemoryMB = 256 }, cloudprovider.ErrInputParameter},
		"same as tmpl": {func(f *impFake, o *SImageImportOptions) { o.TempVMName = o.TemplateName }, cloudprovider.ErrInputParameter},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newImpFake(t)
			o := impOpts()
			c.mod(f, &o)
			_, err := f.client(t).ImportQcow2AsTemplate(o)
			if errors.Cause(err) != c.want {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v, want none", w)
			}
		})
	}
}

func TestImportQcow2RefreshThenImport(t *testing.T) {
	f := newImpFake(t)
	f.srcHidden = true
	o := impOpts()
	o.Refresh = true
	if _, err := f.client(t).ImportQcow2AsTemplate(o); err != nil {
		t.Fatal(err)
	}
	if f.refreshed != 1 || !strings.HasPrefix(f.writes()[0], "GET /cas/casrs/storage/refresh?id=1&poolName=defaultpool") {
		t.Fatalf("refresh not first: %v", f.writes())
	}
}

// Failure at each write step: cleanup removes only what was created and
// never touches the source.
func TestImportQcow2CleanupOnFailure(t *testing.T) {
	cases := map[string]struct {
		mod         func(f *impFake)
		wantCleanup string // write prefix expected as the last write
		vmRecycled  bool
	}{
		"overlay http error":           {func(f *impFake) { f.failVolumeHTTP = true }, "POST /cas/casrs/storage/volume", false},
		"overlay task fails":           {func(f *impFake) { f.failTask[8001] = true }, "DELETE /cas/casrs/storage/volume/delete/1/defaultpool/tplimp-ubuntu-2204-sys.qcow2", false},
		"wrong backing":                {func(f *impFake) { f.badBacking = "/vms/images/other" }, "DELETE /cas/casrs/storage/volume/delete/1/defaultpool/tplimp-ubuntu-2204-sys.qcow2", false},
		"vm/add task fails, VM exists": {func(f *impFake) { f.vmAddCreates = true }, "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp", true},
		"template task fails":          {func(f *impFake) { f.failTask[8003] = true }, "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp", true},
		"unexpected vm status":         {func(f *impFake) { f.vmStatus = "weird" }, "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp", true},
		"vm/add http error, no VM":     {func(f *impFake) { f.failVMAddHTTP = true }, "DELETE /cas/casrs/storage/volume/delete/1/defaultpool/tplimp-ubuntu-2204-sys.qcow2", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newImpFake(t)
			c.mod(f)
			res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
			if err == nil {
				t.Fatal("want error")
			}
			if res == nil || res.Template != nil {
				t.Fatalf("res = %+v", res)
			}
			w := f.writes()
			if !strings.HasPrefix(w[len(w)-1], c.wantCleanup) {
				t.Fatalf("last write = %s, want %s (all %v)", w[len(w)-1], c.wantCleanup, w)
			}
			for _, h := range w {
				if strings.Contains(h, "addVmTemplate") && strings.Contains(h, "opType=2") {
					t.Fatal("opType=2 sent")
				}
			}
			if c.vmRecycled {
				if f.vms[0]["delTime"] == nil {
					t.Error("temporary VM not recycled")
				}
				if !strings.Contains(w[len(w)-1], "type=3") {
					t.Errorf("cleanup delete %s", w[len(w)-1])
				}
			}
			f.assertSourceUntouched(t)
		})
	}
}

func TestImportQcow2PowersOffRunningVM(t *testing.T) {
	f := newImpFake(t)
	f.vmStatus = "running"
	if _, err := f.client(t).ImportQcow2AsTemplate(impOpts()); err != nil {
		t.Fatal(err)
	}
	w := strings.Join(f.writes(), "\n")
	if !strings.Contains(w, "PUT /cas/casrs/vm/powerOff/50") || strings.Index(w, "powerOff") > strings.Index(w, "addVmTemplate") {
		t.Fatalf("powerOff must come before addVmTemplate: %s", w)
	}
}

// The template exists but the temp VM delete fails: the template is
// returned with an error that names the VM to remove by hand.
func TestImportQcow2TempVMDeleteFails(t *testing.T) {
	f := newImpFake(t)
	f.failVMDelete = true
	res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "remove it by hand") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Template == nil || res.Template.Id != 90 {
		t.Fatalf("res = %+v", res)
	}
	f.assertSourceUntouched(t)
}
