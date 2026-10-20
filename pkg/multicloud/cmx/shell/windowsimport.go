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
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas"
	casprovider "yunion.io/x/cloudmux/pkg/multicloud/cas/provider"
)

// Secrets for the answer file: read from the environment when the flag
// is not given (flags show up in ps and shell history). Never printed.
const (
	WIN_ADMIN_PASSWORD_ENV = "CAS_WIN_ADMIN_PASSWORD"
	WIN_PRODUCT_KEY_ENV    = "CAS_WIN_PRODUCT_KEY"
)

// WindowsAnswerOptions are the autounattend.xml parameters.
type WindowsAnswerOptions struct {
	OsVersion         string   `help:"CAS osVersion, e.g. 'Microsoft Windows Server 2019(64-bit)'; picks the architecture and the default virtio driver paths" required:"true"`
	ImageIndex        int      `help:"Edition in install.wim by index (1-based; see dism /Get-WimInfo). Exclusive with --image-name"`
	ImageName         string   `help:"Edition in install.wim by name, e.g. 'Windows Server 2019 SERVERSTANDARD'"`
	AdminPassword     string   `help:"Administrator password (prefer env CAS_WIN_ADMIN_PASSWORD; never printed; stored base64-encoded, PlainText=false, in the answer ISO)"`
	ProductKey        string   `help:"Product key (optional; prefer env CAS_WIN_PRODUCT_KEY; never printed)"`
	Locale            string   `help:"UI/system/user locale" default:"en-US"`
	InputLocale       string   `help:"Keyboard locale (default --locale)"`
	Timezone          string   `help:"Windows time zone id" default:"China Standard Time"`
	DriverPath        []string `help:"virtio driver folder for WinPE and the installed system (repeatable), default from a built-in per-OS map of the CAS virtio floppy (A:\\amd64\\2k19 ...); UNVERIFIED layout, check it on your CAS"`
	DiskLayout        string   `help:"Partitions of disk 0: mbr for BIOS guests, gpt for UEFI (UNVERIFIED which firmware CAS uses)" choices:"mbr|gpt" default:"mbr"`
	CastoolsInstaller string   `help:"CAStools installer on the CAStools CD run at first logon (default CAS_tools_setup.exe when --castools)"`
	CastoolsArgs      string   `help:"Silent-install switches for the CAStools installer (UNVERIFIED; without a working silent switch the wizard blocks until --install-timeout)"`
}

func (o *WindowsAnswerOptions) unattend() cas.SUnattendOptions {
	pw, key := o.AdminPassword, o.ProductKey
	if len(pw) == 0 {
		pw = os.Getenv(WIN_ADMIN_PASSWORD_ENV)
	}
	if len(key) == 0 {
		key = os.Getenv(WIN_PRODUCT_KEY_ENV)
	}
	return cas.SUnattendOptions{
		OsVersion: o.OsVersion, ImageIndex: o.ImageIndex, ImageName: o.ImageName,
		AdminPassword: cas.SecretString(pw), ProductKey: cas.SecretString(key),
		Locale: o.Locale, InputLocale: o.InputLocale, TimeZone: o.Timezone, DriverPaths: o.DriverPath,
		DiskLayout: o.DiskLayout, CastoolsInstaller: o.CastoolsInstaller, CastoolsArgs: o.CastoolsArgs,
	}
}

func (o *WindowsAnswerOptions) secretWarnings() []string {
	ret := []string{}
	if len(o.AdminPassword) > 0 || len(o.ProductKey) > 0 {
		ret = append(ret, "--admin-password/--product-key on the command line are visible in ps and shell history; prefer "+WIN_ADMIN_PASSWORD_ENV+" / "+WIN_PRODUCT_KEY_ENV)
	}
	return ret
}

type WindowsAnswerIsoOptions struct {
	WindowsAnswerOptions
	Out string `help:"Write the answer ISO here (created 0600, must not exist); copy it into the ISO pool and pass --answer-iso-file" required:"true"`
}

