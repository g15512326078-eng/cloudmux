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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	winISO    = "win2019.iso"
	winTpl    = "win2019"
	winTempVM = "tplwin-win2019"
	winDisk   = "tplwin-win2019-sys.qcow2"
	winAnswer = "tplwin-win2019-answer.iso"
)

// winFake is an httptest CAS for the Windows pipeline.
type winFake struct {
	t    *testing.T
	lock sync.Mutex
	hits []string
	// bodies by "METHOD path", all of them in order
	bodies  []string
	vols    map[string][]dict // pool -> volumes
	vms     []dict
	tpls    []dict
	storage []dict // vm/detail storage of VM 50
	polls   map[int64]int
	fail    map[int64]bool

	installPolls int // vmList polls while running before the guest shuts off
	running      int

	vmAddStatus   int  // vm/add -> this status
	vmAddCreates  bool // ... the VM exists anyway
	extraDisk     bool // detail lists a second disk
	startStatus   int  // vm/start -> this status
	startNoop     bool // start "succeeds" but the VM stays shut off
	modifyStatus  int  // vm/modify -> this status
	modifyNoop    bool // vm/modify succeeds but detaches nothing
	tplFail       bool
	tplBacking    string
	deleteVMFail  bool
	castools      string
	onStart       func()
	deleteAnswers int

	// disk-wipe guard knobs
	diskVol          func(v dict)  // edit the new disk's volume entry
	diskDetail       func(st dict) // edit the disk's vm/detail entry
	extraStorage     []dict        // more vm/detail entries
	diskHidden       bool          // disk name only listed after a refresh
	installAlloc     float64       // allocation after the install (default 9000 MB)
	oddStatus        int           // polls with an unknown status after the start
	bootdevs         []string      // vm/bootDev order
	bootNoop         bool          // bootDev change is ignored
	bootGetFail      bool          // GET vm/bootDev -> 500
	floppyStuck      bool          // floppy disconnect is ignored
	powerOffs        int
	refreshed        map[string]int
	cdPathRewrite    bool
	modifyStatusBoot int
	bootFirst        bool

	// N-R1 / N-R3 knobs
	firmware       interface{}  // boot.detail.bootFirmware (default 0; nil omits it)
	firmwareAfter  interface{}  // ... after the bootDev write, when set
	detailAllFail  bool         // GET vm/50/detail/all -> 500
	autoStart      interface{}  // vm/detail bootDev.autoStart (default 0; nil omits it)
	bootTouched    bool         // a bootDev write was applied
	bootSideEffect func(d dict) // edits vm/detail once bootTouched
	answerDelCode  int          // answer ISO delete -> this status
	answerDelNoop  bool         // answer ISO delete "succeeds" but stays listed
}

func newWinFake(t *testing.T) *winFake {
	return &winFake{t: t, polls: map[int64]int{}, fail: map[int64]bool{}, installPolls: 3, castools: "running", installAlloc: 9000, refreshed: map[string]int{}, firmware: 0, autoStart: 0,
		vols: map[string][]dict{
			"isopool":     {{"name": winISO, "format": "raw", "size": 5000, "storagePoolName": "isopool"}},
			"defaultpool": {},
			"tplpool":     {},
		}}
}

