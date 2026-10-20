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
	"os"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

func (o *ImageImportTemplateOptions) sshStager() (cas.ImportSourceStager, error) {
	up := cas.SSSHUploadOptions{
		Addr: o.SshAddr, User: o.SshUser, Password: o.SshPassword, LocalPath: o.SshUpload,
		KnownHostsFile: o.SshKnownHosts, HostKeySHA256: o.SshHostKeySha256, InsecureIgnoreHostKey: o.SshInsecureIgnoreHostKey,
	}
	if len(up.Password) == 0 {
		up.Password = os.Getenv(SSH_PASSWORD_ENV)
	}
	if len(o.SshKeyFile) > 0 {
		key, err := os.ReadFile(o.SshKeyFile)
		if err != nil {
			return nil, errors.Wrapf(err, "read ssh key file %s", o.SshKeyFile)
		}
		up.PrivateKey = key
	}
	return cas.NewSSHStager(up), nil
}

func (o *ImageImportTemplateOptions) sshPreflightWarnings() []string {
	if len(o.SshUpload) > 0 && o.SshInsecureIgnoreHostKey {
		return []string{cas.SSHInsecureHostKeyWarning}
	}
	return nil
}
