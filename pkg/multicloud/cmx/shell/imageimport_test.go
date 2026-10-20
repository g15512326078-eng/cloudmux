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

package shell

import (
	"testing"

	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

func TestImageImportOptions(t *testing.T) {
	o := &ImageImportTemplateOptions{HostId: 1, Pool: "p", File: "f.qcow2", TemplateName: "t", Vswitch: "vs"}
	io, err := o.importOptions()
	if err != nil || io.Stager != nil {
		t.Fatalf("mode a: %v %v", err, io.Stager)
	}
	o.SshAddr = "h:22"
	if _, err := o.importOptions(); err == nil {
		t.Fatal("--ssh-* without --ssh-upload must fail")
	}
	h := &ImageImportTemplateOptions{HostId: 1, Pool: "p", File: "f.qcow2", TemplateName: "t", Vswitch: "vs", HttpUpload: "/tmp/x.qcow2", ServeAddr: "0.0.0.0:9292", ServeAdvertise: "198.51.100.7:9292"}
	hi, err := h.importOptions()
	hs, ok := hi.Stager.(*cas.SHTTPStager)
	if err != nil || !ok || hs.Options.ServeAddr != "0.0.0.0:9292" || hs.Options.AdvertiseAddr != "198.51.100.7:9292" {
		t.Fatalf("mode http: %v %v", err, hi.Stager)
	}
	h.ServeAnyAddr = true
	if hi, err := h.importOptions(); err != nil || !hi.Stager.(*cas.SHTTPStager).Options.AllowAnyAddr {
		t.Fatalf("--serve-any-addr not passed: %v", err)
	}
	for name, bad := range map[string]ImageImportTemplateOptions{
		"http without serve-addr": {HttpUpload: "/tmp/x"},
		"ssh and http":            {HttpUpload: "/tmp/x", ServeAddr: "a:1", SshUpload: "/tmp/y"},
		"serve-addr in pool-file": {ServeAddr: "a:1", UploadMode: "pool-file"},
		"ssh flag in http mode":   {HttpUpload: "/tmp/x", ServeAddr: "a:1", SshKnownHosts: "k"},
		"http mode, no file":      {UploadMode: "http", ServeAddr: "a:1"},
		"serve-any in pool-file":  {ServeAnyAddr: true},
	} {
		if _, err := bad.importOptions(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