func (f *winFake) writes() []string {
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

func (f *winFake) wrote(prefix string) bool {
	for _, w := range f.writes() {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

func (f *winFake) addVol(pool string, v dict) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.vols[pool] = append(f.vols[pool], v)
}

func (f *winFake) hasVol(pool, name string) bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	for _, v := range f.vols[pool] {
		if v["name"] == name {
			return true
		}
	}
	return false
}

func (f *winFake) setUsers(p, users string) {
	for pool, vs := range f.vols {
		for _, v := range vs {
			if fmt.Sprint(v["path"]) == p || path.Join(winPoolPath(pool), fmt.Sprint(v["name"])) == p {
				v["users"] = users
			}
		}
	}
}

func winPoolPath(pool string) string {
	switch pool {
	case "isopool":
		return "/vms/isos"
	case "tplpool":
		return "/vms/template"
	}
	return "/vms/images"
}

func (f *winFake) vm50() dict {
	for _, v := range f.vms {
		if v["id"] == 50 {
			return v
		}
	}
	return nil
}

func (f *winFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
	p := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	if len(body) > 0 {
		f.bodies = append(f.bodies, r.Method+" "+p+" "+string(body))
	}
	switch {
	case r.Method == "GET" && p == "host/id/1":
		reply(w, dict{"id": 1, "name": "cvk1", "ip": "192.0.2.10", "hostPoolId": 1, "clusterId": 1})
	case r.Method == "GET" && p == "storage/pool":
		reply(w, dict{"storagePool": []dict{
			{"name": "defaultpool", "type": "dir", "path": "/vms/images"},
			{"name": "isopool", "type": "dir", "path": "/vms/isos"},
		}})
	case r.Method == "GET" && p == "host/id/1/storage":
		reply(w, dict{"storagePool": []dict{{"name": "tplpool", "type": "netfs", "path": "/vms/template/"}}})
	case r.Method == "GET" && p == "host/id/1/vswitch":
		reply(w, dict{"vSwitch": []dict{{"id": 3, "name": "vswitch0", "mtu": 1500}}})
	case r.Method == "GET" && p == "profile/all":
		reply(w, dict{"portProfile": []dict{{"id": 7, "name": "Default"}}})
	case r.Method == "GET" && p == "vmTemplate/all":
		reply(w, dict{"domain": append([]dict{}, f.tpls...)})
	case r.Method == "GET" && p == "vm/vmList":
		if vm := f.vm50(); vm != nil && (vm["vmStatus"] == "running" || vm["vmStatus"] == "unknown") {
			f.running++
			vm["vmStatus"] = "running"
			if f.oddStatus > 0 {
				f.oddStatus--
				vm["vmStatus"] = "unknown"
			}
			if f.installPolls > 0 && f.running > f.installPolls {
				vm["vmStatus"] = "shutOff"
				for _, v := range f.vols["defaultpool"] {
					if v["name"] == winDisk {
						v["allocation"] = f.installAlloc
					}
				}
			}
		}
		reply(w, dict{"domain": append([]dict{}, f.vms...)})
	case r.Method == "GET" && p == "vm/detail/50":
		d := dict{"id": 50, "name": winTempVM, "uuid": "uuid-win", "status": "shutOff", "uptime": f.running, "title": "",
			"cpu": dict{"cpuSockets": 1, "cpuCores": 2}, "memory": dict{"size": 4096, "unit": "MB"},
			"network":  []dict{{"mac": "0c:da:41:1d:00:01", "vsName": "vswitch0", "deviceModel": "virtio"}},
			"graphics": []dict{{"type": "vnc", "port": 5908, "address": "0.0.0.0", "password": "vnc-secret-xyz"}}, // array (A:61493)
			"bootDev":  dict{},
			"storage":  append(append([]dict{}, f.storage...), f.extraStorage...)}
		if f.autoStart != nil {
			d["bootDev"] = dict{"autoStart": f.autoStart}
		}
		if f.bootTouched && f.bootSideEffect != nil {
			f.bootSideEffect(d)
		}
		reply(w, d)
	case r.Method == "GET" && p == "vm/50/detail/all":
		if f.detailAllFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		bd := dict{"autoRun": false, "bootList": []dict{{"bootdev": "vda", "order": 1}}}
		fw := f.firmware
		if f.bootTouched && f.firmwareAfter != nil {
			fw = f.firmwareAfter
		}
		if fw != nil {
			bd["bootFirmware"] = fw
		}
		reply(w, dict{"id": 50, "domainName": winTempVM, "boot": dict{"dispName": "boot", "detail": bd}})
	case r.Method == "GET" && p == "vm/bootDev/50":
		if f.bootGetFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		items := []string{}
		for _, d := range f.bootdevs {
			items = append(items, d+";label "+d)
		}
		reply(w, dict{"id": 50, "name": winTempVM, "bootdev": dict{"bootdevs": strings.Join(items, ",")}})
	case r.Method == "GET" && p == "vm/50/castoolStatus":
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, f.castools)
	case r.Method == "GET" && p == "vmTemplate/storage":
		reply(w, dict{"data": []dict{{"id": 1, "storeFile": "/vms/template/win2019.qcow2", "device": "vda", "diskDevice": "disk", "driveType": "qcow2"}}})
	case r.Method == "GET" && p == "storage/volume":
		pool := q.Get("poolName")
		if pool == "tplpool" {
			reply(w, dict{"storageVolume": []dict{{"name": "win2019.qcow2", "format": "qcow2", "size": 61440, "backingStore": f.tplBacking}}})
			return
		}
		out := []dict{}
		for _, v := range f.vols[pool] {
			if v["name"] == winDisk && f.diskHidden {
				continue
			}
			if n := q.Get("volumeName"); len(n) == 0 || v["name"] == n {
				out = append(out, v)
			}
		}
		reply(w, dict{"storageVolume": out})
	case r.Method == "GET" && p == "storage/refresh":
		f.refreshed[q.Get("poolName")]++
		if q.Get("poolName") == "defaultpool" {
			f.diskHidden = false
		}
		reply(w, dict{"state": 0, "errorCode": 0, "message": "ok"})
	case r.Method == "POST" && p == "vm/add" && f.vmAddStatus != 0:
		if f.vmAddCreates {
			f.vms = append(f.vms, dict{"id": 50, "uuid": "uuid-win", "name": winTempVM, "vmStatus": "shutOff", "hostId": 1})
		}
		w.Header().Set("Error-Code", "999")
		w.WriteHeader(f.vmAddStatus)
	case r.Method == "POST" && p == "vm/add":
		b, _ := jsonutils.Parse(body)
		name, _ := b.GetString("name")
		f.vms = append(f.vms, dict{"id": 50, "uuid": "uuid-win", "name": name, "vmStatus": "shutOff", "hostId": 1})
		arr, _ := b.GetArray("storage")
		hd := 'a'
		for _, it := range arr {
			dev, _ := it.GetString("device")
			sf, _ := it.GetString("storeFile")
			switch dev {
			case "disk":
				pool, _ := it.GetString("poolName")
				dp := path.Join(winPoolPath(pool), sf)
				capacity, _ := it.Int("capacity")
				st := dict{"device": "disk", "deviceName": "vda", "path": dp, "format": "qcow2", "backingFile": "", "isShareStorage": false}
				if f.diskDetail != nil {
					f.diskDetail(st)
				}
				f.storage = append(f.storage, st)
				v := dict{"name": sf, "format": "qcow2", "size": capacity, "allocation": 0.19, "level": 0, "backingStore": "", "users": name}
				if f.diskVol != nil {
					f.diskVol(v)
				}
				f.vols[pool] = append(f.vols[pool], v)
				f.bootdevs = []string{"hda", "vda", "fda"}
				if f.bootFirst {
					f.bootdevs = []string{"vda", "hda", "fda"}
				}
				if f.extraDisk {
					f.storage = append(f.storage, dict{"device": "disk", "deviceName": "vdb", "path": "/vms/images/precious.qcow2"})
				}
			case "cdrom":
				cp := sf
				if f.cdPathRewrite && sf == DEFAULT_CASTOOLS_ISO {
					cp = "/vms/castools/castools.iso"
				}
				f.storage = append(f.storage, dict{"device": "cdrom", "deviceName": "hd" + string(hd), "path": cp})
				hd++
				f.setUsers(sf, name)
			case "floppy":
				f.storage = append(f.storage, dict{"device": "floppy", "deviceName": "fda", "path": "/vms/isos/virtio-win.vfd"})
			}
		}
		f.task(w, 8002)
	case r.Method == "PUT" && p == "vm/start/50":
		if f.startStatus != 0 {
			w.Header().Set("Error-Code", "999")
			w.WriteHeader(f.startStatus)
			return
		}
		if !f.startNoop {
			f.vm50()["vmStatus"] = "running"
		}
		if f.onStart != nil {
			f.onStart()
		}
		f.task(w, 8006)
	case r.Method == "PUT" && p == "vm/modify":
		if f.modifyStatus != 0 {
			w.Header().Set("Error-Code", "999")
			w.WriteHeader(f.modifyStatus)
			return
		}
		b, _ := jsonutils.Parse(body)
		if bd, err := b.GetString("bootDev", "bootdevs"); err == nil {
			if f.modifyStatusBoot != 0 {
				w.Header().Set("Error-Code", "999")
				w.WriteHeader(f.modifyStatusBoot)
				return
			}
			if !f.bootNoop {
				f.bootdevs = strings.Split(bd, ",")
			}
			f.bootTouched = true
			f.task(w, 8008)
			return
		}
		dev, _ := b.GetString("cdrom", "device")
		fl, _ := b.GetString("floppy", "deviceName")
		if len(fl) > 0 {
			dev = fl
		}
		if !f.modifyNoop && !(len(fl) > 0 && f.floppyStuck) {
			kept := []dict{}
			for _, st := range f.storage {
				if st["deviceName"] == dev {
					f.setUsers(fmt.Sprint(st["path"]), "")
					continue
				}
				kept = append(kept, st)
			}
			f.storage = kept
		}
		f.task(w, 8007)
	case r.Method == "PUT" && p == "vm/powerOff/50":
		f.powerOffs++
		f.vm50()["vmStatus"] = "shutOff"
		f.task(w, 8005)
	case r.Method == "POST" && p == "vmTemplate/addVmTemplate":
		if f.tplFail {
			f.fail[8003] = true
		} else {
			f.tpls = append(f.tpls, dict{"id": 90, "name": q.Get("domainName"), "templetStoragePath": "/vms/template"})
		}
		f.task(w, 8003)
	case r.Method == "DELETE" && strings.HasPrefix(p, "vm/delete/uuid/"):
		if f.deleteVMFail {
			w.Header().Set("Error-Code", "999")
			w.WriteHeader(http.StatusConflict)
			return
		}
		for _, vm := range f.vms {
			if vm["uuid"] == strings.TrimPrefix(p, "vm/delete/uuid/") {
				vm["delTime"] = "2026-10-20T12:00:00+08:00"
			}
		}
		f.task(w, 8004)
	case r.Method == "DELETE" && strings.HasPrefix(p, "storage/volume/delete/1/isopool/"):
		name := strings.TrimPrefix(p, "storage/volume/delete/1/isopool/")
		if f.answerDelCode != 0 {
			w.Header().Set("Error-Code", "999")
			w.WriteHeader(f.answerDelCode)
			return
		}
		if f.answerDelNoop {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		kept := []dict{}
		for _, v := range f.vols["isopool"] {
			if v["name"] != name {
				kept = append(kept, v)
			}
		}
		f.vols["isopool"] = kept
		f.deleteAnswers++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "GET" && strings.HasPrefix(p, "message/"):
		var id int64
		fmt.Sscanf(strings.TrimPrefix(p, "message/"), "%d", &id)
		f.polls[id]++
		result := 0
		if f.fail[id] {
			result = 1
		}
		reply(w, dict{"msgId": id, "completed": f.polls[id] >= 2, "result": result, "failMsg": "injected"})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *winFake) task(w http.ResponseWriter, id int64) {
	reply(w, dict{"msgId": id, "completed": false})
}

func (f *winFake) client(t *testing.T) *SCasClient {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 2 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	return cli
}

// winStager plays the upload: Stage adds the file to the fake pool and
// keeps what it read.
type winStager struct {
	f       *winFake
	local   string
	data    []byte
	mode    os.FileMode
	fail    error
	planned int
	staged  int
}

func (s *winStager) Plan(t SImportTarget) (*SImportStagePlan, error) {
	s.planned++
	return &SImportStagePlan{Step: "upload", Method: "FAKE", Detail: "fake upload of " + path.Base(s.local)}, nil
}

func (s *winStager) Stage(t SImportTarget) ([]string, error) {
	s.staged++
	if s.local != "" {
		st, err := os.Stat(s.local)
		if err != nil {
			return nil, err
		}
		s.mode = st.Mode().Perm()
		s.data, _ = os.ReadFile(s.local)
	}
	if s.fail != nil {
		return nil, s.fail
	}
	s.f.addVol(t.PoolName, dict{"name": t.FileName, "format": "raw", "size": 1})
	return nil, nil
}

type winRun struct {
	f      *winFake
	answer *winStager
	opts   SWindowsImportOptions
}

func newWinRun(t *testing.T) *winRun {
	f := newWinFake(t)
	r := &winRun{f: f, answer: &winStager{f: f}}
	r.opts = SWindowsImportOptions{
		HostId: 1, IsoPool: "isopool", IsoFile: winISO, DiskPool: "defaultpool", TemplateName: winTpl, VSwitchName: "vswitch0",
		OsVersion: "Microsoft Windows Server 2019(64-bit)",
		Unattend:  SUnattendOptions{ImageIndex: 2, AdminPassword: SecretString(testWinPassword), ProductKey: SecretString(testWinKey)},
		AnswerStager: func(local string) (ImportSourceStager, error) {
			r.answer.local = local
			return r.answer, nil
		},
		AnswerMode:   "fake",
		PollInterval: 2 * time.Millisecond, InstallTimeout: 2 * time.Second,
	}
	return r
}

func (r *winRun) run(t *testing.T) (*SWindowsImportResult, error) {
	res, err := r.f.client(t).ImportWindowsISOAsTemplate(r.opts)
	assertNoWinSecret(t, r.f, res, err)
	return res, err
}

// assertNoWinSecret: neither the password, its encodings nor the
// product key reach the result, the error or any request.
func assertNoWinSecret(t *testing.T, f *winFake, res *SWindowsImportResult, err error) {
	t.Helper()
	secrets := []string{testWinPassword, testWinKey, encodeUnattendPassword(testWinPassword, "AdministratorPassword"), encodeUnattendPassword(testWinPassword, "Password")}
	out := []string{}
	if res != nil {
		js, _ := json.Marshal(res)
		out = append(out, string(js), fmt.Sprintf("%+v", *res))
	}
	if err != nil {
		out = append(out, err.Error(), fmt.Sprintf("%+v", err))
	}
	f.lock.Lock()
	out = append(out, f.hits...)
	out = append(out, f.bodies...)
	f.lock.Unlock()
	for _, o := range out {
		for _, s := range secrets {
			if strings.Contains(o, s) {
				t.Fatalf("secret %q leaked into %s", s, o)
			}
		}
	}
}

func (r *winRun) addBody(t *testing.T) jsonutils.JSONObject {
	t.Helper()
	for _, b := range r.f.bodies {
		if strings.HasPrefix(b, "POST vm/add ") {
			j, err := jsonutils.ParseString(strings.TrimPrefix(b, "POST vm/add "))
			if err != nil {
				t.Fatal(err)
			}
			return j
		}
	}
	t.Fatal("no vm/add body")
	return nil
}

func TestWindowsImportHappyPath(t *testing.T) {
	r := newWinRun(t)
	r.opts.Castools = true // N-R5: waits for CAStools by default
	r.opts.Unattend.CastoolsArgs = "/S"
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if res.Template == nil || res.Template.Id != 90 || res.TempVMUuid != "uuid-win" {
		t.Fatalf("result %+v", res)
	}
	want := []string{
		"GET /cas/casrs/storage/refresh?id=1&poolName=defaultpool", // B-W1, before the name check
		"GET /cas/casrs/storage/refresh?id=1&poolName=isopool",
		"GET /cas/casrs/storage/refresh?id=1&poolName=isopool",
		"POST /cas/casrs/vm/add",
		"PUT /cas/casrs/vm/start/50",
		"PUT /cas/casrs/vm/modify", // install ISO
		"PUT /cas/casrs/vm/modify", // answer ISO
		"PUT /cas/casrs/vm/modify", // castools
		"PUT /cas/casrs/vm/modify", // floppy
		"PUT /cas/casrs/vm/modify", // boot order
		"POST /cas/casrs/vmTemplate/addVmTemplate?",
		"DELETE /cas/casrs/vm/delete/uuid/uuid-win?delBase0=false&isWipeVolume=false&type=3",
		"DELETE /cas/casrs/storage/volume/delete/1/isopool/" + winAnswer,
		"GET /cas/casrs/storage/refresh?id=1&poolName=isopool", // N-R3 confirm
	}
	w := r.f.writes()
	if len(w) != len(want) {
		t.Fatalf("writes %d:\n%s", len(w), strings.Join(w, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(w[i], want[i]) {
			t.Errorf("write %d = %s, want %s", i, w[i], want[i])
		}
	}
	if !strings.Contains(w[10], "opType=1") || strings.Contains(w[10], "opType=2") || strings.Contains(w[10], "disConnect") {
		t.Errorf("addVmTemplate %s", w[10])
	}
	add := r.addBody(t)
	for k, v := range map[string]interface{}{"system": int64(0), "bootingDevice": int64(2), "autoBooting": int64(0), "memory": int64(4096), "cpu": int64(2)} {
		if got, _ := add.Int(k); got != v.(int64) {
			t.Errorf("vm/add %s = %d, want %d", k, got, v)
		}
	}
	if s, _ := add.GetString("osVersion"); s != "Microsoft Windows Server 2019(64-bit)" {
		t.Errorf("osVersion %s", s)
	}
	if s, _ := add.GetString("osBit"); s != "x86_64" {
		t.Errorf("osBit %s", s)
	}
	if b, _ := add.Bool("autoLoadVirtio"); b {
		t.Error("autoLoadVirtio in floppy mode")
	}
	st, _ := add.GetArray("storage")
	if len(st) != 5 {
		t.Fatalf("storage items %d", len(st))
	}
	get := func(i int, k string) string { s, _ := st[i].GetString(k); return s }
	if get(0, "device") != "disk" || get(0, "storeFile") != winDisk || get(0, "poolName") != "defaultpool" || get(0, "targetBus") != "virtio" || get(0, "driveType") != "qcow2" {
		t.Errorf("disk item %s", st[0])
	}
	if c, _ := st[0].Int("capacity"); c != 60*1024 {
		t.Errorf("capacity %d", c)
	}
	if m, _ := st[0].Int("mode"); m != 2 {
		t.Errorf("mode %d", m)
	}
	for i, p := range []string{"/vms/isos/" + winISO, "/vms/isos/" + winAnswer, DEFAULT_CASTOOLS_ISO} {
		if get(i+1, "device") != "cdrom" || get(i+1, "storeFile") != p || get(i+1, "targetBus") != "ide" {
			t.Errorf("cdrom %d: %s", i, st[i+1])
		}
	}
	if get(4, "device") != "floppy" || get(4, "storeFile") != "autoVirtio" {
		t.Errorf("floppy %s", st[4])
	}
	nic, _ := add.GetArray("network")
	if len(nic) != 1 {
		t.Fatal("nic")
	}
	if m, _ := nic[0].GetString("deviceModel"); m != "virtio" {
		t.Errorf("nic model %s", m)
	}
	// detach bodies
	detach := []string{}
	for _, b := range r.f.bodies {
		if strings.HasPrefix(b, "PUT vm/modify ") {
			detach = append(detach, b)
		}
	}
	if len(detach) != 5 || !strings.Contains(detach[4], `{"bootDev":{"autoStart":0,"bootFirmware":0,"bootdevs":"vda,hda,fda"},"id":50}`) || !strings.Contains(detach[1], `"operation":"disconnect"`) || !strings.Contains(detach[1], winAnswer) || !strings.Contains(detach[1], `"device":"hdb"`) || !strings.Contains(detach[3], `"floppy":{"deviceName":"fda","operation":"disconnect"}`) {
		t.Errorf("detach bodies:\n%s", strings.Join(detach, "\n"))
	}
	// the answer ISO: 0600 temp file, removed, holds the real answer file
	if r.answer.mode != 0600 {
		t.Errorf("answer temp file mode %o", r.answer.mode)
	}
	if _, err := os.Stat(r.answer.local); !os.IsNotExist(err) {
		t.Errorf("answer temp file %s not removed", r.answer.local)
	}
	_, files := readISORoot(t, r.answer.data, 17, true)
	if len(files) != 1 || files[0].name != "autounattend.xml" || !strings.Contains(string(files[0].data), encodeUnattendPassword(testWinPassword, "AdministratorPassword")) || !strings.Contains(string(files[0].data), `A:\amd64\2k19`) {
		t.Fatalf("answer ISO content %v", names(files))
	}
	if r.f.hasVol("isopool", winAnswer) {
		t.Error("answer ISO not deleted")
	}
	if !r.f.wrote("GET /cas/casrs/storage/refresh") || r.f.polls[8006] == 0 {
		t.Error("no refresh / start task")
	}
	castools := false
	for _, h := range r.f.hits {
		castools = castools || strings.Contains(h, "vm/50/castoolStatus")
	}
	if !castools {
		t.Error("castoolStatus not polled")
	}
	for _, wn := range res.Warnings {
		if strings.Contains(wn, "never reported running") || strings.Contains(wn, "SECRET LEFT") {
			t.Errorf("unexpected warning %s", wn)
		}
	}
	if !strings.Contains(res.AnswerXML, "CAS_tools_setup.exe /S") || strings.Contains(res.AnswerXML, encodeUnattendPassword(testWinPassword, "AdministratorPassword")) {
		t.Error("redacted answer XML")
	}
}

func TestWindowsImportDryRunZeroWrites(t *testing.T) {
	r := newWinRun(t)
	r.opts.DryRun = true
	r.opts.BootOrder = WIN_BOOT_DISK_FIRST
	r.opts.VirtioFloppy = WIN_VIRTIO_AUTO
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if w := r.f.writes(); len(w) != 0 {
		t.Fatalf("dry-run wrote: %v", w)
	}
	if r.answer.local != "" || r.answer.planned != 0 || r.answer.staged != 0 {
		t.Fatal("dry-run touched the answer stager (temp file)")
	}
	steps := []string{}
	for _, s := range res.Steps {
		steps = append(steps, s.Step)
		if s.Done {
			t.Errorf("step %s done in dry-run", s.Step)
		}
	}
	if got := strings.Join(steps, ","); got != "refresh-disk-pool,refresh-pool-before-answer-iso,upload-answer-iso,create-vm,check-vm-disks,start-vm,recheck-vm-disks,wait-install,check-install,detach-media,boot-disk-first,clone-to-template,check-template-flat,delete-temp-vm,delete-answer-iso" {
		t.Errorf("steps %s", got)
	}
	add := res.Steps[3].Detail
	if !strings.Contains(add, `"bootingDevice":1`) || !strings.Contains(add, `"autoLoadVirtio":true`) || strings.Contains(add, "floppy") {
		t.Errorf("vm/add plan %s", add)
	}
	if !strings.Contains(res.AnswerXML, redactedSecret) && !strings.Contains(res.AnswerXML, "&lt;redacted&gt;") {
		t.Error("no redacted XML in the plan")
	}
	for _, want := range []string{`A:\amd64\2k19`, "/shutdown", "<WillWipeDisk>true</WillWipeDisk>"} {
		if !strings.Contains(res.AnswerXML, want) {
			t.Errorf("plan XML lacks %s", want)
		}
	}
}

func TestWindowsImportDryRunStagesNothing(t *testing.T) {
	r := newWinRun(t)
	r.opts.DryRun = true
	inst := &winStager{f: r.f, local: "/nonexistent/win.iso"}
	r.opts.IsoFile = "new.iso"
	r.opts.IsoStager = inst
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
	if inst.planned != 1 || inst.staged != 0 || len(r.f.writes()) != 0 {
		t.Fatalf("planned %d staged %d writes %v", inst.planned, inst.staged, r.f.writes())
	}
}

func TestWindowsImportReadOnlyZeroRequests(t *testing.T) {
	r := newWinRun(t)
	cli := r.f.client(t)
	cli.cpcfg.ReadOnly = true
	_, err := cli.ImportWindowsISOAsTemplate(r.opts)
	if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Fatalf("err %v", err)
	}
	if len(r.f.hits) != 0 || r.answer.local != "" {
		t.Fatalf("read-only made requests %v", r.f.hits)
	}
}

func TestWindowsImportUploadsInstallISO(t *testing.T) {
	r := newWinRun(t)
	inst := &winStager{f: r.f}
	r.opts.IsoFile = "new.iso"
	r.opts.IsoStager = inst
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if inst.staged != 1 || !r.f.hasVol("isopool", "new.iso") {
		t.Fatal("install ISO not staged")
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "kept for reuse")
	}
	if !found {
		t.Error("no keep warning for the uploaded install ISO")
	}
}

func TestWindowsImportPreplacedAnswer(t *testing.T) {
	r := newWinRun(t)
	r.opts.AnswerStager = nil
	r.opts.AnswerIsoFile = "my-answer.iso"
	r.f.vols["isopool"] = append(r.f.vols["isopool"], dict{"name": "my-answer.iso", "format": "raw", "size": 1})
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if r.f.deleteAnswers != 0 || !r.f.hasVol("isopool", "my-answer.iso") {
		t.Fatal("a hand-placed answer ISO must not be deleted")
	}
	if res.AnswerISO != "/vms/isos/my-answer.iso" {
		t.Fatal(res.AnswerISO)
	}
}

func TestWindowsImportRefusals(t *testing.T) {
	for name, c := range map[string]func(r *winRun){
		"not an iso":             func(r *winRun) { r.opts.IsoFile = "win.qcow2" },
		"qcow2 volume":           func(r *winRun) { r.f.vols["isopool"][0]["format"] = "qcow2" },
		"missing iso":            func(r *winRun) { r.opts.IsoFile = "nope.iso" },
		"template exists":        func(r *winRun) { r.f.tpls = []dict{{"id": 1, "name": winTpl}} },
		"vm exists":              func(r *winRun) { r.f.vms = []dict{{"id": 9, "uuid": "u9", "name": winTempVM, "vmStatus": "shutOff"}} },
		"disk exists":            func(r *winRun) { r.f.vols["defaultpool"] = []dict{{"name": winDisk, "format": "qcow2"}} },
		"answer exists":          func(r *winRun) { r.f.vols["isopool"] = append(r.f.vols["isopool"], dict{"name": winAnswer}) },
		"no answer transport":    func(r *winRun) { r.opts.AnswerStager = nil },
		"both answer sources":    func(r *winRun) { r.opts.AnswerIsoFile = "a.iso" },
		"linux os":               func(r *winRun) { r.opts.OsVersion = "CentOS 7" },
		"small memory":           func(r *winRun) { r.opts.MemoryMB = 1024 },
		"bad boot order":         func(r *winRun) { r.opts.BootOrder = "net" },
		"castools no cd":         func(r *winRun) { r.opts.Unattend.CastoolsInstaller = "x.exe" },
		"answer is install":      func(r *winRun) { r.opts.AnswerStager = nil; r.opts.AnswerIsoFile = winISO },
		"no-wait castools no cd": func(r *winRun) { r.opts.NoWaitCastools = true },
		"min installed >= disk":  func(r *winRun) { r.opts.MinInstalledGB = 60 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			c(r)
			if _, err := r.run(t); err == nil {
				t.Fatal("want error")
			}
			for _, w := range r.f.writes() {
				if !strings.Contains(w, "storage/refresh") {
					t.Fatalf("writes on refusal: %v", r.f.writes())
				}
			}
			if r.f.wrote("POST /cas/casrs/vm/add") {
				t.Fatal("vm/add sent")
			}
		})
	}
}

func assertKept(t *testing.T, r *winRun, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "temporary VM "+winTempVM+" (uuid-win) was kept") {
		t.Fatalf("want a kept-VM error, got %v", err)
	}
	if r.f.wrote("DELETE /cas/casrs/vm/delete") {
		t.Fatal("the VM was deleted")
	}
	if r.f.deleteAnswers != 0 {
		t.Fatal("the answer ISO was deleted while the VM uses it")
	}
}