type WindowsTemplateOptions struct {
	WindowsAnswerOptions

	HostId       int64  `help:"CAS host id" required:"true"`
	IsoPool      string `help:"File pool holding the install ISO; the answer ISO goes there too" required:"true"`
	IsoFile      string `help:"Install ISO name in --iso-pool (.iso)" required:"true"`
	DiskPool     string `help:"File pool for the new system disk" required:"true"`
	DiskGb       int    `help:"System disk size in GB (thin)" default:"60"`
	DiskBus      string `help:"System disk bus (virtio needs the floppy drivers)" choices:"virtio|ide|scsi" default:"virtio"`
	NicModel     string `help:"NIC model" choices:"virtio|e1000|rtl8139" default:"virtio"`
	TemplateName string `help:"Name of the new template" required:"true"`
	Desc         string `help:"Template description"`
	Vswitch      string `help:"vSwitch of the temporary VM NIC" required:"true"`
	Profile      string `help:"Network profile name" default:"Default"`
	Cpu          int    `help:"vCPUs of the temporary VM" default:"2"`
	MemoryMb     int    `help:"Memory MB of the temporary VM (>= 2048)" default:"4096"`
	TempVmName   string `help:"Temporary VM name (default tplwin-<template name>)"`

	VirtioFloppy   string `help:"How the virtio drivers reach Setup: floppy = storage floppy storeFile=autoVirtio (A:), autoload = vm/add autoLoadVirtio=true, or both (UNVERIFIED which one your CAS honours)" choices:"floppy|autoload|floppy+autoload" default:"floppy"`
	BootOrder      string `help:"cdrom (bootingDevice=2): CD first; with BIOS and an empty disk no 'Press any key' prompt, later the prompt times out to the disk. disk-first (bootingDevice=1): relies on the empty disk falling through to the CD (UNVERIFIED on CAS). UEFI always prompts (unsupported)" choices:"cdrom|disk-first" default:"cdrom"`
	Castools       bool   `help:"Mount the CAStools CD and install it at first logon"`
	CastoolsIso    string `help:"CAStools ISO path on the host" default:"/vms/isos/castools.iso"`
	WaitCastools   bool   `help:"Poll vm/<id>/castoolStatus while installing; if CAStools never ran, the install counts as unfinished and the VM is kept (needs --castools)"`
	MinInstalledGb int    `help:"After the guest shut off, the system disk must have at least this many GB allocated, else the VM is kept (a crash or power-off cannot be told from a Sysprep shutdown through the API)" default:"4"`
	InstallTimeout string `help:"Give up waiting for Setup+Sysprep to shut the VM off after this long (the VM is then kept)" default:"2h"`
	PollInterval   string `help:"Status poll interval" default:"30s"`
	Refresh        bool   `help:"GET storage/refresh on the ISO pool first (after copying files in by hand)"`
	DryRun         bool   `help:"Read only; print the plan and the redacted autounattend.xml"`

	AnswerIsoFile string `help:"Answer ISO already placed in --iso-pool (from image-import-windows-answer-iso); otherwise give --answer-upload"`
	AnswerUpload  string `help:"Upload the generated answer ISO with ssh or http (same --ssh-* / --serve-* settings)" choices:"ssh|http"`

	// install ISO transport (as image-import template)
	UploadMode               string `help:"How the install ISO gets into the pool: pool-file (default), ssh (--ssh-upload), http (--http-upload; nova/download imageType raw, UNVERIFIED for ISOs)" choices:"pool-file|ssh|http"`
	SshUpload                string `help:"Local install ISO to copy over SSH (needs -tags cas_ssh)"`
	HttpUpload               string `help:"Local install ISO CAS pulls from a temporary stub served by cmx"`
	SshAddr                  string `help:"SSH host:port (default <host ip>:22)"`
	SshUser                  string `help:"SSH user" default:"root"`
	SshPassword              string `help:"SSH password (prefer env CAS_SSH_PASSWORD)"`
	SshKeyFile               string `help:"SSH private key file (PEM, unencrypted)"`
	SshKnownHosts            string `help:"known_hosts file"`
	SshHostKeySha256         string `help:"Pinned host key fingerprint SHA256:..."`
	SshInsecureIgnoreHostKey bool   `help:"Skip host key verification (not recommended)"`
	ServeAddr                string `help:"Mode http: host:port the stub listens on"`
	ServeAdvertise           string `help:"Mode http: host:port CAS fetches from (default --serve-addr)"`
	HttpSkipChecksum         bool   `help:"Mode http: do not send the md5"`
	ServeAnyAddr             bool   `help:"Mode http: allow a wildcard --serve-addr"`
}

