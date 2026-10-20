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
	vmAddStatus    int    // vm/add -> this HTTP status (no task)
	vmAddStCreates bool   // ... and the VM was created anyway
	failVMDelete   bool   // DELETE vm/delete/uuid -> 409
	vmAdd500       bool   // vm/add -> 500 without creating a VM (ambiguous)
	dupVMOnAdd     bool   // vm/add: a foreign VM with the same name appears too
	vmListFail     bool   // vm/vmList -> 500 once vm/add was received
	vmAdded        bool
	overlayUsers   string // users reported on the overlay
	tplNever       bool   // addVmTemplate task never completes
	tplLeftOnFail  bool   // addVmTemplate fails but leaves a template row
	tplBacking     string // backingStore of the template's disk volume
	tplStorageFail bool   // GET vmTemplate/storage -> 500
	tplNoBackKey   bool   // template volume JSON has no backingStore key
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
		reply(w, dict{"storagePool": []dict{{"name": "tplpool", "type": "netfs", "path": "/vms/template/"}}})
	case r.Method == "GET" && path == "host/id/1/vswitch":
		reply(w, dict{"vSwitch": []dict{{"id": 3, "name": "vswitch0", "mtu": 1500}}})
	case r.Method == "GET" && path == "profile/all":
		reply(w, dict{"portProfile": []dict{{"id": 7, "name": "Default"}}})
	case r.Method == "GET" && path == "vmTemplate/all":
		reply(w, dict{"domain": append([]dict{}, f.templates...)})
	case r.Method == "GET" && path == "vm/vmList":
		if f.vmListFail && f.vmAdded {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		reply(w, dict{"domain": append([]dict{}, f.vms...)})
	case r.Method == "GET" && path == "vmTemplate/storage":
		if f.tplStorageFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if q.Get("vmId") != "90" {
			f.t.Errorf("vmTemplate/storage vmId=%s", q.Get("vmId"))
		}
		reply(w, dict{"data": []dict{
			// api.yaml RsTemplateStorage shape
			{"id": 1, "storeFile": "/vms/template/ubuntu-2204.qcow2", "device": "vda", "diskDevice": "disk", "driveType": "qcow2", "format": "Read-Write"},
			{"id": 2, "storeFile": "/vms/isos/castools.iso", "device": "hdc", "diskDevice": "cdrom", "format": "Read-Only"},
		}})
	case r.Method == "GET" && path == "storage/volume" && q.Get("poolName") == "tplpool":
		v := dict{"name": "ubuntu-2204.qcow2", "format": "qcow2", "size": 3072, "backingStore": f.tplBacking}
		if f.tplNoBackKey {
			delete(v, "backingStore")
		}
		if name := q.Get("volumeName"); len(name) > 0 && name != v["name"] {
			reply(w, dict{"storageVolume": []dict{}})
			return
		}
		reply(w, dict{"storageVolume": []dict{v}})
	case r.Method == "GET" && path == "storage/volume":
		if q.Get("poolName") != "defaultpool" {
			f.t.Errorf("storage/volume pool %s", q.Get("poolName"))
		}
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
		f.volumes = append(f.volumes, dict{"name": name, "format": "qcow2", "size": 3072, "backingStore": base, "users": f.overlayUsers})
		f.task(w, 8001)
	case r.Method == "POST" && path == "vm/add" && f.vmAddStatus != 0:
		if f.vmAddStCreates {
			f.vms = append(f.vms, dict{"id": 50, "uuid": "uuid-tmp", "name": "tplimp-ubuntu-2204", "vmStatus": "shutOff", "hostId": 1})
		}
		w.Header().Set("Error-Code", "999")
		w.WriteHeader(f.vmAddStatus)
	case r.Method == "POST" && path == "vm/add" && f.vmAdd500:
		w.WriteHeader(http.StatusInternalServerError)
	case r.Method == "POST" && path == "vm/add":
		f.vmAdded = true
		b, _ := jsonutils.Parse(body)
		name, _ := b.GetString("name")
		if f.dupVMOnAdd {
			f.vms = append(f.vms, dict{"id": 51, "uuid": "uuid-foreign", "name": name, "vmStatus": "shutOff", "hostId": 1})
		}
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
		if !f.failTask[8003] || f.tplLeftOnFail {
			f.templates = append(f.templates, dict{"id": 90, "name": q.Get("domainName"), "templetStoragePath": "/vms/template"})
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
		done := f.polls[id] >= 2
		if id == 8003 && f.tplNever {
			done = false
		}
		reply(w, dict{"msgId": id, "completed": done, "result": result, "failMsg": "injected"})
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
	if !strings.Contains(w[3], "delBase0=false&isWipeVolume=false&type=3") {
		t.Errorf("delete %s", w[3])
	}
	if s := res.Steps[len(res.Steps)-2]; s.Step != "check-template-flat" || s.Write || !strings.Contains(s.Detail, "/vms/template/ubuntu-2204.qcow2") {
		t.Errorf("flat check step %+v", s)
	}
	vol, _ := jsonutils.ParseString(f.bodies["POST storage/volume"])
	for k, v := range map[string]string{"baseFile": impSrcPath, "volName": "tplimp-ubuntu-2204-sys.qcow2", "format": "qcow2", "spName": "defaultpool"} {
		if got, _ := vol.GetString(k); got != v {
			t.Errorf("storage/volume %s = %q, want %q", k, got, v)
		}
	}
	if c, _ := vol.Int("capacity"); c != 3072 {
		t.Errorf("capacity = %d, want 3072 (next GiB above 2252.5 MB)", c)
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
		if s.Done || (!s.Write && s.Step != "check-template-flat") {
			t.Errorf("dry-run step %+v must be planned, not done", s)
		}
		steps = append(steps, s.Step)
	}
	if got := strings.Join(steps, ","); got != "refresh-pool,create-overlay,create-vm,clone-to-template,check-template-flat,delete-temp-vm" {
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
		"source has backing file": {func(f *impFake, o *SImageImportOptions) {
			f.volumes[0]["backingStore"] = "/vms/images/base.qcow2"
		}, cloudprovider.ErrNotSupported},
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
		"vm booted by CAS":             {func(f *impFake) { f.vmStatus = "running" }, "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp", true},
		"vm/add 400, no VM":            {func(f *impFake) { f.vmAddStatus = 400 }, "DELETE /cas/casrs/storage/volume/delete/1/defaultpool/tplimp-ubuntu-2204-sys.qcow2", false},
		"vm/add 409 is ambiguous":      {func(f *impFake) { f.vmAddStatus = 409 }, "POST /cas/casrs/vm/add", false},
		"vm/add 408 is ambiguous":      {func(f *impFake) { f.vmAddStatus = 408 }, "POST /cas/casrs/vm/add", false},
		"vm/add 429 is ambiguous":      {func(f *impFake) { f.vmAddStatus = 429 }, "POST /cas/casrs/vm/add", false},
		"vm/add 400 but VM exists":     {func(f *impFake) { f.vmAddStatus, f.vmAddStCreates = 400, true }, "POST /cas/casrs/vm/add", false},
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

// A VM that CAS booted despite autoBooting=0 is never templated.
func TestImportQcow2RefusesBootedVM(t *testing.T) {
	f := newImpFake(t)
	f.vmStatus = "running"
	_, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if errors.Cause(err) != cloudprovider.ErrInvalidStatus && !strings.Contains(fmt.Sprint(err), "refusing to template a booted image") {
		t.Fatalf("err = %v", err)
	}
	w := strings.Join(f.writes(), "\n")
	if strings.Contains(w, "addVmTemplate") {
		t.Fatalf("booted VM templated: %s", w)
	}
	if !strings.Contains(w, "PUT /cas/casrs/vm/powerOff/50") || !strings.Contains(w, "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp") {
		t.Fatalf("booted VM not stopped and recycled: %s", w)
	}
}

func hasDelete(w []string) bool {
	for _, h := range w {
		if strings.HasPrefix(h, "DELETE ") {
			return true
		}
	}
	return false
}

// B2: after vm/add was accepted, an ambiguous VM lookup leaves the overlay
// (and any VM) alone and reports what to remove by hand.
func TestImportQcow2AmbiguousAfterVMAddKeepsOverlay(t *testing.T) {
	cases := map[string]struct {
		mod  func(f *impFake)
		want string
	}{
		"duplicate temp VM name": {func(f *impFake) { f.dupVMOnAdd = true }, "2 VMs are named tplimp-ubuntu-2204"},
		"vmList 500 after add":   {func(f *impFake) { f.vmListFail = true }, "cannot list VMs"},
		"vm/add 500, no VM":      {func(f *impFake) { f.vmAdd500 = true }, "no VM named tplimp-ubuntu-2204 is listed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newImpFake(t)
			c.mod(f)
			res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			for _, must := range []string{"remove by hand", "VM tplimp-ubuntu-2204", "overlay /vms/images/tplimp-ubuntu-2204-sys.qcow2"} {
				if !strings.Contains(err.Error(), must) {
					t.Errorf("err lacks %q: %v", must, err)
				}
			}
			if w := f.writes(); hasDelete(w) {
				t.Fatalf("must not delete anything: %v", w)
			}
			if res == nil || res.Template != nil {
				t.Fatalf("res = %+v", res)
			}
			f.assertSourceUntouched(t)
		})
	}
}

// B2: vm/add rejected (400), but CAS lists a user on the overlay: re-read
// before delete and refuse.
func TestImportQcow2OverlayInUseNotDeleted(t *testing.T) {
	f := newImpFake(t)
	f.vmAddStatus = 400
	f.overlayUsers = "someone"
	_, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "is used by someone; not deleting it") {
		t.Fatalf("err = %v", err)
	}
	if w := f.writes(); hasDelete(w) {
		t.Fatalf("in-use overlay deleted: %v", w)
	}
}

// N1: addVmTemplate times out: the VM is not deleted while the clone may
// still run, and the (maybe half-made) template is reported.
func TestImportQcow2TemplateTimeoutKeepsVM(t *testing.T) {
	f := newImpFake(t)
	f.tplNever = true
	cli := f.client(t)
	cli.taskTimeout = 200 * time.Millisecond
	res, err := cli.ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "did not finish in time") || !strings.Contains(err.Error(), "uuid-tmp") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "template 90 \"ubuntu-2204\" exists after the failure") {
		t.Errorf("half-made template not reported: %v", err)
	}
	if res == nil || res.Template == nil || res.Template.Id != 90 {
		t.Fatalf("res = %+v", res)
	}
	if w := f.writes(); hasDelete(w) {
		t.Fatalf("deleted during a running clone: %v", w)
	}
}

