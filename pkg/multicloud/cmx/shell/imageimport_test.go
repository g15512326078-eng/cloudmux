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
}