// transport returns image-import template options carrying only the
// transport settings, for the given local file and mode.
func (o *WindowsTemplateOptions) transport(mode, local string) *ImageImportTemplateOptions {
	t := &ImageImportTemplateOptions{UploadMode: mode}
	switch mode {
	case "ssh":
		t.SshUpload, t.SshAddr, t.SshUser, t.SshPassword, t.SshKeyFile = local, o.SshAddr, o.SshUser, o.SshPassword, o.SshKeyFile
		t.SshKnownHosts, t.SshHostKeySha256, t.SshInsecureIgnoreHostKey = o.SshKnownHosts, o.SshHostKeySha256, o.SshInsecureIgnoreHostKey
	case "http":
		t.HttpUpload, t.ServeAddr, t.ServeAdvertise, t.HttpSkipChecksum, t.ServeAnyAddr = local, o.ServeAddr, o.ServeAdvertise, o.HttpSkipChecksum, o.ServeAnyAddr
	}
	return t
}

// stager builds the transport for local; sensitive marks the answer ISO
// (http: the CAS staging copy is deleted after the pool copy, B-W3).
func (o *WindowsTemplateOptions) stager(mode, local string, sensitive bool) (cas.ImportSourceStager, error) {
	t := o.transport(mode, local)
	if _, err := t.uploadMode(); err != nil {
		return nil, err
	}
	switch mode {
	case "http":
		return cas.NewHTTPStager(cas.SHTTPUploadOptions{
			LocalPath: local, ServeAddr: o.ServeAddr, AdvertiseAddr: o.ServeAdvertise, ImageType: "raw",
			SkipChecksum: o.HttpSkipChecksum, AllowAnyAddr: o.ServeAnyAddr, Sensitive: sensitive,
		}), nil
	case "ssh":
		return t.sshStager()
	}
	return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "upload mode %q", mode)
}

func (o *WindowsTemplateOptions) importOptions() (cas.SWindowsImportOptions, error) {
	ret := cas.SWindowsImportOptions{
		HostId: o.HostId, IsoPool: o.IsoPool, IsoFile: o.IsoFile, DiskPool: o.DiskPool, DiskSizeGB: o.DiskGb,
		DiskBus: o.DiskBus, NicModel: o.NicModel, TemplateName: o.TemplateName, Description: o.Desc,
		OsVersion: o.OsVersion, CpuCores: o.Cpu, MemoryMB: o.MemoryMb, VSwitchName: o.Vswitch, ProfileName: o.Profile,
		TempVMName: o.TempVmName, Unattend: o.unattend(), AnswerIsoFile: o.AnswerIsoFile,
		VirtioFloppy: o.VirtioFloppy, Castools: o.Castools, CastoolsISO: o.CastoolsIso, BootOrder: o.BootOrder,
		WaitCastools: o.WaitCastools, MinInstalledGB: o.MinInstalledGb, Refresh: o.Refresh, DryRun: o.DryRun,
	}
	var err error
	if ret.InstallTimeout, err = time.ParseDuration(o.InstallTimeout); err != nil || ret.InstallTimeout <= 0 {
		return ret, errors.Wrapf(cloudprovider.ErrInputParameter, "--install-timeout %q", o.InstallTimeout)
	}
	if ret.PollInterval, err = time.ParseDuration(o.PollInterval); err != nil || ret.PollInterval < time.Second {
		return ret, errors.Wrapf(cloudprovider.ErrInputParameter, "--poll-interval %q (>= 1s)", o.PollInterval)
	}
	mode := o.UploadMode
	if len(mode) == 0 {
		switch {
		case len(o.SshUpload) > 0 && len(o.HttpUpload) > 0:
			return ret, errors.Wrap(cloudprovider.ErrInputParameter, "--ssh-upload and --http-upload are exclusive")
		case len(o.SshUpload) > 0:
			mode = "ssh"
		case len(o.HttpUpload) > 0:
			mode = "http"
		default:
			mode = "pool-file"
		}
	}
	sshSet := len(o.SshAddr) > 0 || len(o.SshPassword) > 0 || len(o.SshKeyFile) > 0 || len(o.SshKnownHosts) > 0 || len(o.SshHostKeySha256) > 0 || o.SshInsecureIgnoreHostKey
	httpSet := len(o.ServeAddr) > 0 || len(o.ServeAdvertise) > 0 || o.HttpSkipChecksum || o.ServeAnyAddr
	if (sshSet && mode != "ssh" && o.AnswerUpload != "ssh") || (httpSet && mode != "http" && o.AnswerUpload != "http") {
		return ret, errors.Wrap(cloudprovider.ErrInputParameter, "--ssh-* / --serve-* options need --upload-mode or --answer-upload ssh / http")
	}
	if (mode != "ssh" && len(o.SshUpload) > 0) || (mode != "http" && len(o.HttpUpload) > 0) {
		return ret, errors.Wrap(cloudprovider.ErrInputParameter, "--ssh-upload / --http-upload need the matching --upload-mode")
	}
	switch mode {
	case "pool-file":
	case "ssh", "http":
		local := o.SshUpload
		if mode == "http" {
			local = o.HttpUpload
		}
		if len(local) == 0 {
			return ret, errors.Wrapf(cloudprovider.ErrMissingParameter, "--upload-mode %s needs --%s-upload <local ISO>", mode, mode)
		}
		if err := cas.CheckISOFile(local); err != nil {
			return ret, err
		}
		if ret.IsoStager, err = o.stager(mode, local, false); err != nil {
			return ret, err
		}
	default:
		return ret, errors.Wrapf(cloudprovider.ErrInputParameter, "upload mode %q", mode)
	}
	if len(o.AnswerUpload) > 0 {
		am := o.AnswerUpload
		// validate the transport now (placeholder path; the real file is
		// the temp ISO written during the run)
		if _, err := o.stager(am, "answer.iso", true); err != nil {
			return ret, err
		}
		ret.AnswerMode = am
		ret.AnswerStager = func(local string) (cas.ImportSourceStager, error) { return o.stager(am, local, true) }
	}
	return ret, nil
}

