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
	"encoding/json"
	"fmt"
	"os"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas"
	casprovider "yunion.io/x/cloudmux/pkg/multicloud/cas/provider"
)

// SSH_PASSWORD_ENV is read when --ssh-password is not given.
const SSH_PASSWORD_ENV = "CAS_SSH_PASSWORD"

type ImageImportTemplateOptions struct {
	HostId       int64  `help:"CAS host id that sees the pool" required:"true"`
	Pool         string `help:"Storage pool name (file pool: dir/netfs/fs)" required:"true"`
	File         string `help:"qcow2 file name in the pool, or its absolute path inside the pool directory" required:"true"`
	TemplateName string `help:"Name of the new template" required:"true"`
	Desc         string `help:"Template description"`
	Vswitch      string `help:"vSwitch of the temporary VM NIC" required:"true"`
	Profile      string `help:"Network profile name" default:"Default"`
	OsVersion    string `help:"CAS osVersion string" default:"Other Linux(64-bit)"`
	Cpu          int    `help:"vCPUs of the temporary VM" default:"1"`
	MemoryMb     int    `help:"Memory MB of the temporary VM (>= 512)" default:"1024"`
	TempVmName   string `help:"Temporary VM name (default tplimp-<template name>)"`
	Refresh      bool   `help:"GET storage/refresh before looking the file up (use after copying it in by hand)"`
	DryRun       bool   `help:"Only read and print the planned calls; no writes"`

	SshUpload                string `help:"Mode b: local qcow2 to copy into the pool over SSH first (off unless set)"`
	SshAddr                  string `help:"SSH host:port (default <host ip>:22)"`
	SshUser                  string `help:"SSH user" default:"root"`
	SshPassword              string `help:"SSH password (prefer env CAS_SSH_PASSWORD; avoid ps/history)"`
	SshKeyFile               string `help:"SSH private key file (PEM, unencrypted)"`
	SshKnownHosts            string `help:"known_hosts file used to verify the host key"`
	SshHostKeySha256         string `help:"Pinned host key fingerprint, e.g. SHA256:... (ssh-keygen -lf)"`
	SshInsecureIgnoreHostKey bool   `help:"Explicitly skip host key verification (not recommended)"`
}

func (o *ImageImportTemplateOptions) importOptions() (cas.SImageImportOptions, error) {
	ret := cas.SImageImportOptions{
		HostId: o.HostId, PoolName: o.Pool, File: o.File, TemplateName: o.TemplateName, Description: o.Desc,
		OsVersion: o.OsVersion, CpuCores: o.Cpu, MemoryMB: o.MemoryMb, VSwitchName: o.Vswitch, ProfileName: o.Profile,
		TempVMName: o.TempVmName, Refresh: o.Refresh, DryRun: o.DryRun,
	}
	if len(o.SshUpload) == 0 {
		for _, s := range []string{o.SshAddr, o.SshPassword, o.SshKeyFile, o.SshKnownHosts, o.SshHostKeySha256} {
			if len(s) > 0 {
				return ret, errors.Wrap(cloudprovider.ErrInputParameter, "--ssh-* options need --ssh-upload")
			}
		}
		return ret, nil
	}
	up := &cas.SSSHUploadOptions{
		Addr: o.SshAddr, User: o.SshUser, Password: o.SshPassword, LocalPath: o.SshUpload,
		KnownHostsFile: o.SshKnownHosts, HostKeySHA256: o.SshHostKeySha256, InsecureIgnoreHostKey: o.SshInsecureIgnoreHostKey,
	}
	if len(up.Password) == 0 {
		up.Password = os.Getenv(SSH_PASSWORD_ENV)
	}
	if len(o.SshKeyFile) > 0 {
		key, err := os.ReadFile(o.SshKeyFile)
		if err != nil {
			return ret, errors.Wrapf(err, "read ssh key file %s", o.SshKeyFile)
		}
		up.PrivateKey = key
	}
	ret.Upload = up
	return ret, nil
}

func init() {
	cmd := NewCommand("image-import")
	NewCO[ImageImportTemplateOptions](cmd).RawRun("template", "Turn a qcow2 cloud image in a CAS pool (or uploaded over SSH) into a VM template; --dry-run prints the plan", func(cli ICloudProvider, args *ImageImportTemplateOptions) error {
		opts, err := args.importOptions()
		if err != nil {
			return err
		}
		if cli == nil || cli.GetProvider() == nil {
			return errors.Wrap(cloudprovider.ErrMissingParameter, "image-import-template needs CAS credentials")
		}
		p, ok := cli.GetProvider().(*casprovider.SCasProvider)
		if !ok {
			return errors.Wrap(cloudprovider.ErrNotSupported, "image-import-template requires the CAS provider")
		}
		res, err := p.GetClient().ImportQcow2AsTemplate(opts)
		if res != nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if eerr := enc.Encode(res); eerr != nil {
				fmt.Fprintln(os.Stderr, eerr)
			}
		}
		return err
	})
}
