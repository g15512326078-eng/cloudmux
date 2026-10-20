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

	SshUpload                string `help:"Mode b: local qcow2 to copy into the pool over SSH first (off unless set; needs a cmx built with -tags cas_ssh)"`
	SshAddr                  string `help:"SSH host:port (default <host ip>:22)"`
	SshUser                  string `help:"SSH user; it only needs write access to the pool directory" default:"root"`
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
	stager, err := o.sshStager()
	if err != nil {
		return ret, err
	}
	ret.Stager = stager
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
		for _, w := range args.sshPreflightWarnings() {
			// before anything connects, not only in the result
			fmt.Fprintln(os.Stderr, "WARNING: "+w)
		}
		res, err := p.GetClient().ImportQcow2AsTemplate(opts)
		if res != nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if eerr := enc.Encode(res); eerr != nil {
				fmt.Fprintln(os.Stderr, eerr)
			}
			for _, w := range res.Warnings {
				fmt.Fprintln(os.Stderr, "WARNING: "+w)
			}
		}
		// a non-nil err makes cmx exit non-zero, also when a template was
		// created but failed the backing-file check
		return err
	})
}