func hasWarning(res *SWindowsImportResult, s string) bool {
	if res == nil {
		return false
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, s) {
			return true
		}
	}
	return false
}

func TestWindowsImportInstallTimeoutKeepsVM(t *testing.T) {
	r := newWinRun(t)
	r.f.installPolls = 0 // never shuts off
	r.opts.InstallTimeout = 30 * time.Millisecond
	res, err := r.run(t)
	assertKept(t, r, err)
	if errors.Cause(err) != cloudprovider.ErrTimeout && !strings.Contains(err.Error(), "did not shut off within") {
		t.Fatalf("err %v", err)
	}
	if !hasWarning(res, "SECRET LEFT ON THE CAS NODE: answer ISO") {
		t.Error("no answer ISO warning")
	}
	if r.f.wrote("POST /cas/casrs/vmTemplate") || r.f.wrote("PUT /cas/casrs/vm/modify") {
		t.Fatal("went on after a timeout")
	}
}

func TestWindowsImportNeverRunningKeepsVM(t *testing.T) {
	r := newWinRun(t)
	r.f.startNoop = true
	_, err := r.run(t)
	assertKept(t, r, err)
	if !strings.Contains(err.Error(), "never seen running") {
		t.Fatal(err)
	}
}

func TestWindowsImportInterruptedWhileInstalling(t *testing.T) {
	r := newWinRun(t)
	r.f.installPolls = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.f.onStart = cancel
	r.opts.Context = ctx
	_, err := r.run(t)
	assertKept(t, r, err)
	if errors.Cause(err) != context.Canceled && !strings.Contains(err.Error(), "interrupted") {
		t.Fatal(err)
	}
}