// N1: the template task fails but CAS left a template: report it, recycle
// the VM.
func TestImportQcow2TemplateFailureReportsLeftover(t *testing.T) {
	f := newImpFake(t)
	f.failTask[8003] = true
	f.tplLeftOnFail = true
	res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "template 90 \"ubuntu-2204\" exists after the failure and may be incomplete") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Template == nil || res.Template.Id != 90 {
		t.Fatalf("res = %+v", res)
	}
	w := f.writes()
	if !strings.HasPrefix(w[len(w)-1], "DELETE /cas/casrs/vm/delete/uuid/uuid-tmp?delBase0=false") {
		t.Fatalf("last write %v", w)
	}
}

// B1: the template disk still has a backing file -> error, template kept
// and named, temp VM still recycled, nothing else deleted.
func TestImportQcow2TemplateBackedFails(t *testing.T) {
	f := newImpFake(t)
	f.tplBacking = impSrcPath
	res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil {
		t.Fatal("want error")
	}
	for _, must := range []string{"TemplateHasBackingFile", "template 90 \"ubuntu-2204\" was created and is kept", "backing file " + impSrcPath, "do NOT delete"} {
		if !strings.Contains(err.Error(), must) {
			t.Errorf("err lacks %q: %v", must, err)
		}
	}
	if res == nil || res.Template == nil || res.Template.Id != 90 {
		t.Fatalf("res = %+v", res)
	}
	w := f.writes()
	// R5: the temp VM stays live after a B1 error
	if len(w) != 3 || hasDelete(w) {
		t.Fatalf("writes = %v", w)
	}
	if !strings.Contains(err.Error(), "temporary VM tplimp-ubuntu-2204 (uuid-tmp) was kept live") {
		t.Errorf("temp VM not named: %v", err)
	}
	f.assertSourceUntouched(t)
}