func provider(cli ICloudProvider, cmd string) (*casprovider.SCasProvider, error) {
	if cli == nil || cli.GetProvider() == nil {
		return nil, errors.Wrapf(cloudprovider.ErrMissingParameter, "%s needs CAS credentials", cmd)
	}
	p, ok := cli.GetProvider().(*casprovider.SCasProvider)
	if !ok {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "%s requires the CAS provider", cmd)
	}
	return p, nil
}

const windowsTemplateHelp = "Install Windows from an ISO unattended (autounattend.xml on a generated answer ISO, virtio drivers from the CAS floppy) in a temporary VM, Sysprep it and clone it to a template; --dry-run prints the plan with the answer file redacted. Install media are always disconnected and the boot order set to disk-first before cloning; any doubt (disk-wipe guard, media, boot order, install evidence) keeps the temporary VM. UNVERIFIED on live CAS: the virtio floppy layout and driver paths, two CDs plus a floppy in vm/add, the boot-order behaviour and the bootDev body, BIOS vs UEFI, the vm/modify disconnect bodies, the CAStools silent switch, the answer-file cleanup before Sysprep"

// writeAnswerISO writes data to out, which must not exist, as 0600.
func writeAnswerISO(out string, data []byte) error {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(out)
		return err
	}
	return f.Close()
}

func init() {
	cmd := NewCommand("image-import")
	NewCO[WindowsTemplateOptions](cmd).RawRun("windows-template", windowsTemplateHelp, func(cli ICloudProvider, args *WindowsTemplateOptions) error {
		opts, err := args.importOptions()
		if err != nil {
			return err
		}
		p, err := provider(cli, "image-import-windows-template")
		if err != nil {
			return err
		}
		warns := args.secretWarnings()
		if len(args.SshUpload) > 0 || args.AnswerUpload == "ssh" {
			warns = append(warns, args.transport("ssh", "x").sshPreflightWarnings()...)
		}
		for _, w := range warns {
			fmt.Fprintln(os.Stderr, "WARNING: "+w)
		}
		ctx, stop := interruptContext()
		defer stop()
		opts.Context = ctx
		res, err := p.GetClient().ImportWindowsISOAsTemplate(opts)
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
		return err
	})
	NewCO[WindowsAnswerIsoOptions](cmd).RawRun("windows-answer-iso", "Write the answer ISO (autounattend.xml, Administrator password encoded) to a local file for --answer-iso-file. Sends no CAS request beyond the endpoint probe every cmx command makes. The file holds secrets: keep it private and delete it after use", func(cli ICloudProvider, args *WindowsAnswerIsoOptions) error {
		n, err := runWindowsAnswerISO(args, time.Now())
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%d bytes); it contains the encoded Administrator password\n", args.Out, n)
		return nil
	})
}

func runWindowsAnswerISO(args *WindowsAnswerIsoOptions, now time.Time) (int, error) {
	u := args.unattend()
	data, err := u.BuildAnswerISO(now)
	if err != nil {
		return 0, err
	}
	for _, w := range args.secretWarnings() {
		fmt.Fprintln(os.Stderr, "WARNING: "+w)
	}
	if err := writeAnswerISO(args.Out, data); err != nil {
		return 0, err
	}
	return len(data), nil
}