func TestWindowsImportInterruptedBeforeWrites(t *testing.T) {
	r := newWinRun(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.opts.Context = ctx
	_, err := r.run(t)
	if err == nil || len(r.f.writes()) != 0 {
		t.Fatalf("err %v writes %v", err, r.f.writes())
	}
}

func TestWindowsImportCastoolsNeverSeenKeepsVM(t *testing.T) {
	r := newWinRun(t)
	r.opts.Castools = true // the default: wait for CAStools
	r.f.castools = "not running"
	_, err := r.run(t)
	assertKept(t, r, err)
	if !strings.Contains(err.Error(), "CAStools was never reported running") || r.f.wrote("PUT /cas/casrs/vm/modify") {
		t.Fatal(err)
	}
}

func TestWindowsImportFailureCleanup(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(r *winRun)
		check func(t *testing.T, r *winRun, res *SWindowsImportResult, err error)
	}{
		"answer upload fails": {
			func(r *winRun) { r.answer.fail = errors.Error("injected upload failure") },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if r.f.wrote("POST /cas/casrs/vm/add") || !hasWarning(res, "SECRET LEFT ON THE CAS NODE: answer ISO") {
					t.Fatal("went on, or no warning about a partial answer ISO")
				}
				if _, serr := os.Stat(r.answer.local); !os.IsNotExist(serr) {
					t.Fatal("temp file left")
				}
			},
		},
		"vm/add rejected": {
			func(r *winRun) { r.f.vmAddStatus = http.StatusBadRequest },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if r.f.wrote("DELETE /cas/casrs/vm/") || r.f.deleteAnswers != 1 {
					t.Fatalf("want answer ISO deleted, no VM delete: %v", r.f.writes())
				}
			},
		},
		"vm/add rejected but VM exists": {
			func(r *winRun) { r.f.vmAddStatus, r.f.vmAddCreates = http.StatusBadRequest, true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if r.f.wrote("DELETE") || !strings.Contains(err.Error(), "was kept") {
					t.Fatalf("want everything kept: %v %v", err, r.f.writes())
				}
			},
		},
		"vm/add ambiguous, VM created": {
			func(r *winRun) { r.f.vmAddStatus, r.f.vmAddCreates = http.StatusInternalServerError, true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if !r.f.wrote("DELETE /cas/casrs/vm/delete/uuid/uuid-win") || r.f.wrote("PUT /cas/casrs/vm/start") || r.f.deleteAnswers != 0 {
					t.Fatalf("want the never-started VM recycled, answer kept: %v", r.f.writes())
				}
			},
		},
		"vm/add ambiguous, no VM": {
			func(r *winRun) { r.f.vmAddStatus = http.StatusInternalServerError },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if r.f.wrote("DELETE") || !hasWarning(res, "SECRET LEFT ON THE CAS NODE: answer ISO") {
					t.Fatalf("want answer ISO kept with a warning: %v", r.f.writes())
				}
			},
		},
		"second disk": {
			func(r *winRun) { r.f.extraDisk = true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				assertKept(t, r, err)
				if r.f.wrote("PUT /cas/casrs/vm/start") || !strings.Contains(err.Error(), "wipes disk 0") {
					t.Fatalf("want refuse-to-start, VM kept: %v %v", err, r.f.writes())
				}
			},
		},
		"start rejected": {
			func(r *winRun) { r.f.startStatus = http.StatusBadRequest },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if !r.f.wrote("DELETE /cas/casrs/vm/delete/uuid/uuid-win") {
					t.Fatal("want recycle after a definite start rejection")
				}
			},
		},
		"start ambiguous": {
			func(r *winRun) { r.f.startStatus = http.StatusInternalServerError },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) { assertKept(t, r, err) },
		},
		"detach fails": {
			func(r *winRun) { r.f.modifyStatus = http.StatusInternalServerError },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				assertKept(t, r, err)
				if r.f.wrote("POST /cas/casrs/vmTemplate") {
					t.Fatal("cloned with media attached")
				}
			},
		},
		"detach no-op": {
			func(r *winRun) { r.f.modifyNoop = true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				assertKept(t, r, err)
				if !strings.Contains(err.Error(), "still has") || r.f.wrote("POST /cas/casrs/vmTemplate") {
					t.Fatal(err)
				}
			},
		},
		"template fails": {
			func(r *winRun) { r.f.tplFail = true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) { assertKept(t, r, err) },
		},
		"template has backing file": {
			func(r *winRun) { r.f.tplBacking = "/vms/images/" + winDisk },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				assertKept(t, r, err)
				if errors.Cause(err) != ErrTemplateHasBackingFile && !strings.Contains(err.Error(), "backing file") {
					t.Fatal(err)
				}
			},
		},
		"temp VM delete fails": {
			func(r *winRun) { r.f.deleteVMFail = true },
			func(t *testing.T, r *winRun, res *SWindowsImportResult, err error) {
				if res.Template == nil || r.f.deleteAnswers != 0 || !hasWarning(res, "SECRET LEFT ON THE CAS NODE: answer ISO") {
					t.Fatalf("want template + answer kept with warning: %v", err)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			c.setup(r)
			res, err := r.run(t)
			if err == nil {
				t.Fatal("want error")
			}
			for _, w := range r.f.writes() {
				if strings.Contains(w, "opType=2") || strings.Contains(w, "isWipeVolume=true") || strings.Contains(w, "delBase0=true") {
					t.Fatalf("forbidden write %s", w)
				}
			}
			c.check(t, r, res, err)
		})
	}
}