// B1: when the template disks cannot be listed the import fails too.
func TestImportQcow2TemplateUnverifiedFails(t *testing.T) {
	f := newImpFake(t)
	f.tplStorageFail = true
	res, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "TemplateBackingUnverified") {
		t.Fatalf("err = %v", err)
	}
	if res == nil || res.Template == nil {
		t.Fatalf("res = %+v", res)
	}
	if w := f.writes(); hasDelete(w) || !strings.Contains(err.Error(), "kept live") {
		t.Fatalf("temp VM must stay live: %v %v", w, err)
	}
}

// R2: a template volume without a backingStore key is not "flat".
func TestImportQcow2TemplateBackingKeyMissing(t *testing.T) {
	f := newImpFake(t)
	f.tplNoBackKey = true
	_, err := f.client(t).ImportQcow2AsTemplate(impOpts())
	if err == nil || !strings.Contains(err.Error(), "TemplateBackingUnverified") || !strings.Contains(err.Error(), "no backingStore field") {
		t.Fatalf("err = %v", err)
	}
	if w := f.writes(); hasDelete(w) {
		t.Fatalf("temp VM must stay live: %v", w)
	}
}

// R1: classification uses diskDevice (api.yaml), not device.
func TestTemplateStorageIsDisk(t *testing.T) {
	for st, want := range map[STemplateStorage]bool{
		{Device: "vda", DiskDevice: "disk"}:   true,
		{Device: "hdc", DiskDevice: "cdrom"}:  false,
		{Device: "fda", DiskDevice: "floppy"}: false,
		{Device: "disk"}:                      true,
		{Device: "vda"}:                       true, // no kind: checked (fails closed)
		{}:                                    true,
	} {
		if got := st.isDisk(); got != want {
			t.Errorf("%+v: %v", st, got)
		}
	}
}

