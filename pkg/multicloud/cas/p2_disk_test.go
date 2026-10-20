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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const p2PoolPath = "/vms/images"

type p2Vol struct {
	name, format string
	sizeMB       int64
	users        string
	level        int
}

type p2Disk struct {
	dev, path, format, bus string
	sizeMB                 int64
}

type p2Snap struct {
	name, parent string
}

// p2Fake is a stateful CAS: one host 7 in host pool 1, one local pool
// "defaultpool" at /vms/images, one VM (id 9, uuid-9, name vm9).
type p2Fake struct {
	t *testing.T

	lock      sync.Mutex
	hits      []string
	bodies    map[string][]jsonutils.JSONObject // "METHOD path" -> bodies
	vols      map[string]*p2Vol
	disks     []p2Disk
	snaps     []p2Snap
	vmStatus  string
	nextMsg   int64
	failMsg   map[string]bool // "METHOD path" -> task fails
	modifyAck int64           // msgId vm/modify returns; 0 = task
	resized   bool            // vm/modify really changes the size
	volDelay  map[string]int  // volumeName -> GETs to skip before appearing
	recycled  bool            // after DeleteVM type=3
	// undeployed: vm/vmList is empty until the deploy (S-1 name check)
	undeployed bool
}

func newP2Fake(t *testing.T) *p2Fake {
	return &p2Fake{
		t:        t,
		bodies:   map[string][]jsonutils.JSONObject{},
		vols:     map[string]*p2Vol{"vm9-sys.qcow2": {name: "vm9-sys.qcow2", format: "qcow2", sizeMB: 20480, users: "vm9"}},
		disks:    []p2Disk{{dev: "vda", path: p2PoolPath + "/vm9-sys.qcow2", format: "qcow2", bus: "virtio", sizeMB: 20480}},
		vmStatus: "shutOff",
		nextMsg:  3000,
		failMsg:  map[string]bool{},
		resized:  true,
	}
}