func TestCastoolStatusBodies(t *testing.T) {
	for body, want := range map[string]string{"running": "running", `"not running"`: "not running", " running\n": "running"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
		got, err := cli.GetCastoolStatus(5)
		srv.Close()
		if err != nil || got != want {
			t.Errorf("%q: got %q %v", body, got, err)
		}
	}
}

// B-W1: every doubt about disk 0 keeps the VM unstarted.
func TestWindowsImportDiskGuardFailsClosed(t *testing.T) {
	for name, setup := range map[string]func(f *winFake){
		"extra lun device": func(f *winFake) { f.extraStorage = []dict{{"device": "lun", "deviceName": "sdb", "path": "/dev/sdb"}} },
		"extra empty device": func(f *winFake) {
			f.extraStorage = []dict{{"device": "", "deviceName": "vdb", "path": "/vms/images/x.qcow2"}}
		},
		"extra Disk (case)": func(f *winFake) {
			f.extraStorage = []dict{{"device": "Disk", "deviceName": "vdb", "path": "/vms/images/x.qcow2"}}
		},
		"prefix-only path": func(f *winFake) {
			f.diskDetail = func(st dict) { st["path"] = "/vms/images/tplwin-win2019-precious.qcow2" }
		},
		"other directory":      func(f *winFake) { f.diskDetail = func(st dict) { st["path"] = "/vms/other/" + winDisk } },
		"detail backing file":  func(f *winFake) { f.diskDetail = func(st dict) { st["backingFile"] = "/vms/images/base.qcow2" } },
		"shared storage":       func(f *winFake) { f.diskDetail = func(st dict) { st["isShareStorage"] = true } },
		"no device name":       func(f *winFake) { f.diskDetail = func(st dict) { st["deviceName"] = "" } },
		"volume backing store": func(f *winFake) { f.diskVol = func(v dict) { v["backingStore"] = "/vms/images/base.qcow2" } },
		"no backingStore key":  func(f *winFake) { f.diskVol = func(v dict) { delete(v, "backingStore") } },
		"numeric backingStore": func(f *winFake) { f.diskVol = func(v dict) { v["backingStore"] = 0 } },
		"wrong size":           func(f *winFake) { f.diskVol = func(v dict) { v["size"] = 40960 } },
		"already allocated":    func(f *winFake) { f.diskVol = func(v dict) { v["allocation"] = 20480 } },
		"raw format":           func(f *winFake) { f.diskVol = func(v dict) { v["format"] = "raw" } },
		"base file level":      func(f *winFake) { f.diskVol = func(v dict) { v["level"] = 1 } },
		"foreign user":         func(f *winFake) { f.diskVol = func(v dict) { v["users"] = "prod-db" } },
		"renamed volume":       func(f *winFake) { f.diskVol = func(v dict) { v["name"] = "other.qcow2" } },
		"no allocation key":    func(f *winFake) { f.diskVol = func(v dict) { delete(v, "allocation") } }, // N-R4
		"null allocation":      func(f *winFake) { f.diskVol = func(v dict) { v["allocation"] = nil } },
		"text allocation":      func(f *winFake) { f.diskVol = func(v dict) { v["allocation"] = "n/a" } },
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			setup(r.f)
			_, err := r.run(t)
			assertKept(t, r, err)
			if !strings.Contains(err.Error(), "refused to start it") || r.f.wrote("PUT /cas/casrs/vm/start") {
				t.Fatalf("want refuse-to-start: %v %v", err, r.f.writes())
			}
		})
	}
}

