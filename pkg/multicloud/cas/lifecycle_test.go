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
	"sync"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

type lifeFake struct {
	t *testing.T

	lock     sync.Mutex
	hits     []string // "METHOD path?query"
	messages map[int64]*SMessage
	polls    map[int64]int // times GetMessage called before flipping completed

	deployBody string
}

func (f *lifeFake) record(r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
}

func (f *lifeFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.record(r)
	path := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	parts := strings.Split(path, "/")
	switch {
	case r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "vm" &&
		(parts[1] == "start" || parts[1] == "stop" || parts[1] == "powerOff" || parts[1] == "restart"):
		ids := map[string]int64{"start": 1001, "stop": 1002, "powerOff": 1003, "restart": 1004}
		reply(w, dict{"msgId": ids[parts[1]], "completed": false, "progress": 0, "targetId": 9, "targetName": "vm9"})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "vm/delete/uuid/"):
		reply(w, dict{"msgId": 1005, "completed": false, "targetId": 9})
	case r.Method == http.MethodPost && path == "vm/deploy":
		buf := make([]byte, r.ContentLength)
		n, _ := r.Body.Read(buf)
		f.lock.Lock()
		f.deployBody = string(buf[:n])
		f.lock.Unlock()
		reply(w, dict{"msgId": 1006, "completed": false, "targetName": "newvm"})
	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "message":
		var id int64
		fmt.Sscanf(parts[1], "%d", &id)
		f.lock.Lock()
		f.polls[id]++
		n := f.polls[id]
		msg, ok := f.messages[id]
		if !ok {
			f.lock.Unlock()
			w.WriteHeader(http.StatusNotFound)
			return
		}
		out := *msg
		if !out.Completed && n >= 2 {
			out.Completed = true
			if out.Result == 0 && out.FailMsg == "FAIL" {
				// leave result as set by test
			}
		}
		f.lock.Unlock()
		reply(w, out)
	case r.Method == http.MethodGet && path == "vmvnc/vnc/9":
		reply(w, dict{"ip": "10.0.0.9", "port": 5909, "password": "VNC-SECRET"})
	case r.Method == http.MethodGet && path == "vm/detail/42":
		reply(w, dict{"id": 42, "uuid": "uuid-new", "name": "newvm", "status": "shutOff", "storage": []dict{}, "network": []dict{}})
	case r.Method == http.MethodGet && path == "vm/detail/uuid/uuid-new":
		reply(w, dict{"id": 42, "uuid": "uuid-new", "name": "newvm", "status": "shutOff", "storage": []dict{}, "network": []dict{}})
	case r.Method == http.MethodGet && path == "vm/vmList":
		reply(w, dict{"domain": []dict{{"id": 42, "uuid": "uuid-new", "name": "newvm", "title": "newvm", "hostId": 1, "hostPoolId": 1, "cpu": 2, "memory": 2048, "vmStatus": "shutOff", "system": 1}}})
	case r.Method == http.MethodGet && path == "hostpool/all":
		reply(w, dict{"hostPool": []dict{{"id": 1, "name": "hp1"}}})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

func newLifeClient(t *testing.T, f *lifeFake) *SCasClient {
	if f.messages == nil {
		f.messages = map[int64]*SMessage{}
	}
	if f.polls == nil {
		f.polls = map[int64]int{}
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 10 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	return cli
}

func TestWaitTaskSuccessAndFailure(t *testing.T) {
	f := &lifeFake{t: t, messages: map[int64]*SMessage{
		1001: {MsgId: 1001, Result: TASK_RESULT_SUCCESS, FailMsg: "ok"},
		1002: {MsgId: 1002, Result: TASK_RESULT_FAILURE, FailMsg: "boom"},
	}}
	cli := newLifeClient(t, f)

	msg, err := cli.waitTask(1001)
	if err != nil || msg == nil || !msg.Completed || msg.Result != 0 {
		t.Fatalf("success: msg=%v err=%v", msg, err)
	}
	if f.polls[1001] < 2 {
		t.Errorf("expected at least 2 polls, got %d", f.polls[1001])
	}

	_, err = cli.waitTask(1002)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("failure: err=%v", err)
	}

	_, err = cli.waitTask(0)
	if err == nil {
		t.Fatal("empty msgId should error")
	}
}

func TestLifecycleOps(t *testing.T) {
	f := &lifeFake{t: t, messages: map[int64]*SMessage{
		1001: {MsgId: 1001, Result: TASK_RESULT_SUCCESS, TargetId: 9},
		1002: {MsgId: 1002, Result: TASK_RESULT_SUCCESS, TargetId: 9},
		1003: {MsgId: 1003, Result: TASK_RESULT_SUCCESS, TargetId: 9},
		1004: {MsgId: 1004, Result: TASK_RESULT_SUCCESS, TargetId: 9},
		1005: {MsgId: 1005, Result: TASK_RESULT_SUCCESS, TargetId: 9},
	}}
	cli := newLifeClient(t, f)
	region := &SRegion{cli: cli}
	vm := &SInstance{Id: 9, Uuid: "uuid-9", region: region}

	if err := vm.StartVM(context.Background()); err != nil {
		t.Fatalf("StartVM: %v", err)
	}
	if err := vm.StopVM(context.Background(), &cloudprovider.ServerStopOptions{}); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if err := vm.StopVM(context.Background(), &cloudprovider.ServerStopOptions{IsForce: true}); err != nil {
		t.Fatalf("StopVM force: %v", err)
	}
	if err := vm.RestartVM(context.Background()); err != nil {
		t.Fatalf("RestartVM: %v", err)
	}
	if err := vm.DeleteVM(context.Background()); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}

	want := []string{
		"PUT /cas/casrs/vm/start/9",
		"GET /cas/casrs/message/1001",
		"PUT /cas/casrs/vm/stop/9",
		"GET /cas/casrs/message/1002",
		"PUT /cas/casrs/vm/powerOff/9",
		"GET /cas/casrs/message/1003",
		"PUT /cas/casrs/vm/restart/9",
		"GET /cas/casrs/message/1004",
		"DELETE /cas/casrs/vm/delete/uuid/uuid-9?isWipeVolume=true&type=1",
		"GET /cas/casrs/message/1005",
	}
	f.lock.Lock()
	hits := append([]string{}, f.hits...)
	f.lock.Unlock()
	// Keep only the first poll of each message to compare paths
	compact := []string{}
	seenMsg := map[string]bool{}
	for _, h := range hits {
		if strings.HasPrefix(h, "GET /cas/casrs/message/") {
			if seenMsg[h] {
				continue
			}
			seenMsg[h] = true
		}
		compact = append(compact, h)
	}
	if strings.Join(compact, "\n") != strings.Join(want, "\n") {
		t.Errorf("hits:\n%s\nwant:\n%s", strings.Join(compact, "\n"), strings.Join(want, "\n"))
	}
}

