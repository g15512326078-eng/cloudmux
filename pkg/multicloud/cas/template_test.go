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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

type tplFake struct {
	t *testing.T

	lock      sync.Mutex
	hits      []string
	templates []dict
	vms       []dict
	polls     map[int64]int
	lastBody  string
	// noopModify: modifyVmTemplate "succeeds" but leaves the name alone
	noopModify bool
	// deleteNotFound: deleteVmTemplateInfo answers 404; deleteRemoves
	// says whether the template is gone anyway (concurrent delete).
	deleteNotFound bool
	deleteRemoves  bool
	// infoRaw, when set, is the literal vmTemplateInfo body (for shapes
	// such as "storages": null that dict marshalling cannot express).
	infoRaw string
}

func (f *tplFake) writes() []string {
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

func (f *tplFake) find(id int64) dict {
	for _, tpl := range f.templates {
		if fmt.Sprintf("%v", tpl["id"]) == fmt.Sprintf("%d", id) {
			return tpl
		}
	}
	return nil
}

func (f *tplFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.RequestURI())
	path := strings.TrimPrefix(r.URL.Path, API_PREFIX+"/")
	q := r.URL.Query()
	var id int64
	fmt.Sscanf(q.Get("id"), "%d", &id)
	switch {
	case r.Method == http.MethodGet && path == "vmTemplate/all":
		reply(w, dict{"domain": append([]dict{}, f.templates...)})
	case r.Method == http.MethodGet && path == "vmTemplate/vmTemplateInfo":
		if len(f.infoRaw) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(f.infoRaw))
			return
		}
		tpl := f.find(id)
		if tpl == nil {
			w.Header().Set("Error-Code", "1000")
			w.WriteHeader(http.StatusConflict)
			return
		}
		reply(w, tpl)
	case r.Method == http.MethodGet && path == "vm/vmList":
		reply(w, dict{"domain": append([]dict{}, f.vms...)})
	case r.Method == http.MethodPost && path == "vmTemplate/addVmTemplate":
		f.templates = append(f.templates, dict{"id": 77, "name": q.Get("domainName"), "description": q.Get("desc"),
			"cpu": 2, "memory": 2048, "system": 1, "storageCapacity": 20480, "deployed": false})
		reply(w, dict{"msgId": 7001, "completed": false, "targetId": id})
	case r.Method == http.MethodPut && path == "vmTemplate/modifyVmTemplate":
		data, _ := io.ReadAll(r.Body)
		f.lastBody = string(data)
		body, _ := jsonutils.Parse(data)
		tid, _ := body.Int("id")
		if tpl := f.find(tid); tpl != nil && !f.noopModify {
			tpl["name"], _ = body.GetString("name")
			tpl["description"], _ = body.GetString("description")
		}
		reply(w, dict{"msgId": 7002, "completed": false, "targetId": tid})
	case r.Method == http.MethodDelete && path == "vmTemplate/deleteVmTemplateInfo" && f.deleteNotFound:
		if f.deleteRemoves {
			kept := []dict{}
			for _, tpl := range f.templates {
				if fmt.Sprintf("%v", tpl["id"]) != q.Get("id") {
					kept = append(kept, tpl)
				}
			}
			f.templates = kept
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodDelete && path == "vmTemplate/deleteVmTemplateInfo":
		kept := []dict{}
		for _, tpl := range f.templates {
			if fmt.Sprintf("%v", tpl["id"]) != q.Get("id") {
				kept = append(kept, tpl)
			}
		}
		f.templates = kept
		reply(w, dict{"msgId": 7003, "completed": false, "targetId": id})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "message/"):
		var mid int64
		fmt.Sscanf(strings.TrimPrefix(path, "message/"), "%d", &mid)
		f.polls[mid]++
		reply(w, dict{"msgId": mid, "completed": f.polls[mid] >= 2, "result": 0})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.RequestURI())
		w.WriteHeader(http.StatusNotFound)
	}
}