func TestWindowsImportRefreshesDiskPoolBeforeNameCheck(t *testing.T) {
	r := newWinRun(t)
	r.f.vols["defaultpool"] = []dict{{"name": winDisk, "format": "qcow2", "size": 61440}}
	r.f.diskHidden = true // copied in by hand, not yet listed
	_, err := r.run(t)
	if errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Fatalf("err %v", err)
	}
	if w := r.f.writes(); len(w) != 1 || !strings.Contains(w[0], "poolName=defaultpool") {
		t.Fatalf("writes %v", w)
	}
}

// N-W1: disks changed between the guard and the start: power off, keep.
func TestWindowsImportDisksChangedAfterStart(t *testing.T) {
	r := newWinRun(t)
	r.f.onStart = func() {
		r.f.extraStorage = []dict{{"device": "disk", "deviceName": "vdb", "path": "/vms/images/precious.qcow2"}}
	}
	_, err := r.run(t)
	assertKept(t, r, err)
	if r.f.powerOffs != 1 || !strings.Contains(err.Error(), "powered off") {
		t.Fatalf("powerOffs %d: %v", r.f.powerOffs, err)
	}
}

// N-W3: a shut-off guest with a nearly empty disk did not install.
func TestWindowsImportShutOffWithoutInstallKeepsVM(t *testing.T) {
	r := newWinRun(t)
	r.f.installAlloc = 300
	_, err := r.run(t)
	assertKept(t, r, err)
	if !strings.Contains(err.Error(), "probably not installed") || r.f.wrote("PUT /cas/casrs/vm/modify") || r.f.wrote("POST /cas/casrs/vmTemplate") {
		t.Fatal(err)
	}
}

