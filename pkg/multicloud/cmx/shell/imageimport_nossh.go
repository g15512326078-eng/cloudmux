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
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

// The default build does not link golang.org/x/crypto/ssh (x/crypto
// v0.19.0 has open advisories, see the review notes); SSH upload needs
// -tags cas_ssh.
const sshNotBuiltMsg = "SSH upload (--ssh-upload) is not compiled into this cmx; rebuild with: go build -tags cas_ssh ./cmd/cmx (or use the default pool-file mode)"

func (o *ImageImportTemplateOptions) sshStager() (cas.ImportSourceStager, error) {
	return nil, errors.Wrap(cloudprovider.ErrNotSupported, sshNotBuiltMsg)
}

func (o *ImageImportTemplateOptions) sshPreflightWarnings() []string { return nil }