// testStorages carries an unknown field ("futureField", nested) that a
// rename must send back as-is.
func testStorages() []dict {
	return []dict{
		{"id": 100, "capacity": 4096, "storeFile": "/vms/template/tmpName.qcow2", "targetBus": "virtio",
			"futureField": dict{"nested": []interface{}{1, "x", true}}, "ratio": 0.5},
		{"id": 101, "capacity": 10240, "storeFile": "/vms/template/tmpName-1.qcow2"},
	}
}

// jsonEqual compares two JSON texts semantically (encoding/json values).
func jsonEqual(t *testing.T, a, b string) bool {
	var va, vb interface{}
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Fatalf("unmarshal %s: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	return reflect.DeepEqual(va, vb)
}

func newTplFake(t *testing.T) (*tplFake, *SRegion) {
	f := &tplFake{t: t, polls: map[int64]int{}}
	f.templates = []dict{
		{"id": 4, "name": "tmpName", "title": "tmpName", "description": "desc", "deployed": false,
			"cpuSockets": 2, "cpuCores": 2, "memory": 4096, "memoryInit": 4, "memoryUnit": "GB", "system": 1,
			"storages": testStorages()},
		{"id": 5, "name": "other", "description": "", "deployed": false},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.taskPollInterval = 10 * time.Millisecond
	cli.taskTimeout = 2 * time.Second
	return f, &SRegion{cli: cli}
}

func TestVmTemplateInfoFixture(t *testing.T) {
	obj, err := jsonutils.ParseString(fixture(t, "vmtemplate_info"))
	if err != nil {
		t.Fatal(err)
	}
	info := &SVmTemplateInfo{}
	if err := obj.Unmarshal(info); err != nil {
		t.Fatal(err)
	}
	if info.Id != 4 || info.Name != "tmpName" || !info.Deployed || info.Memory != 1024 || info.CpuSockets != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestSaveImageClonesVM(t *testing.T) {
	f, region := newTplFake(t)
	vm := &SInstance{Id: 9, Uuid: "uuid-9", Name: "vm9", region: region}
	img, err := vm.SaveImage(&cloudprovider.SaveImageOptions{Name: " golden ", Notes: "from vm9"})
	if err != nil {
		t.Fatal(err)
	}
	if img.GetId() != "77" || img.GetName() != "golden" || img.GetDescription() != "from vm9" {
		t.Fatalf("image = %s %s %s", img.GetId(), img.GetName(), img.GetDescription())
	}
	w := f.writes()
	if len(w) != 1 || !strings.HasPrefix(w[0], "POST /cas/casrs/vmTemplate/addVmTemplate?") {
		t.Fatalf("writes = %v", w)
	}
	for _, want := range []string{"id=9", "uuid=uuid-9", "domainName=golden", "desc=from+vm9", "opType=1"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("addVmTemplate %q missing %q", w[0], want)
		}
	}
	for _, never := range []string{"opType=2", "scope=", "templetStoragePath=", "disConnect="} {
		if strings.Contains(w[0], never) {
			t.Errorf("addVmTemplate %q must not carry %q", w[0], never)
		}
	}
	if f.polls[7001] < 2 {
		t.Errorf("task 7001 polled %d times, want waitTask until completed", f.polls[7001])
	}
}

func TestSaveImageRefusesDuplicateName(t *testing.T) {
	f, region := newTplFake(t)
	vm := &SInstance{Id: 9, Uuid: "uuid-9", region: region}
	_, err := vm.SaveImage(&cloudprovider.SaveImageOptions{Name: "other"})
	if errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Fatalf("err = %v, want ErrDuplicateId", err)
	}
	_, err = vm.SaveImage(&cloudprovider.SaveImageOptions{Name: "  "})
	if errors.Cause(err) != cloudprovider.ErrMissingParameter {
		t.Fatalf("empty name err = %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
}

func TestRenameTemplateBodyKeepsConfig(t *testing.T) {
	f, region := newTplFake(t)
	img := &SImage{SVmTemplate: SVmTemplate{Id: 4, Name: "tmpName"}, cache: region.getStoragecache()}
	if err := img.Rename("renamed", nil); err != nil {
		t.Fatal(err)
	}
	if img.GetName() != "renamed" || img.GetDescription() != "desc" {
		t.Fatalf("after rename: %q %q", img.GetName(), img.GetDescription())
	}
	body, err := jsonutils.ParseString(f.lastBody)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"id": "4", "name": `"renamed"`, "description": `"desc"`, "memory": "4096",
		"memoryInit": "4", "memoryUnit": `"GB"`, "cpu": "4", "cpuSocket": "2", "cpuCore": "2"}
	for k, v := range want {
		got, err := body.Get(k)
		if err != nil || got.String() != v {
			t.Errorf("body %s = %v, want %s (body %s)", k, got, v, f.lastBody)
		}
	}
	if body.Contains("domain") {
		t.Errorf("rename body must not carry a wrapper: %s", f.lastBody)
	}
	// storages must come back exactly as vmTemplateInfo returned them
	sent, err := body.Get("storages")
	if err != nil {
		t.Fatalf("rename body has no storages: %s", f.lastBody)
	}
	if !jsonEqual(t, sent.String(), jsonutils.Marshal(testStorages()).String()) {
		t.Errorf("storages sent %s, want %s", sent, jsonutils.Marshal(testStorages()))
	}
	w := f.writes()
	if len(w) != 1 || w[0] != "PUT /cas/casrs/vmTemplate/modifyVmTemplate" {
		t.Fatalf("writes = %v", w)
	}

	desc := "new desc"
	if err := img.Rename("renamed", &desc); err != nil || img.GetDescription() != "new desc" {
		t.Fatalf("desc update: %v %q", err, img.GetDescription())
	}
}

func TestRenameTemplateRefusals(t *testing.T) {
	f, region := newTplFake(t)
	cli := region.cli
	if _, err := cli.RenameVmTemplate(4, "other", nil); errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Errorf("dup name err = %v", err)
	}
	f.templates[0]["deployed"] = true
	if _, err := cli.RenameVmTemplate(4, "renamed", nil); errors.Cause(err) != cloudprovider.ErrInvalidStatus {
		t.Errorf("deployed err = %v", err)
	}
	if _, err := cli.RenameVmTemplate(404, "x", nil); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("missing err = %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
	// unchanged name/desc is a no-op without a write
	f.templates[0]["deployed"] = false
	if tpl, err := cli.RenameVmTemplate(4, "tmpName", nil); err != nil || tpl.Name != "tmpName" {
		t.Fatalf("no-op rename: %v %v", tpl, err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("no-op writes = %v", w)
	}
	// a task that succeeds without changing the name is an error
	f.noopModify = true
	if _, err := cli.RenameVmTemplate(4, "renamed", nil); err == nil || !strings.Contains(err.Error(), "still named") {
		t.Fatalf("noop modify err = %v", err)
	}
}

func TestDeleteTemplate(t *testing.T) {
	f, region := newTplFake(t)
	img := &SImage{SVmTemplate: SVmTemplate{Id: 5}, cache: region.getStoragecache()}
	if err := img.Delete(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := f.writes()
	if len(w) != 1 || w[0] != "DELETE /cas/casrs/vmTemplate/deleteVmTemplateInfo?id=5" {
		t.Fatalf("writes = %v", w)
	}
	if f.find(5) != nil {
		t.Fatal("template 5 still present")
	}
	// already gone: nil, no further write
	if err := img.Delete(context.Background()); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if w := f.writes(); len(w) != 1 {
		t.Fatalf("writes after second delete = %v", w)
	}
}

func TestDeleteTemplateRefusedWhenInUse(t *testing.T) {
	f, region := newTplFake(t)
	cli := region.cli
	// a recycled VM deployed from the template still blocks
	f.vms = []dict{
		{"id": 9, "uuid": "uuid-9", "name": "vm9", "templateId": 4, "delTime": "2020-10-29T22:54:10+08:00"},
		{"id": 10, "uuid": "uuid-10", "name": "vm10", "templateId": 5},
	}
	err := cli.DeleteVmTemplate(4)
	if errors.Cause(err) != cloudprovider.ErrInvalidStatus || !strings.Contains(err.Error(), "vm9(uuid-9)") {
		t.Fatalf("in-use err = %v", err)
	}
	f.vms = nil
	f.templates[0]["deployed"] = true
	err = cli.DeleteVmTemplate(4)
	if errors.Cause(err) != cloudprovider.ErrInvalidStatus || !strings.Contains(err.Error(), "fast-deployed") {
		t.Fatalf("deployed err = %v", err)
	}
	if w := f.writes(); len(w) != 0 {
		t.Fatalf("writes = %v, want none", w)
	}
}

func TestTemplateWritesBlockedWhenReadOnly(t *testing.T) {
	f, region := newTplFake(t)
	cli := region.cli
	cli.cpcfg.ReadOnly = true
	vm := &SInstance{Id: 9, Uuid: "uuid-9", region: region}
	img := &SImage{SVmTemplate: SVmTemplate{Id: 5}, cache: region.getStoragecache()}
	for name, fn := range map[string]func() error{
		"SaveImage": func() error { _, err := vm.SaveImage(&cloudprovider.SaveImageOptions{Name: "x"}); return err },
		"Rename":    func() error { return img.Rename("y", nil) },
		"Delete":    func() error { return img.Delete(context.Background()) },
	} {
		if err := fn(); errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
			t.Errorf("%s: %v, want ErrAccountReadOnly", name, err)
		}
	}
	if len(f.hits) != 0 {
		t.Fatalf("read-only account reached the server: %v", f.hits)
	}
	// the transport guard still blocks a direct write
	if _, err := cli.doAsync("DELETE", "vmTemplate/deleteVmTemplateInfo", nil, nil); errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Fatalf("transport guard: %v", err)
	}
	if len(f.hits) != 0 {
		t.Fatalf("hits = %v", f.hits)
	}
}

// B1: no write unless storages can be echoed unchanged.
func TestRenameTemplateRefusedWithoutStorages(t *testing.T) {
	for name, storages := range map[string]interface{}{
		"missing":       nil,
		"null":          "RAW",
		"empty":         []dict{},
		"object":        dict{"storage": testStorages()},
		"string":        "/vms/template/x",
		"non-object el": []interface{}{testStorages()[0], 7},
	} {
		t.Run(name, func(t *testing.T) {
			f, region := newTplFake(t)
			if storages == nil {
				delete(f.templates[0], "storages")
			} else if storages == "RAW" {
				f.infoRaw = `{"id":4,"name":"tmpName","description":"desc","storages":null}`
			} else {
				f.templates[0]["storages"] = storages
			}
			_, err := region.cli.RenameVmTemplate(4, "renamed", nil)
			if errors.Cause(err) != cloudprovider.ErrNotSupported || !strings.Contains(err.Error(), "refusing to rename") {
				t.Fatalf("err = %v, want refusal", err)
			}
			if w := f.writes(); len(w) != 0 {
				t.Fatalf("writes = %v, want none", w)
			}
		})
	}
}

// N2: DELETE answering not-found is success only if vmTemplate/all agrees.
func TestDeleteTemplateNotFoundMidFlight(t *testing.T) {
	f, region := newTplFake(t)
	f.deleteNotFound, f.deleteRemoves = true, true
	if err := region.cli.DeleteVmTemplate(5); err != nil {
		t.Fatalf("gone after 404: %v", err)
	}

	f, region = newTplFake(t)
	f.deleteNotFound, f.deleteRemoves = true, false
	err := region.cli.DeleteVmTemplate(5)
	if err == nil || !strings.Contains(err.Error(), "does not confirm it is gone") {
		t.Fatalf("still listed after 404: err = %v, want error", err)
	}
}