func TestWindowsImportTransientStatusTolerated(t *testing.T) {
	r := newWinRun(t)
	r.f.oddStatus = 2
	r.f.installPolls = 6
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
	r = newWinRun(t)
	r.f.oddStatus = 100
	r.f.installPolls = 200
	_, err := r.run(t)
	assertKept(t, r, err)
}

// B-W2: media are always detached and verified.
func TestWindowsImportMediaDetachVerified(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(r *winRun)
		want  string
	}{
		"floppy stays": {func(r *winRun) { r.f.floppyStuck = true }, "still has /vms/isos/virtio-win.vfd"},
		"CD not listed as ours": {func(r *winRun) {
			r.opts.Castools = true
			r.opts.CastoolsISO = "/vms/isos/castools.iso"
			r.f.diskDetail = nil
			r.f.extraStorage = nil
			r.f.cdPathRewrite = true
		}, "does not list"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			c.setup(r)
			_, err := r.run(t)
			assertKept(t, r, err)
			if !strings.Contains(err.Error(), c.want) || r.f.wrote("POST /cas/casrs/vmTemplate") {
				t.Fatal(err)
			}
		})
	}
}

// N-W4: boot order disk-first before cloning, verified.
func TestWindowsImportBootOrder(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *winFake)
		ok    bool
	}{
		"ignored":     {func(f *winFake) { f.bootNoop = true }, false},
		"unreadable":  {func(f *winFake) { f.bootGetFail = true }, false},
		"modify fail": {func(f *winFake) { f.bootNoop = true; f.modifyStatusBoot = http.StatusInternalServerError }, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			c.setup(r.f)
			_, err := r.run(t)
			assertKept(t, r, err)
			if !strings.Contains(err.Error(), "boot order") || r.f.wrote("POST /cas/casrs/vmTemplate") {
				t.Fatal(err)
			}
		})
	}
	// already disk-first: no change sent, still verified
	r := newWinRun(t)
	r.f.bootFirst = true
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range r.f.bodies {
		if strings.Contains(b, "bootDev") {
			t.Fatal("bootDev sent although already disk-first")
		}
	}
	found := false
	for _, st := range res.Steps {
		found = found || st.Step == "verify-boot-order"
	}
	if !found {
		t.Fatal("boot order not verified")
	}
}

func TestGetVMBootDevsParse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// api.yaml A:21052 example
		reply(w, dict{"id": 5, "bootdev": dict{"bootdevs": "vda;高速磁盘(Virtio) vda,sda;光驱 sda,fda;FDC磁盘 fda,0c:da:41:1d:90:56;网络0c:da:41:1d:90:56"}})
	}))
	defer srv.Close()
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
	got, err := cli.GetVMBootDevs(5)
	if err != nil || strings.Join(got, ",") != "vda,sda,fda,0c:da:41:1d:90:56" {
		t.Fatalf("%v %v", got, err)
	}
}

// B-W2: there is no way to keep the media attached.
func TestWindowsImportNoKeepMediaOption(t *testing.T) {
	if _, ok := reflect.TypeOf(SWindowsImportOptions{}).FieldByName("KeepMedia"); ok {
		t.Fatal("KeepMedia must not exist")
	}
}

