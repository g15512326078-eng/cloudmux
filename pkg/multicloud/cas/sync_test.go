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
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

type dict = map[string]interface{}

// fakeCloud is a small, self-consistent CAS (unlike the api.yaml examples)
// that honours the query parameters the provider sends, and counts hits.
type fakeCloud struct {
	t *testing.T

	hosts []dict
	vms   []fakeVM

	ignoreOffset bool
	ignoreLimit  bool
	notFound409  bool // unknown vm/detail/uuid -> 409 "不存在" instead of 404

	lock sync.Mutex
	hits map[string]int // "path?sorted query" -> count
}

type fakeVM struct {
	id      int
	hostId  int
	uuid    string
	ip      string // guest IP in vm/detail; default 10.0.0.<id>
	hidden  bool   // has a detail but is missing from vm/vmList
	cpu     int    // vmList cpu; default 2
	delTime string // non-empty => recycle bin
}

// updateVM changes a VM between requests (under the lock the handler uses).
func (f *fakeCloud) updateVM(id int, fn func(vm *fakeVM)) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for i := range f.vms {
		if f.vms[i].id == id {
			fn(&f.vms[i])
		}
	}
}

func newFakeCloud(t *testing.T, nHosts, nVMs int) *fakeCloud {
	f := &fakeCloud{t: t, hits: map[string]int{}}
	for h := 1; h <= nHosts; h++ {
		f.hosts = append(f.hosts, dict{
			"id": h, "name": fmt.Sprintf("cvk%d", h), "hostPoolId": 1, "clusterId": 1,
			"ip": fmt.Sprintf("192.168.0.%d", h), "cpuCount": 8, "cpuSockets": 1, "memorySize": 16384,
			"status": 1, "user": "root", "pwd": "HOST-SECRET",
		})
	}
	for i := 1; i <= nVMs; i++ {
		f.vms = append(f.vms, fakeVM{id: i, hostId: (i-1)%nHosts + 1, uuid: fmt.Sprintf("uuid-%d", i)})
	}
	return f
}

func (f *fakeCloud) start() (*SCasClient, *httptest.Server) {
	srv := httptest.NewServer(f)
	f.t.Cleanup(srv.Close)
	cfg := NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})
	return &SCasClient{CasClientConfig: cfg}, srv
}

func (f *fakeCloud) vmList(vm fakeVM) dict {
	cpu := vm.cpu
	if cpu == 0 {
		cpu = 2
	}
	d := dict{"id": vm.id, "uuid": vm.uuid, "name": fmt.Sprintf("vm%d", vm.id), "title": fmt.Sprintf("VM %d", vm.id),
		"hostId": vm.hostId, "hostPoolId": 1, "clusterId": 1, "cpu": cpu, "memory": 2048, "vmStatus": "running", "system": 1}
	if len(vm.delTime) > 0 {
		d["delTime"] = vm.delTime
	}
	return d
}

func (f *fakeCloud) vmDetail(vm fakeVM) dict {
	ip := vm.ip
	if len(ip) == 0 {
		ip = fmt.Sprintf("10.0.0.%d", vm.id)
	}
	return dict{"id": vm.id, "uuid": vm.uuid, "name": fmt.Sprintf("vm%d", vm.id), "status": "running",
		"graphics": []dict{{"type": "vnc", "port": 5900 + vm.id, "password": "VNC-SECRET"}},
		"storage": []dict{
			{"device": "disk", "deviceName": "vda", "path": fmt.Sprintf("/vms/images/vm%d", vm.id), "size": 20480, "format": "qcow2", "targetBus": "virtio"},
			{"device": "cdrom", "deviceName": "hdc", "path": ""},
			{"device": "disk", "deviceName": "vdb", "path": fmt.Sprintf("/vms/share1/vm%d-data", vm.id), "size": 10240, "format": "qcow2", "targetBus": "virtio"},
		},
		"network": []dict{{"netType": 0, "mac": fmt.Sprintf("0C:DA:41:00:00:%02x", vm.id), "ipAddr": ip,
			"vsName": "vswitch0", "deviceModel": "virtio"}},
	}
}

func (f *fakeCloud) hitCount(prefix string) int {
	f.lock.Lock()
	defer f.lock.Unlock()
	n := 0
	for k, v := range f.hits {
		if strings.HasPrefix(k, prefix) {
			n += v
		}
	}
	return n
}