func TestImportQcow2LevelWarning(t *testing.T) {
	f := newImpFake(t)
	f.volumes[0]["level"] = 1
	o := impOpts()
	o.DryRun = true
	res, err := f.client(t).ImportQcow2AsTemplate(o)
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "level 1") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestOverlayCapacityMB(t *testing.T) {
	for in, want := range map[float64]int64{0.5: 1024, 1023.9: 1024, 1024: 2048, 2048: 3072, 2252.5: 3072, 10240: 11264} {
		if got := overlayCapacityMB(in); got != want || float64(got) <= in {
			t.Errorf("overlayCapacityMB(%v) = %d, want %d", in, got, want)
		}
	}
}

func TestIsDefiniteRejection(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"409":       {&SCasError{StatusCode: 409}, false},
		"408":       {&SCasError{StatusCode: 408}, false},
		"429":       {&SCasError{StatusCode: 429}, false},
		"499":       {&SCasError{StatusCode: 499}, false},
		"400":       {errors.Wrap(&SCasError{StatusCode: 400}, "x"), true},
		"403":       {&SCasError{StatusCode: 403}, true},
		"404 raw":   {&SCasError{StatusCode: 404}, true},
		"mapped":    {errors.Wrap(cloudprovider.ErrNotFound, "x"), false},
		"read-only": {errors.Wrap(cloudprovider.ErrAccountReadOnly, "x"), true},
		"500":       {&SCasError{StatusCode: 500}, false},
		"timeout":   {errors.Wrap(cloudprovider.ErrTimeout, "x"), false},
		"transport": {errors.Error("connection reset"), false},
	}
	for name, c := range cases {
		if got := isDefiniteRejection(c.err); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
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