func (f *p2Fake) task(w http.ResponseWriter, key string) {
	f.nextMsg++
	id := f.nextMsg
	if f.failMsg[key] {
		id = -id // remembered as failing below
	}
	reply(w, dict{"msgId": abs64(id), "completed": false, "targetId": 9})
	if id < 0 {
		f.failMsg[fmt.Sprintf("msg/%d", -id)] = true
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (f *p2Fake) detail() dict {
	st := []dict{{"device": "cdrom", "deviceName": "hdc", "path": ""}}
	for _, d := range f.disks {
		st = append(st, dict{"device": "disk", "deviceName": d.dev, "path": d.path, "format": d.format, "targetBus": d.bus, "size": d.sizeMB, "cacheType": "directsync"})
	}
	return dict{"id": 9, "uuid": "uuid-9", "name": "vm9", "status": f.vmStatus, "storage": st, "network": []dict{}}
}

func (f *p2Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
	p := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	key := r.Method + " " + p
	var body jsonutils.JSONObject
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		body, _ = jsonutils.Parse(raw)
		f.bodies[key] = append(f.bodies[key], body)
	}
	q := r.URL.Query()
	parts := strings.Split(p, "/")
	switch {
	case r.Method == "GET" && p == "hostpool/all":
		reply(w, dict{"hostPool": []dict{{"id": 1, "name": "hp1"}}})
	case r.Method == "GET" && p == "host":
		reply(w, dict{"host": []dict{{"id": 7, "name": "cvk7", "hostPoolId": 1, "status": 1}}})
	case r.Method == "GET" && p == "host/id/7":
		reply(w, dict{"id": 7, "name": "cvk7", "hostPoolId": 1, "status": 1})
	case r.Method == "GET" && p == "vm/vmList":
		if f.undeployed {
			reply(w, dict{"domain": []dict{}})
			return
		}
		if f.recycled {
			reply(w, dict{"domain": []dict{{"id": 9, "uuid": "uuid-9", "name": "vm9", "title": "vm9", "hostId": 7, "hostPoolId": 1, "cpu": 1, "memory": 1024, "vmStatus": "shutOff", "system": 1, "delTime": 1603983250000}}})
			return
		}
		reply(w, dict{"domain": []dict{{"id": 9, "uuid": "uuid-9", "name": "vm9", "title": "vm9", "hostId": 7, "hostPoolId": 1, "cpu": 1, "memory": 1024, "vmStatus": f.vmStatus, "system": 1}}})
	case r.Method == "GET" && (p == "vm/detail/9" || p == "vm/detail/uuid/uuid-9"):
		reply(w, f.detail())
	case r.Method == "GET" && p == "vm/detail/all" && q.Get("hostId") == "7":
		reply(w, dict{"domain": []dict{f.detail()}})
	case r.Method == "GET" && p == "storage/pool":
		reply(w, dict{"storagePool": []dict{{"name": "defaultpool", "path": p2PoolPath, "type": "dir", "status": 1, "totalSize": 102400, "freeSize": 51200}}})
	case r.Method == "GET" && p == "host/id/7/storage":
		reply(w, dict{})
	case r.Method == "GET" && p == "storage/volume":
		if q.Get("hostId") != "7" || q.Get("poolName") != "defaultpool" {
			f.t.Errorf("storage/volume query %s", r.URL.RawQuery)
		}
		ret := []dict{}
		for _, v := range f.vols {
			if n := q.Get("volumeName"); len(n) > 0 && n != v.name {
				continue
			}
			if f.volDelay != nil {
				if left := f.volDelay[v.name]; left > 0 {
					f.volDelay[v.name] = left - 1
					continue
				}
			}
			ret = append(ret, dict{"name": v.name, "format": v.format, "size": v.sizeMB, "users": v.users, "level": v.level, "storagePoolName": "defaultpool"})
		}
		reply(w, dict{"storageVolume": ret})
	case r.Method == "POST" && p == "storage/volume":
		name, _ := body.GetString("volName")
		cap, _ := body.Int("capacity")
		format, _ := body.GetString("format")
		f.vols[name] = &p2Vol{name: name, format: format, sizeMB: cap}
		f.task(w, key)
	case r.Method == "DELETE" && len(parts) == 6 && strings.HasPrefix(p, "storage/volume/delete/7/defaultpool/"):
		delete(f.vols, parts[5])
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "PUT" && p == "vm/addDevice":
		st, _ := body.Get("storage")
		pt, _ := st.Int("pathType")
		bus, _ := st.GetString("targetBus")
		format, _ := st.GetString("format")
		var dpath string
		var size int64
		if pt == 0 {
			dpath, _ = st.GetString("path")
			if v := f.vols[strings.TrimPrefix(dpath, p2PoolPath+"/")]; v != nil {
				v.users, size = "vm9", v.sizeMB
			}
		} else {
			fn, _ := st.GetString("fileName")
			size, _ = st.Int("size")
			dpath = p2PoolPath + "/" + fn
			f.vols[fn] = &p2Vol{name: fn, format: format, sizeMB: size, users: "vm9"}
		}
		if !f.failMsg[key] {
			f.disks = append(f.disks, p2Disk{dev: fmt.Sprintf("vd%c", 'a'+len(f.disks)), path: dpath, format: format, bus: bus, sizeMB: size})
		}
		f.task(w, key)
	case r.Method == "PUT" && p == "vm/delDevice":
		st, _ := body.Get("storage")
		dev, _ := st.GetString("deviceName")
		for i := range f.disks {
			if f.disks[i].dev == dev {
				if v := f.vols[strings.TrimPrefix(f.disks[i].path, p2PoolPath+"/")]; v != nil {
					v.users = ""
				}
				f.disks = append(f.disks[:i], f.disks[i+1:]...)
				break
			}
		}
		f.task(w, key)
	case r.Method == "PUT" && p == "vm/modify":
		st, _ := body.Get("storage")
		dev, _ := st.GetString("deviceName")
		size, _ := st.Int("size")
		for i := range f.disks {
			if f.disks[i].dev == dev && f.resized {
				f.disks[i].sizeMB = size
			}
		}
		if f.modifyAck != 0 {
			reply(w, dict{"msgId": f.modifyAck, "completed": true})
			return
		}
		f.task(w, key)
	case r.Method == "GET" && p == "vm/snapshot/9":
		reply(w, f.snapTree(""))
	case r.Method == "POST" && p == "vm/snapshot":
		name, _ := body.GetString("name")
		parent := ""
		if len(f.snaps) > 0 {
			parent = f.snaps[len(f.snaps)-1].name
		}
		f.snaps = append(f.snaps, p2Snap{name: name, parent: parent})
		f.task(w, key)
	case r.Method == "PUT" && p == "vm/snapshot/resume":
		f.task(w, key)
	case r.Method == "DELETE" && len(parts) == 4 && parts[0] == "vm" && parts[1] == "snapshot" && parts[2] == "9":
		name, _ := url.PathUnescape(parts[3])
		found := false
		for i := range f.snaps {
			if f.snaps[i].name == name {
				f.snaps = append(f.snaps[:i], f.snaps[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			w.Header().Set("Error-Code", "1001")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.task(w, key)
	case r.Method == "PUT" && len(parts) == 3 && parts[0] == "vm" && (parts[1] == "stop" || parts[1] == "powerOff" || parts[1] == "start"):
		f.vmStatus = map[string]string{"stop": "shutOff", "powerOff": "shutOff", "start": "running"}[parts[1]]
		f.task(w, key)
	case r.Method == "DELETE" && strings.HasPrefix(p, "vm/delete/uuid/"):
		f.recycled = true
		f.task(w, key)
	case r.Method == "GET" && len(parts) == 2 && parts[0] == "message":
		var id int64
		fmt.Sscanf(parts[1], "%d", &id)
		result := TASK_RESULT_SUCCESS
		if f.failMsg[fmt.Sprintf("msg/%d", id)] {
			result = TASK_RESULT_FAILURE
		}
		reply(w, dict{"msgId": id, "completed": true, "result": result, "failMsg": "fake failure", "targetId": 9})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusBadRequest)
	}
}

// snapTree renders children of parent as CAS does: an object for one
// child, an array for several, nested under snapshots.snapshot.
func (f *p2Fake) snapTree(parent string) dict {
	kids := []dict{}
	for _, s := range f.snaps {
		if s.parent == parent {
			kid := dict{"name": s.name, "desc": "d-" + s.name, "creationTime": "2020-12-02 11:34:59"}
			if sub := f.snapTree(s.name); sub != nil {
				kid["snapshots"] = sub
			}
			kids = append(kids, kid)
		}
	}
	switch len(kids) {
	case 0:
		if parent == "" {
			return dict{}
		}
		return nil
	case 1:
		return dict{"snapshot": kids[0]}
	}
	return dict{"snapshot": kids}
}

func (f *p2Fake) writes() []string {
	f.lock.Lock()
	defer f.lock.Unlock()
	ret := []string{}
	for _, h := range f.hits {
		if !strings.HasPrefix(h, "GET ") {
			ret = append(ret, h)
		}
	}
	return ret
}

func (f *p2Fake) lastBody(key string) jsonutils.JSONObject {
	f.lock.Lock()
	defer f.lock.Unlock()
	b := f.bodies[key]
	if len(b) == 0 {
		f.t.Fatalf("no body for %s", key)
	}
	return b[len(b)-1]
}

func newP2Env(t *testing.T) (*p2Fake, *SRegion, *SHost, *SInstance) {
	f := newP2Fake(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	region := &SRegion{cli: cli}
	zone := &SZone{SHostPool: SHostPool{Id: 1, Name: "hp1"}, region: region}
	host := &SHost{Id: 7, Name: "cvk7", HostPoolId: 1, zone: zone}
	vm := &SInstance{Id: 9, Uuid: "uuid-9", Name: "vm9", HostId: 7, HostPoolId: 1, region: region, host: host}
	return f, region, host, vm
}

func localPool(t *testing.T, host *SHost) *SStorage {
	storages, err := host.getStorages()
	if err != nil || len(storages) != 1 {
		t.Fatalf("storages %v %v", storages, err)
	}
	return &storages[0]
}

func checkFields(t *testing.T, what string, obj jsonutils.JSONObject, want map[string]interface{}) {
	t.Helper()
	for k, v := range want {
		got, err := obj.Get(k)
		if err != nil {
			t.Errorf("%s.%s missing (%s)", what, k, obj)
			continue
		}
		if jsonutils.Marshal(v).String() != got.String() {
			t.Errorf("%s.%s = %s, want %v", what, k, got, v)
		}
	}
}

func TestParseDiskId(t *testing.T) {
	for id, want := range map[string][2]string{
		"host/7/defaultpool/a.qcow2": {"host/7/defaultpool", "a.qcow2"},
		"hostpool/1/share/data1":     {"hostpool/1/share", "data1"},
	} {
		s, v, err := parseDiskId(id)
		if err != nil || s != want[0] || v != want[1] {
			t.Errorf("parseDiskId(%s) = %s %s %v", id, s, v, err)
		}
	}
	for _, bad := range []string{"uuid-9/vda", "host/7/pool", "x/7/pool/v", "host/7/pool/", "host/7/pool/a/b"} {
		if _, _, err := parseDiskId(bad); errors.Cause(err) != cloudprovider.ErrInputParameter {
			t.Errorf("parseDiskId(%s): %v", bad, err)
		}
	}
}

func TestStableDiskIdAndListing(t *testing.T) {
	f, _, host, vm := newP2Env(t)
	// Leftovers / templates / ISOs must NOT appear as pool disks (B1).
	f.vols["free.qcow2"] = &p2Vol{name: "free.qcow2", format: "qcow2", sizeMB: 1024}
	f.vols["base.qcow2"] = &p2Vol{name: "base.qcow2", format: "qcow2", sizeMB: 1024, level: 1}
	f.vols["os.iso"] = &p2Vol{name: "os.iso", format: "iso", sizeMB: 600}
	disks, err := vm.GetIDisks()
	if err != nil || len(disks) != 1 {
		t.Fatalf("vm disks %v %v", disks, err)
	}
	if id := disks[0].GetGlobalId(); id != "host/7/defaultpool/vm9-sys.qcow2" {
		t.Errorf("sys disk id %s", id)
	}
	if disks[0].GetDiskType() != "sys" {
		t.Errorf("type %s", disks[0].GetDiskType())
	}
	pool := localPool(t, host)
	all, err := pool.GetIDisks()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, d := range all {
		ids = append(ids, d.GetGlobalId())
	}
	got := strings.Join(ids, ",")
	if got != "host/7/defaultpool/vm9-sys.qcow2" {
		t.Errorf("pool disks = %s (must not list unattached leftovers)", got)
	}
	// Known unattached id is still reachable for Delete / Attach.
	d, err := pool.GetIDiskById("host/7/defaultpool/free.qcow2")
	if err != nil || d.GetDiskSizeMB() != 1024 {
		t.Fatalf("GetIDiskById free: %v %v", d, err)
	}
}

func TestCreateIDisk(t *testing.T) {
	f, _, host, _ := newP2Env(t)
	pool := localPool(t, host)
	disk, err := pool.CreateIDisk(&cloudprovider.DiskCreateConfig{Name: "data1", SizeGb: 10})
	if err != nil {
		t.Fatalf("CreateIDisk: %v", err)
	}
	if disk.GetGlobalId() != "host/7/defaultpool/data1.qcow2" || disk.GetDiskSizeMB() != 10240 || disk.GetDiskType() != "data" {
		t.Errorf("disk %s %d %s", disk.GetGlobalId(), disk.GetDiskSizeMB(), disk.GetDiskType())
	}
	checkFields(t, "storage/volume", f.lastBody("POST storage/volume"), map[string]interface{}{
		"hostId": 7, "spName": "defaultpool", "volName": "data1.qcow2", "capacity": 10240, "format": "qcow2", "mode": 2,
	})
	// same name again: refused, nothing written
	n := len(f.writes())
	if _, err := pool.CreateIDisk(&cloudprovider.DiskCreateConfig{Name: "data1", SizeGb: 10}); errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Errorf("duplicate: %v", err)
	}
	for _, bad := range []*cloudprovider.DiskCreateConfig{{Name: "a/b", SizeGb: 1}, {Name: "x", SizeGb: 0}, {Name: "x", SizeGb: 1, SnapshotId: "s"}} {
		if _, err := pool.CreateIDisk(bad); err == nil {
			t.Errorf("CreateIDisk(%+v) should fail", bad)
		}
	}
	if len(f.writes()) != n {
		t.Errorf("rejected creates wrote: %v", f.writes()[n:])
	}
}

func TestDiskDeleteNoWipeAndRefusesAttached(t *testing.T) {
	f, _, host, vm := newP2Env(t)
	f.vols["free.qcow2"] = &p2Vol{name: "free.qcow2", format: "qcow2", sizeMB: 1024}
	pool := localPool(t, host)
	d, err := pool.GetIDiskById("host/7/defaultpool/free.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ws := f.writes()
	if len(ws) != 1 || ws[0] != "DELETE /cas/casrs/storage/volume/delete/7/defaultpool/free.qcow2?isWipeVolume=false" {
		t.Errorf("writes = %v", ws)
	}
	// deleting again: gone, no-op
	if err := d.Delete(context.Background()); err != nil {
		t.Errorf("second delete: %v", err)
	}
	// attached disk (via VM) is refused
	disks, _ := vm.GetIDisks()
	if err := disks[0].Delete(context.Background()); errors.Cause(err) != cloudprovider.ErrInvalidStatus {
		t.Errorf("attached delete: %v", err)
	}
	// unattached handle, but attached since the last listing: refused
	f.vols["late.qcow2"] = &p2Vol{name: "late.qcow2", format: "qcow2", sizeMB: 1}
	late, err := pool.GetIDiskById("host/7/defaultpool/late.qcow2")
	if err != nil {
		t.Fatal(err)
	}
	f.lock.Lock()
	f.vols["late.qcow2"].users = "vm9"
	f.lock.Unlock()
	if err := late.Delete(context.Background()); errors.Cause(err) != cloudprovider.ErrInvalidStatus {
		t.Errorf("late-attached delete: %v", err)
	}
	if n := len(f.writes()); n != 1 {
		t.Errorf("writes = %v", f.writes())
	}
}

func TestDiskResize(t *testing.T) {
	f, _, _, vm := newP2Env(t)
	disks, _ := vm.GetIDisks()
	sys := disks[0]
	if err := sys.Resize(context.Background(), 10240); errors.Cause(err) != cloudprovider.ErrInputParameter {
		t.Errorf("shrink: %v", err)
	}
	if err := sys.Resize(context.Background(), 20480); err != nil {
		t.Errorf("same size: %v", err)
	}
	if n := len(f.writes()); n != 0 {
		t.Fatalf("shrink/same wrote %v", f.writes())
	}
	if err := sys.Resize(context.Background(), 30720); err != nil {
		t.Fatalf("grow: %v", err)
	}
	checkFields(t, "modify", f.lastBody("PUT vm/modify"), map[string]interface{}{"id": 9})
	st, _ := f.lastBody("PUT vm/modify").Get("storage")
	checkFields(t, "modify.storage", st, map[string]interface{}{"device": "disk", "deviceName": "vda", "format": "qcow2", "size": 30720, "cacheType": "directsync"})
	if sys.GetDiskSizeMB() != 30720 {
		t.Errorf("size after = %d", sys.GetDiskSizeMB())
	}
	// msgId -1: done inline, no message poll
	f.modifyAck = MSG_ID_NO_TASK
	if err := sys.Resize(context.Background(), 40960); err != nil {
		t.Fatalf("grow -1: %v", err)
	}
	// CAS accepted but size unchanged: error
	f.modifyAck = 0
	f.resized = false
	if err := sys.Resize(context.Background(), 51200); err == nil || !strings.Contains(err.Error(), "after resize") {
		t.Errorf("unchanged size: %v", err)
	}
	// unattached volume: not supported
	free := volumeDisk(&SStorage{SStoragePool: SStoragePool{Name: "defaultpool", Path: p2PoolPath}}, &SStorageVolume{Name: "x.qcow2", Format: "qcow2", Size: 1})
	if err := free.Resize(context.Background(), 2048); errors.Cause(err) != cloudprovider.ErrNotSupported {
		t.Errorf("unattached resize: %v", err)
	}
}

func TestWaitStorageVolume(t *testing.T) {
	f, _, host, _ := newP2Env(t)
	pool := localPool(t, host)
	// Delay appearance: first N GETs omit the new volume.
	f.lock.Lock()
	f.volDelay = map[string]int{"late.qcow2": 2}
	f.lock.Unlock()
	disk, err := pool.CreateIDisk(&cloudprovider.DiskCreateConfig{Name: "late", SizeGb: 1})
	if err != nil {
		t.Fatalf("CreateIDisk: %v", err)
	}
	if disk.GetGlobalId() != "host/7/defaultpool/late.qcow2" {
		t.Errorf("id %s", disk.GetGlobalId())
	}
}

func TestRegionGetIDiskById(t *testing.T) {
	f, region, _, vm := newP2Env(t)
	_ = f
	disks, _ := vm.GetIDisks()
	id := disks[0].GetGlobalId()
	got, err := region.GetIDiskById(id)
	if err != nil || got.GetGlobalId() != id {
		t.Fatalf("GetIDiskById(%s) = %v %v", id, got, err)
	}
	if _, err := region.GetIDiskById("uuid-9/vda"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("legacy id: %v", err)
	}
	if _, err := region.GetIDiskById("host/7/defaultpool/missing.qcow2"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestDiskIdSkipsLegacyOnTransientStorageError(t *testing.T) {
	// A disk with storage pre-set keeps the stable id even if a later
	// getStorage would fail; GetIDisks surfaces transient errors.
	st := &SStorage{SStoragePool: SStoragePool{Name: "defaultpool", Path: p2PoolPath}, host: &SHost{Id: 7, HostPoolId: 1}}
	d := SDisk{
		SInstanceStorage: SInstanceStorage{Device: "disk", DeviceName: "vda", Path: p2PoolPath + "/vm9-sys.qcow2"},
		ins:              &SInstance{Uuid: "uuid-9"},
		storage:          st,
	}
	if id := d.GetId(); id != "host/7/defaultpool/vm9-sys.qcow2" {
		t.Errorf("stable id with storage set: %s", id)
	}
	d.storage = nil
	d.ins = &SInstance{Uuid: "uuid-9", region: &SRegion{cli: &SCasClient{CasClientConfig: NewCasClientConfig("http://127.0.0.1:1", "u", "p")}}}
	// No host → getStorage fails with a non-NotFound error; must not flip to uuid/vda.
	id := d.GetId()
	if id == "uuid-9/vda" {
		t.Errorf("transient/missing host flipped to legacy id %s", id)
	}
	if id != p2PoolPath+"/vm9-sys.qcow2" {
		t.Errorf("want path stand-in, got %s", id)
	}
}