func TestCreateVMDeploy(t *testing.T) {
	f := &lifeFake{t: t, messages: map[int64]*SMessage{
		1006: {MsgId: 1006, Result: TASK_RESULT_SUCCESS, TargetId: 42, TargetName: "newvm"},
	}}
	cli := newLifeClient(t, f)
	region := &SRegion{cli: cli}
	zone := &SZone{SHostPool: SHostPool{Id: 1, Name: "hp1"}, region: region}
	host := &SHost{Id: 7, Name: "cvk7", HostPoolId: 1, zone: zone}

	vm, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{
		Name:              "newvm",
		ExternalImageId:   "CAS/acc1/template/11",
		Cpu:               4,
		MemoryMB:          4096,
		ExternalNetworkId: "vswitch/vswitch0/hostpool/1/default",
		IpAddr:            "10.0.0.50",
		SysDisk:           cloudprovider.SDiskInfo{StorageExternalId: "host/7/defaultpool"},
		Description:       "p1 test",
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if vm.GetGlobalId() != "uuid-new" || vm.GetName() != "newvm" {
		t.Errorf("vm = %s %s", vm.GetGlobalId(), vm.GetName())
	}
	f.lock.Lock()
	body := f.deployBody
	f.lock.Unlock()
	obj, err := jsonutils.ParseString(body)
	if err != nil {
		t.Fatalf("deploy body: %v (%s)", err, body)
	}
	checks := map[string]interface{}{
		"id": int64(11), "domainName": "newvm", "title": "newvm", "deployType": int64(1),
		"deployMode": int64(0), "targetHostId": int64(7), "cpuSocket": int64(4), "cpuCore": int64(1),
		"memory": int64(4096), "desc": "p1 test",
	}
	for k, want := range checks {
		got, _ := obj.Get(k)
		if jsonutils.Marshal(want).String() != got.String() {
			t.Errorf("deploy.%s = %s, want %v", k, got, want)
		}
	}
	net, _ := obj.GetArray("network")
	if len(net) != 1 {
		t.Fatalf("network = %v", net)
	}
	if vs, _ := net[0].GetString("vSwitchName"); vs != "vswitch0" {
		t.Errorf("vSwitchName = %s", vs)
	}
	if ip, _ := net[0].GetString("ip"); ip != "10.0.0.50" {
		t.Errorf("ip = %s", ip)
	}
	st, _ := obj.GetArray("storage")
	if len(st) != 1 {
		t.Fatalf("storage = %v", st)
	}
	if pool, _ := st[0].GetString("pool"); pool != "defaultpool" {
		t.Errorf("pool = %s", pool)
	}
	foundDeploy := false
	for _, h := range f.hits {
		if h == "POST /cas/casrs/vm/deploy" {
			foundDeploy = true
		}
	}
	if !foundDeploy {
		t.Errorf("missing deploy POST: %v", f.hits)
	}
}

func TestGetVNCInfo(t *testing.T) {
	f := &lifeFake{t: t}
	cli := newLifeClient(t, f)
	vm := &SInstance{Id: 9, Uuid: "uuid-9", region: &SRegion{cli: cli}}
	out, err := vm.GetVNCInfo(nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Protocol != "vnc" || out.Host != "10.0.0.9" || out.Port != 5909 || out.Password != "VNC-SECRET" || out.Hypervisor != api.HYPERVISOR_CAS {
		t.Errorf("vnc = %+v", out)
	}
}

func TestWriteOpsBlockedWhenReadOnly(t *testing.T) {
	f := &lifeFake{t: t}
	cli := newLifeClient(t, f)
	cli.cpcfg.ReadOnly = true
	region := &SRegion{cli: cli}
	vm := &SInstance{Id: 9, Uuid: "uuid-9", region: region}
	for name, fn := range map[string]func() error{
		"StartVM":   func() error { return vm.StartVM(context.Background()) },
		"StopVM":    func() error { return vm.StopVM(context.Background(), nil) },
		"RestartVM": func() error { return vm.RestartVM(context.Background()) },
		"DeleteVM":  func() error { return vm.DeleteVM(context.Background()) },
	} {
		err := fn()
		if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
			t.Errorf("%s: %v, want ErrAccountReadOnly", name, err)
		}
	}
	host := &SHost{Id: 1, zone: &SZone{region: region}}
	_, err := host.CreateVM(&cloudprovider.SManagedVMCreateConfig{Name: "x", ExternalImageId: "1", Cpu: 1, MemoryMB: 512})
	if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Errorf("CreateVM: %v, want ErrAccountReadOnly", err)
	}
	// no write should have reached the fake beyond the blocked attempt
	for _, h := range f.hits {
		if !strings.HasPrefix(h, "GET ") {
			t.Errorf("write reached server: %s", h)
		}
	}
}

func TestParseTemplateId(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"11", 11, false},
		{"template/11", 11, false},
		{"CAS/acc1/template/11", 11, false},
		{"", 0, true},
		{"nope", 0, true},
	}
	for _, c := range cases {
		got, err := parseTemplateId(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: want err", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %d %v", c.in, got, err)
		}
	}
}

func TestCapabilitiesWritableCompute(t *testing.T) {
	cli := &SCasClient{CasClientConfig: NewCasClientConfig("http://127.0.0.1", "u", "p")}
	caps := cli.GetCapabilities()
	joined := strings.Join(caps, ",")
	if !strings.Contains(joined, cloudprovider.CLOUD_CAPABILITY_COMPUTE) ||
		strings.Contains(joined, cloudprovider.CLOUD_CAPABILITY_COMPUTE+cloudprovider.READ_ONLY_SUFFIX) {
		t.Errorf("caps = %v, compute must be writable", caps)
	}
	if !strings.Contains(joined, cloudprovider.CLOUD_CAPABILITY_NETWORK+cloudprovider.READ_ONLY_SUFFIX) {
		t.Errorf("network should stay read-only: %v", caps)
	}
}