// N-R5: with --castools CAStools must be seen running unless opted out.
func TestWindowsImportCastoolsWaitDefault(t *testing.T) {
	r := newWinRun(t)
	r.opts.Castools, r.opts.NoWaitCastools = true, true
	r.f.castools = "not running"
	res, err := r.run(t)
	if err != nil || res.Template == nil {
		t.Fatalf("opt-out still waits: %v", err)
	}
	for _, h := range r.f.hits {
		if strings.Contains(h, "castoolStatus") {
			t.Fatal("castoolStatus polled although opted out")
		}
	}
	o := SWindowsImportOptions{Castools: true}
	if !o.waitCastools() {
		t.Fatal("castools must wait by default")
	}
	o.NoWaitCastools = true
	if o.waitCastools() {
		t.Fatal("opt-out ignored")
	}
}

// N-R1: the current firmware is sent back; autoStart is echoed.
func TestWindowsImportBootFirmwareEchoed(t *testing.T) {
	for name, c := range map[string]struct {
		fw, as interface{}
		want   string
	}{
		"bios":            {0, 0, `{"bootDev":{"autoStart":0,"bootFirmware":0,"bootdevs":"vda,hda,fda"},"id":50}`},
		"efi autostart":   {1, 1, `{"bootDev":{"autoStart":1,"bootFirmware":1,"bootdevs":"vda,hda,fda"},"id":50}`},
		"string, no auto": {"1", nil, `{"bootDev":{"bootFirmware":1,"bootdevs":"vda,hda,fda"},"id":50}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			r.f.firmware, r.f.autoStart = c.fw, c.as
			res, err := r.run(t)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, b := range r.f.bodies {
				if strings.HasPrefix(b, "PUT vm/modify ") && strings.Contains(b, "bootDev") {
					got = strings.TrimPrefix(b, "PUT vm/modify ")
				}
			}
			if got != c.want {
				t.Fatalf("body %s, want %s", got, c.want)
			}
			found := false
			for _, st := range res.Steps {
				found = found || st.Step == "verify-vm-unchanged"
			}
			if !found {
				t.Fatal("vm/detail diff not recorded")
			}
		})
	}
}

// N-R1: any change outside the boot order (or an unknown firmware) keeps
// the VM and makes no template; values are never printed.
func TestWindowsImportBootChangeSideEffects(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(f *winFake)
		want  string
	}{
		"memory":      {func(f *winFake) { f.bootSideEffect = func(d dict) { d["memory"] = dict{"size": 2048, "unit": "MB"} } }, "memory.size"},
		"cpu removed": {func(f *winFake) { f.bootSideEffect = func(d dict) { delete(d, "cpu") } }, "cpu"},
		"nic dropped": {func(f *winFake) { f.bootSideEffect = func(d dict) { d["network"] = []dict{} } }, "network"},
		"disk added": {func(f *winFake) {
			f.bootSideEffect = func(d dict) {
				d["storage"] = append(d["storage"].([]dict), dict{"device": "disk", "deviceName": "vdb", "path": "/vms/images/x.qcow2"})
			}
		}, "storage"},
		"description": {func(f *winFake) { f.bootSideEffect = func(d dict) { d["title"] = "changed" } }, "title"},
		"console pw": {func(f *winFake) {
			f.bootSideEffect = func(d dict) {
				d["graphics"] = []dict{{"type": "vnc", "port": 5908, "address": "0.0.0.0", "password": "other-secret"}}
			}
		}, "graphics[0].password"},
		"autostart":      {func(f *winFake) { f.bootSideEffect = func(d dict) { d["bootDev"] = dict{"autoStart": 1} } }, "bootDev.autoStart"},
		"firmware flip":  {func(f *winFake) { f.firmware, f.firmwareAfter = 1, 0 }, "boot.detail.bootFirmware (1 -> 0)"},
		"firmware gone":  {func(f *winFake) { f.firmware = nil }, "no boot.detail.bootFirmware"},
		"firmware odd":   {func(f *winFake) { f.firmware = 2 }, "unknown bootFirmware"},
		"detail/all 500": {func(f *winFake) { f.detailAllFail = true }, "boot firmware"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			c.setup(r.f)
			_, err := r.run(t)
			assertKept(t, r, err)
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "refused to clone it") || r.f.wrote("POST /cas/casrs/vmTemplate") {
				t.Fatalf("want %q: %v", c.want, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("a value leaked: %v", err)
			}
		})
	}
	// status/uptime may change: not a side effect
	r := newWinRun(t)
	r.f.bootSideEffect = func(d dict) { d["status"], d["uptime"] = "running", 99 }
	if _, err := r.run(t); err != nil {
		t.Fatal(err)
	}
}

func TestJSONDiffPaths(t *testing.T) {
	a := jsonutils.Marshal(dict{"x": 1, "s": []dict{{"p": "a"}}, "bootDev": dict{"bootdevs": "a", "autoStart": 0}, "status": "a"})
	b := jsonutils.Marshal(dict{"x": 1, "s": []dict{{"p": "b"}}, "bootDev": dict{"bootdevs": "b", "autoStart": 0}, "status": "b", "n": 1})
	got, err := jsonDiffPaths(a, b, winBootDiffIgnore)
	if err != nil || strings.Join(got, ",") != "n,s[0].p" {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := jsonDiffPaths(a, a, nil); len(got) != 0 {
		t.Fatal(got)
	}
}

// N-R3: the pool answer ISO delete is confirmed by a rescan; otherwise a
// loud SECRET LEFT warning names it.
func TestWindowsImportAnswerDeleteConfirmed(t *testing.T) {
	r := newWinRun(t)
	res, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range res.Steps {
		found = found || st.Step == "confirm-answer-iso-gone"
	}
	if !found || hasWarning(res, "SECRET LEFT") {
		t.Fatalf("not confirmed: %v", res.Warnings)
	}
	for name, setup := range map[string]func(f *winFake){
		"delete 500":  func(f *winFake) { f.answerDelCode = http.StatusInternalServerError },
		"still there": func(f *winFake) { f.answerDelNoop = true },
	} {
		t.Run(name, func(t *testing.T) {
			r := newWinRun(t)
			setup(r.f)
			res, err := r.run(t)
			if err == nil || res == nil || res.Template == nil {
				t.Fatalf("want template plus error: %v", err)
			}
			if !hasWarning(res, "SECRET LEFT ON THE CAS NODE: answer ISO /vms/isos/"+winAnswer) {
				t.Fatalf("no loud warning: %v", res.Warnings)
			}
			if !r.f.hasVol("isopool", winAnswer) {
				t.Fatal("fake: answer ISO should still be listed")
			}
			assertNoWinSecret(t, r.f, res, err)
		})
	}
}
