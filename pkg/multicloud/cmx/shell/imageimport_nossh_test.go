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

//go:build !cas_ssh

package shell

import (
	"strings"
	"testing"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestImageImportOptionsSSHNotBuilt(t *testing.T) {
	o := &ImageImportTemplateOptions{HostId: 1, Pool: "p", File: "f.qcow2", TemplateName: "t", Vswitch: "vs", SshUpload: "/tmp/x.qcow2"}
	_, err := o.importOptions()
	if errors.Cause(err) != cloudprovider.ErrNotSupported || !strings.Contains(err.Error(), "-tags cas_ssh") {
		t.Fatalf("err = %v", err)
	}
	if w := o.sshPreflightWarnings(); len(w) != 0 {
		t.Fatalf("warnings %v", w)
	}
}
