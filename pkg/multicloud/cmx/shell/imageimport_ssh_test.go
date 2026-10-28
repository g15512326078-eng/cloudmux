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

//go:build cas_ssh

package shell

import (
	"fmt"
	"strings"
	"testing"

	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

func TestImageImportOptionsSSH(t *testing.T) {
	o := &ImageImportTemplateOptions{HostId: 1, Pool: "p", File: "f.qcow2", TemplateName: "t", Vswitch: "vs", SshAddr: "h:22"}
	t.Setenv(SSH_PASSWORD_ENV, "env-pw")
	o.SshUpload = "/tmp/x.qcow2"
	io, err := o.importOptions()
	st, ok := io.Stager.(*cas.SSSHStager)
	if err != nil || !ok || st.Options.Password != "env-pw" || st.Options.Addr != "h:22" {
		t.Fatalf("mode b: %v %v", err, io.Stager)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %s %#v", st, st, st, st), "env-pw") {
		t.Fatal("password in String()")
	}
	o.SshInsecureIgnoreHostKey = true
	if w := o.sshPreflightWarnings(); len(w) != 1 || w[0] != cas.SSHInsecureHostKeyWarning {
		t.Fatalf("warnings %v", w)
	}
}