func (f *fakeCloud) total() int {
	return f.hitCount("")
}

func reply(w http.ResponseWriter, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(jsonutils.Marshal(obj).String()))
}

func (f *fakeCloud) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	q := r.URL.Query()
	f.lock.Lock()
	f.hits[path+"?"+q.Encode()]++
	vms := append([]fakeVM{}, f.vms...)
	f.lock.Unlock()
	qint := func(k string) int {
		v, _ := strconv.Atoi(q.Get(k))
		return v
	}
	parts := strings.Split(path, "/")
	switch {
	case path == "hostpool/all":
		reply(w, dict{"hostPool": []dict{{"id": 1, "name": "hp1"}}})
	case path == "host":
		ret := []dict{}
		for _, h := range f.hosts {
			ret = append(ret, dict{"id": h["id"], "name": h["name"], "ip": h["ip"], "status": 1})
		}
		reply(w, dict{"host": ret})
	case len(parts) == 3 && parts[0] == "host" && parts[1] == "id":
		id, _ := strconv.Atoi(parts[2])
		if id < 1 || id > len(f.hosts) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(w, f.hosts[id-1])
	case len(parts) == 4 && parts[0] == "host" && parts[3] == "storage":
		reply(w, dict{"storagePool": dict{"name": "share1", "type": "fs", "path": "/vms/share1", "totalSize": 1000, "freeSize": 400}})
	case len(parts) == 4 && parts[0] == "host" && parts[3] == "vswitch":
		hostId, _ := strconv.Atoi(parts[2])
		vs := []dict{{"id": hostId*10 + 0, "hostId": hostId, "name": "vswitch0", "status": 1}}
		if hostId == 1 {
			vs = append(vs, dict{"id": 11, "hostId": 1, "name": "vswitch1", "status": 1})
		}
		reply(w, dict{"vSwitch": vs})
	case path == "storage/pool":
		reply(w, dict{"storagePool": []dict{
			{"name": "defaultpool", "type": "dir", "path": "/vms/images", "status": 1, "totalSize": 500, "freeSize": 100},
			{"name": "share1", "type": "fs", "path": "/vms/share1", "status": 1, "totalSize": 1000, "freeSize": 400},
		}})
	case path == "vm/vmList":
		match := []dict{}
		for _, vm := range vms {
			// api.yaml: domainIdList is only honoured together with hpId
			if vm.hidden || (q.Has("hostId") && qint("hostId") != vm.hostId) || (q.Has("hpId") && qint("hpId") != 1) ||
				(q.Has("hpId") && q.Has("domainIdList") && qint("domainIdList") != vm.id) {
				continue
			}
			match = append(match, f.vmList(vm))
		}
		offset, limit := qint("offset"), qint("limit")
		if f.ignoreOffset {
			offset = 0
		}
		if q.Has("limit") && !f.ignoreLimit {
			if offset > len(match) {
				offset = len(match)
			}
			end := offset + limit
			if end > len(match) {
				end = len(match)
			}
			match = match[offset:end]
		}
		reply(w, dict{"domain": match})
	case len(parts) == 4 && parts[0] == "vm" && parts[1] == "detail" && parts[2] == "uuid":
		for _, vm := range vms {
			if vm.uuid == parts[3] {
				reply(w, f.vmDetail(vm))
				return
			}
		}
		if f.notFound409 {
			w.Header().Set("Error-Code", "1001")
			w.Header().Set("Error-Message", "虚拟机不存在。")
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case path == "vm/detail/all":
		ret := []dict{}
		for _, vm := range vms {
			if qint("hostId") == vm.hostId {
				ret = append(ret, f.vmDetail(vm))
			}
		}
		reply(w, dict{"domain": ret})
	case len(parts) == 3 && parts[0] == "vm" && parts[1] == "detail":
		id, _ := strconv.Atoi(parts[2])
		for _, vm := range vms {
			if vm.id == id {
				reply(w, f.vmDetail(vm))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case path == "vmTemplate/all":
		reply(w, dict{"domain": dict{"id": 3, "name": "centos7", "storageCapacity": 20480, "system": 1}})
	default:
		f.t.Errorf("unexpected request %s", r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func uuids(vms []SInstance) string {
	ret := []string{}
	for i := range vms {
		ret = append(ret, vms[i].Uuid)
	}
	return strings.Join(ret, ",")
}

func TestListPaged(t *testing.T) {
	cases := []struct {
		name         string
		nVMs         int
		ignoreOffset bool
		ignoreLimit  bool
		wantPages    int
		wantErr      bool
	}{
		{name: "short last page", nVMs: 5, wantPages: 3},   // 2,2,1
		{name: "exact multiple", nVMs: 4, wantPages: 3},    // 2,2,0
		{name: "single short page", nVMs: 1, wantPages: 1}, // 1
		{name: "empty", nVMs: 0, wantPages: 1},             // 0
		{name: "limit ignored", nVMs: 5, ignoreLimit: true, wantPages: 1},
		{name: "offset ignored", nVMs: 5, ignoreOffset: true, wantPages: 2, wantErr: true},
	}
	for _, c := range cases {
		f := newFakeCloud(t, 1, c.nVMs)
		f.ignoreOffset, f.ignoreLimit = c.ignoreOffset, c.ignoreLimit
		cli, _ := f.start()
		cli.pageSize = 2
		vms, err := cli.GetInstances(nil)
		if pages := f.hitCount("vm/vmList"); pages != c.wantPages {
			t.Errorf("%s: %d page requests, want %d", c.name, pages, c.wantPages)
		}
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error instead of a possibly truncated list, got %d vms", c.name, len(vms))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		want := []string{}
		for i := 1; i <= c.nVMs; i++ {
			want = append(want, fmt.Sprintf("uuid-%d", i))
		}
		if uuids(vms) != strings.Join(want, ",") {
			t.Errorf("%s: got %s", c.name, uuids(vms))
		}
	}
	// offsets actually sent
	f := newFakeCloud(t, 1, 5)
	cli, _ := f.start()
	cli.pageSize = 2
	if _, err := cli.GetInstances(nil); err != nil {
		t.Fatal(err)
	}
	for _, off := range []string{"0", "2", "4"} {
		if f.hitCount("vm/vmList?limit=2&offset="+off) != 1 {
			t.Errorf("offset %s not requested exactly once: %v", off, f.hits)
		}
	}
}

func TestGetInstanceByUuid(t *testing.T) {
	f := newFakeCloud(t, 2, 5)
	cli, _ := f.start()
	vm, err := cli.GetInstance("uuid-4")
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if vm.Id != 4 || vm.HostId != 2 || vm.HostPoolId != 1 || vm.detail == nil || len(vm.detail.disks) != 3 {
		t.Errorf("vm = %+v", vm)
	}
	for _, k := range []string{"vm/detail/uuid/uuid-4?", "hostpool/all?", "vm/vmList?domainIdList=4&hpId=1"} {
		if f.hitCount(k) != 1 {
			t.Errorf("expected one %s request: %v", k, f.hits)
		}
	}
	if n := f.total(); n != 3 {
		t.Errorf("GetInstance used %d requests, want 3: %v", n, f.hits)
	}

	// not found: 404, and 409 with a "不存在" message
	for _, use409 := range []bool{false, true} {
		f.notFound409 = use409
		if _, err := cli.GetInstance("nope"); errors.Cause(err) != cloudprovider.ErrNotFound {
			t.Errorf("GetInstance(nope) 409=%v: %v, want ErrNotFound", use409, err)
		}
	}
	if _, err := cli.GetInstance(""); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("GetInstance(\"\") = %v", err)
	}

	// a detail without a list entry is an error, never NotFound (no "lost")
	f.updateVM(5, func(v *fakeVM) { v.hidden = true })
	before := f.hitCount("vm/vmList?limit=")
	if _, err := cli.GetInstance("uuid-5"); err == nil || errors.Cause(err) == cloudprovider.ErrNotFound {
		t.Errorf("GetInstance(hidden) = %v, want a non-NotFound error", err)
	}
	if f.hitCount("vm/vmList?limit=") != before+1 {
		t.Errorf("expected the full paged list fallback")
	}
}

func walkRegion(t *testing.T, cli *SCasClient) (int, int, int) {
	regions, err := cli.GetIRegions()
	if err != nil {
		t.Fatal(err)
	}
	region := regions[0]
	nVM, nDisk, nNic := 0, 0, 0
	zones, err := region.GetIZones()
	if err != nil {
		t.Fatal(err)
	}
	for _, zone := range zones {
		if _, err := zone.GetIStorages(); err != nil {
			t.Fatal(err)
		}
		hosts, err := zone.GetIHosts()
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range hosts {
			if _, err := host.GetIHostNics(); err != nil {
				t.Fatal(err)
			}
			storages, err := host.GetIStorages()
			if err != nil {
				t.Fatal(err)
			}
			for _, st := range storages {
				if _, err := st.GetIDisks(); err != nil {
					t.Fatal(err)
				}
			}
			vms, err := host.GetIVMs()
			if err != nil {
				t.Fatal(err)
			}
			for _, vm := range vms {
				nVM++
				disks, err := vm.GetIDisks()
				if err != nil {
					t.Fatal(err)
				}
				for _, disk := range disks {
					nDisk++
					if _, err := disk.GetIStorage(); err != nil {
						t.Fatalf("disk %s storage: %v", disk.GetGlobalId(), err)
					}
				}
				nics, err := vm.GetINics()
				if err != nil {
					t.Fatal(err)
				}
				nNic += len(nics)
			}
		}
	}
	vpcs, err := region.GetIVpcs()
	if err != nil {
		t.Fatal(err)
	}
	for _, vpc := range vpcs {
		wires, err := vpc.GetIWires()
		if err != nil {
			t.Fatal(err)
		}
		for _, wire := range wires {
			if _, err := wire.GetINetworks(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := region.GetIVMs(); err != nil {
		t.Fatal(err)
	}
	caches, err := region.GetIStoragecaches()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caches {
		if _, err := c.GetICloudImages(); err != nil {
			t.Fatal(err)
		}
	}
	return nVM, nDisk, nNic
}

func TestSyncRequestCount(t *testing.T) {
	const nHosts = 2
	// hostpool/all + host + vmTemplate/all + one vm/vmList page, plus per
	// host: host/id/{id}, storage/pool, host/id/{id}/storage,
	// host/id/{id}/vswitch, vm/detail/all?hostId=.
	want := 4 + 5*nHosts
	for _, nVMs := range []int{6, 60} {
		f := newFakeCloud(t, nHosts, nVMs)
		cli, _ := f.start()
		nVM, nDisk, nNic := walkRegion(t, cli)
		if nVM != nVMs || nDisk != 2*nVMs || nNic != nVMs {
			t.Errorf("%d vms: saw %d vms %d disks %d nics", nVMs, nVM, nDisk, nNic)
		}
		if got := f.total(); got != want {
			t.Errorf("%d vms: %d requests, want %d: %v", nVMs, got, want, f.hits)
		}
		for k := range f.hits {
			if strings.HasPrefix(k, "vm/detail/") && !strings.HasPrefix(k, "vm/detail/all?") {
				t.Errorf("per-VM detail request %s", k)
			}
		}
		if n := f.hitCount("vm/vmList"); n != 1 {
			t.Errorf("%d vm/vmList requests, want 1", n)
		}
	}
	// paging adds only list pages
	f := newFakeCloud(t, nHosts, 60)
	cli, _ := f.start()
	cli.pageSize = 25
	walkRegion(t, cli)
	if got := f.total(); got != want-1+3 {
		t.Errorf("paged: %d requests, want %d", got, want+2)
	}
}

func TestDetailFallbackForMovedVM(t *testing.T) {
	f := newFakeCloud(t, 2, 2)
	cli, _ := f.start()
	region := cli.GetRegion()
	vms, err := region.cachedInstances()
	if err != nil {
		t.Fatal(err)
	}
	vm := vms[0]
	vm.HostId = 2 // list says host 2, but host 2's bulk detail does not have it
	disks, err := vm.GetIDisks()
	if err != nil || len(disks) != 2 {
		t.Fatalf("GetIDisks = %v, %v", disks, err)
	}
	if f.hitCount(fmt.Sprintf("vm/detail/%d", vm.Id)) != 1 {
		t.Errorf("expected a vm/detail/%d fallback: %v", vm.Id, f.hits)
	}
}

func TestNetworkTopology(t *testing.T) {
	// host 1 has vswitch0+vswitch1, host 2 only vswitch0, both in hostpool 1.
	f := newFakeCloud(t, 2, 3)
	cli, _ := f.start()
	region := cli.GetRegion()
	vpcs, err := region.GetIVpcs()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	networkIds := map[string]bool{}
	for _, vpc := range vpcs {
		wires, err := vpc.GetIWires()
		if err != nil {
			t.Fatal(err)
		}
		for _, wire := range wires {
			nets, err := wire.GetINetworks()
			if err != nil || len(nets) != 1 {
				t.Fatalf("wire %s networks = %v, %v", wire.GetGlobalId(), nets, err)
			}
			if wire.GetIVpc().GetGlobalId() != vpc.GetGlobalId() || wire.GetIZone().GetGlobalId() != "1" {
				t.Errorf("wire %s vpc/zone mismatch", wire.GetGlobalId())
			}
			networkIds[nets[0].GetGlobalId()] = true
			got = append(got, vpc.GetGlobalId()+">"+wire.GetGlobalId()+">"+nets[0].GetGlobalId())
		}
	}
	want := "vswitch/vswitch0>vswitch/vswitch0/hostpool/1>vswitch/vswitch0/hostpool/1/default," +
		"vswitch/vswitch1>vswitch/vswitch1/hostpool/1>vswitch/vswitch1/hostpool/1/default"
	if strings.Join(got, ",") != want {
		t.Errorf("topology = %v\nwant %s", got, want)
	}
	if _, err := region.GetIVpcById("vswitch/vswitch1"); err != nil {
		t.Errorf("GetIVpcById: %v", err)
	}
	// every guest NIC points at a synced network
	vms, err := region.GetIVMs()
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		nics, err := vm.GetINics()
		if err != nil {
			t.Fatal(err)
		}
		for _, nic := range nics {
			if !networkIds[nic.GetINetworkId()] {
				t.Errorf("vm %s nic network %q not synced", vm.GetGlobalId(), nic.GetINetworkId())
			}
		}
	}
	// host nics: one per wire of the host
	for hostId, want := range map[string]int{"1": 2, "2": 1} {
		host, err := region.GetIHostById(hostId)
		if err != nil {
			t.Fatal(err)
		}
		nics, err := host.GetIHostNics()
		if err != nil || len(nics) != want {
			t.Errorf("host %s nics = %d, %v, want %d", hostId, len(nics), err, want)
		}
	}
	nic := &SInstanceNic{SInstanceNetwork: SInstanceNetwork{NetType: 2}, ins: &SInstance{HostPoolId: 1}}
	if nic.GetINetworkId() != "" {
		t.Errorf("nic without vswitch -> %q", nic.GetINetworkId())
	}
}

func TestCheapFixes(t *testing.T) {
	f := newFakeCloud(t, 1, 3)
	cli, _ := f.start()
	// mark vm 2 as VMware via a wrapper handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == API_PREFIX+"/vm/vmList" {
			rec := httptest.NewRecorder()
			f.ServeHTTP(rec, r)
			obj, _ := jsonutils.Parse(rec.Body.Bytes())
			items, _ := obj.GetArray("domain")
			for _, it := range items {
				if id, _ := it.Int("id"); id == 2 {
					it.(*jsonutils.JSONDict).Set("type", jsonutils.NewInt(VM_TYPE_VMWARE))
				}
			}
			reply(w, obj)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cli.accessUrl = srv.URL
	vms, err := cli.GetInstances(nil)
	if err != nil || uuids(vms) != "uuid-1,uuid-3" {
		t.Errorf("GetInstances = %s, %v (VMware VM must be filtered)", uuids(vms), err)
	}
	if _, err := cli.GetInstance("uuid-2"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("GetInstance(VMware) = %v, want ErrNotFound", err)
	}

	vm := &SInstance{detail: &SInstanceDetail{nics: []SInstanceNetwork{{NetType: 0, VsName: "vs0"}, {NetType: NIC_NET_TYPE_NONE}}}}
	if nics, _ := vm.GetINics(); len(nics) != 1 {
		t.Errorf("netType none not skipped: %d nics", len(nics))
	}
	for capMB, want := range map[int64]int{0: 0, 1024: 1, 1025: 2, 20480: 20} {
		img := &SImage{SVmTemplate: SVmTemplate{StorageCapacity: capMB}}
		if got := img.GetMinOsDiskSizeGb(); got != want {
			t.Errorf("MinOsDisk(%d MB) = %d, want %d", capMB, got, want)
		}
	}
}

func TestNicIPv6NotReported(t *testing.T) {
	nic := &SInstanceNic{SInstanceNetwork: SInstanceNetwork{IpAddr: "10.0.0.5", Ipv6: "fd00::5", VsName: "vswitch0"}, ins: &SInstance{HostPoolId: 1}}
	if nic.GetIP6() != "" {
		t.Errorf("GetIP6 = %q, want \"\" (placeholder network has no v6 range)", nic.GetIP6())
	}
	if nic.GetIP() != "10.0.0.5" || nic.GetINetworkId() != "vswitch/vswitch0/hostpool/1/default" {
		t.Errorf("v4 part changed: %s %s", nic.GetIP(), nic.GetINetworkId())
	}
	net := &SNetwork{wire: &SWire{vpc: &SVpc{VSwitchName: "vswitch0"}, zone: &SZone{SHostPool: SHostPool{Id: 1}}}}
	if net.GetIp6Start() != "" {
		t.Errorf("placeholder network unexpectedly has a v6 range %q; GetIP6 could be re-enabled", net.GetIp6Start())
	}
}

func TestRefreshUpdatesDetail(t *testing.T) {
	f := newFakeCloud(t, 1, 2)
	cli, _ := f.start()
	region := cli.GetRegion()
	vms, err := region.GetIVMs()
	if err != nil {
		t.Fatal(err)
	}
	vm := vms[0].(*SInstance)
	nics, err := vm.GetINics() // fills vm.detail from the cached bulk detail
	if err != nil || len(nics) != 1 || nics[0].GetIP() != "10.0.0.1" || vm.GetVcpuCount() != 2 {
		t.Fatalf("before: nics %v %v cpu %d", nics, err, vm.GetVcpuCount())
	}
	f.updateVM(1, func(v *fakeVM) { v.ip, v.cpu = "10.9.9.9", 4 })
	if err := vm.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	nics, err = vm.GetINics()
	if err != nil || len(nics) != 1 || nics[0].GetIP() != "10.9.9.9" {
		t.Errorf("after Refresh nic ip = %v, %v, want 10.9.9.9 (detail kept)", nics, err)
	}
	if vm.GetVcpuCount() != 4 {
		t.Errorf("after Refresh cpu = %d, want 4", vm.GetVcpuCount())
	}
	// the region cache still holds the old bulk detail; Refresh must not read it
	cached, _ := region.cachedHostDetails(1)
	if cached["uuid-1"].nics[0].IpAddr != "10.0.0.1" {
		t.Errorf("test premise: cache should still be stale")
	}
}

func TestRecycleBinFilteredEverywhere(t *testing.T) {
	f := newFakeCloud(t, 1, 3)
	f.vms[1].delTime = "2020-10-29T22:54:10+08:00" // uuid-2 recycled
	cli, _ := f.start()
	cli.pageSize = 10

	vms, err := cli.GetInstances(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := uuids(vms)
	if got != "uuid-1,uuid-3" {
		t.Errorf("GetInstances = %s, recycled uuid-2 must be absent", got)
	}

	if _, err := cli.GetInstance("uuid-2"); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("GetInstance(recycled) = %v, want ErrNotFound", err)
	}

	region := cli.GetRegion()
	ivms, err := region.GetIVMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ivms) != 2 {
		t.Errorf("GetIVMs = %d, want 2", len(ivms))
	}
	hosts, err := region.GetIHosts()
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts = %v, %v", hosts, err)
	}
	hivms, err := hosts[0].GetIVMs()
	if err != nil || len(hivms) != 2 {
		t.Errorf("host GetIVMs = %d, %v", len(hivms), err)
	}
	// bulk details must not keep the recycled uuid
	details, err := region.cachedHostDetails(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := details["uuid-2"]; ok {
		t.Errorf("cachedHostDetails still has recycled uuid-2: %v", details)
	}
}
