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

// T2: a Windows install ISO becomes a CAS VM template, unattended.
//
//  1. (optional) stage the install ISO into an ISO pool with the same
//     ImportSourceStager as the qcow2 import (pool-file default, http,
//     ssh under -tags cas_ssh); non-.iso names and qcow2 volumes are
//     refused.
//  2. render autounattend.xml (unattend.go), pack it into an ISO 9660 +
//     Joliet image in memory (isowriter.go) and upload it into the same
//     pool through a stager (or use one placed there by hand).
//  3. POST vm/add: a NEW empty system disk in the disk pool (storeFile is
//     a bare file name, api.yaml A:64828 "如果只有文件名，会自动生成存储卷"),
//     cdrom = install ISO, cdrom = answer ISO, optional cdrom = CAStools,
//     floppy storeFile=autoVirtio, system=0, osVersion, bootingDevice per
//     BootOrder, autoBooting=0.
//  4. check the VM has exactly one disk and it is that new file (the
//     answer file wipes disk 0), then PUT vm/start.
//  5. poll vm/vmList until the guest shut itself off (Sysprep /shutdown),
//     having been seen running; optional GET vm/{id}/castoolStatus.
//  6. disconnect the install, answer and CAStools CDs and the floppy
//     (PUT vm/modify cdrom/floppy operation=disconnect) and check it.
//  7. addVmTemplate opType=1 (clone; opType 2 is never sent), the B1
//     backing-file check, recycle the temp VM (delBase0=false,
//     isWipeVolume=false), delete the answer ISO uploaded by this run.
//
// The "Press any key to boot from CD or DVD" prompt (user concern 2):
// with BIOS media it is shown by the CD boot sector only when the hard
// disk already holds a bootable system. BootOrder "cdrom" (default,
// bootingDevice=2, documented) boots the CD with no prompt while the new
// disk is empty; after Setup's first reboot the disk is bootable, the
// prompt appears, nobody presses a key, it times out and the disk boots,
// which is what Setup wants. "disk-first" (bootingDevice=1) relies on the
// firmware falling through from the empty disk to the CD and avoids the
// prompt completely, but whether CAS puts the CD in the boot list when
// bootingDevice=1 is UNVERIFIED. UEFI media always prompt (cdboot.efi);
// UEFI guests are not supported here (DiskLayout gpt only changes the
// partitions). The "Load driver" step needs no key either: the virtio
// drivers are loaded from A:\ by the answer file (unattend.go).
//
// Safety: read-only accounts and dry-run send no write and start no
// stub; dry-run returns the plan with autounattend.xml redacted. Before
// the guest has started, failures follow the qcow2 importer's rules
// (recycle the temp VM only when it is certainly ours, keep it on
// ambiguity). From the start on, the VM is never recycled on failure: it
// is kept, named and left for inspection, because it holds an install.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	DEFAULT_WIN_DISK_GB         = 60
	DEFAULT_WIN_CPU             = 2
	DEFAULT_WIN_MEMORY_MB       = 4096
	DEFAULT_WIN_INSTALL_TIMEOUT = 2 * time.Hour
	DEFAULT_WIN_POLL_INTERVAL   = 30 * time.Second
	DEFAULT_CASTOOLS_ISO        = "/vms/isos/castools.iso"
	winTempVMPrefix             = "tplwin-"
	winAnswerSuffix             = "-answer.iso"

	WIN_BOOT_CDROM      = "cdrom"
	WIN_BOOT_DISK_FIRST = "disk-first"

	// virtio floppy modes (vm/add)
	WIN_VIRTIO_FLOPPY = "floppy"   // storage[] floppy storeFile=autoVirtio
	WIN_VIRTIO_AUTO   = "autoload" // top-level autoLoadVirtio=true
	WIN_VIRTIO_BOTH   = "floppy+autoload"

	// winStoppedGrace: a VM that is shut off this many polls in a row
	// without ever having been seen running did not install anything.
	winStoppedGrace = 3
	// winListErrors: consecutive vm/vmList failures tolerated while waiting.
	winListErrors = 5
)

// SWindowsImportOptions describes one Windows ISO -> template run.
type SWindowsImportOptions struct {
	HostId int64
	// IsoPool holds the install ISO and receives the answer ISO.
	IsoPool string
	// IsoFile is the install ISO name in IsoPool (must end in .iso).
	IsoFile string
	// IsoStager, when set, uploads the install ISO into IsoPool first.
	IsoStager ImportSourceStager
	// DiskPool receives the new system disk (file pool).
	DiskPool   string
	DiskSizeGB int
	// DiskBus virtio (default), ide or scsi; NicModel virtio (default),
	// e1000 or rtl8139. virtio needs the floppy drivers.
	DiskBus  string
	NicModel string

	TemplateName string
	Description  string
	OsVersion    string
	CpuCores     int
	MemoryMB     int
	VSwitchName  string
	ProfileName  string
	// TempVMName defaults to "tplwin-<template name>".
	TempVMName string

	Unattend SUnattendOptions
	// AnswerStager makes a stager for the generated answer ISO (a local
	// temp file); AnswerMode names it in the plan. Without it,
	// AnswerIsoFile must name an answer ISO already in IsoPool.
	AnswerStager  func(localPath string) (ImportSourceStager, error)
	AnswerMode    string
	AnswerIsoFile string

	// VirtioFloppy: WIN_VIRTIO_FLOPPY (default), WIN_VIRTIO_AUTO,
	// WIN_VIRTIO_BOTH.
	VirtioFloppy string
	// Castools mounts CastoolsISO (default /vms/isos/castools.iso) as a
	// third CD; Unattend.CastoolsInstaller then installs from it.
	Castools    bool
	CastoolsISO string
	// BootOrder WIN_BOOT_CDROM (default) or WIN_BOOT_DISK_FIRST.
	BootOrder string

	InstallTimeout time.Duration
	PollInterval   time.Duration
	// WaitCastools also polls vm/{id}/castoolStatus while installing and
	// warns when CAStools was never seen running.
	WaitCastools bool
	// KeepMedia skips disconnecting the CDs/floppy (the template then
	// references them, including the answer ISO; not recommended).
	KeepMedia bool
	Refresh   bool
	DryRun    bool
	Context   context.Context

	now func() time.Time
}

type SWindowsImportResult struct {
	DryRun     bool          `json:"dry_run"`
	InstallISO string        `json:"install_iso"`
	AnswerISO  string        `json:"answer_iso"`
	SystemDisk string        `json:"system_disk"`
	TempVM     string        `json:"temp_vm"`
	TempVMUuid string        `json:"temp_vm_uuid,omitempty"`
	AnswerXML  string        `json:"answer_xml_redacted,omitempty"`
	Steps      []SImportStep `json:"steps"`
	Template   *SVmTemplate  `json:"template,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
}

func (r *SWindowsImportResult) add(step, method, p, detail string, write, done bool) {
	r.Steps = append(r.Steps, SImportStep{Step: step, Method: method, Path: p, Detail: detail, Write: write, Done: done})
}

func (o *SWindowsImportOptions) normalize() error {
	if o.HostId <= 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "host id")
	}
	if len(o.IsoPool) == 0 || len(o.IsoFile) == 0 || len(o.DiskPool) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "iso pool, iso file and disk pool")
	}
	if !strings.HasSuffix(strings.ToLower(o.IsoFile), ".iso") {
		return errors.Wrapf(cloudprovider.ErrNotSupported, "install media %q is not an .iso", o.IsoFile)
	}
	o.TemplateName = strings.TrimSpace(o.TemplateName)
	if len(o.TemplateName) == 0 || len(o.VSwitchName) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "template name and vswitch")
	}
	o.Unattend.OsVersion = o.OsVersion
	if err := o.Unattend.normalize(); err != nil {
		return err
	}
	if o.Castools && len(o.Unattend.CastoolsInstaller) == 0 {
		o.Unattend.CastoolsInstaller = DEFAULT_CASTOOLS_INSTALLER
	}
	if !o.Castools && len(o.Unattend.CastoolsInstaller) > 0 {
		return errors.Wrap(cloudprovider.ErrInputParameter, "a CAStools installer needs the CAStools CD (castools)")
	}
	if o.Castools && len(o.CastoolsISO) == 0 {
		o.CastoolsISO = DEFAULT_CASTOOLS_ISO
	}
	if o.Castools && !path.IsAbs(o.CastoolsISO) {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "CAStools ISO %q must be an absolute path", o.CastoolsISO)
	}
	if o.DiskSizeGB <= 0 {
		o.DiskSizeGB = DEFAULT_WIN_DISK_GB
	}
	if o.DiskSizeGB < 20 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "system disk %d GB is too small for Windows (min 20)", o.DiskSizeGB)
	}
	switch o.DiskBus {
	case "":
		o.DiskBus = "virtio"
	case "virtio", "ide", "scsi":
	default:
		return errors.Wrapf(cloudprovider.ErrInputParameter, "disk bus %q", o.DiskBus)
	}
	switch o.NicModel {
	case "":
		o.NicModel = "virtio"
	case "virtio", "e1000", "rtl8139":
	default:
		return errors.Wrapf(cloudprovider.ErrInputParameter, "nic model %q", o.NicModel)
	}
	switch o.VirtioFloppy {
	case "":
		o.VirtioFloppy = WIN_VIRTIO_FLOPPY
	case WIN_VIRTIO_FLOPPY, WIN_VIRTIO_AUTO, WIN_VIRTIO_BOTH:
	default:
		return errors.Wrapf(cloudprovider.ErrInputParameter, "virtio floppy mode %q", o.VirtioFloppy)
	}
	switch o.BootOrder {
	case "":
		o.BootOrder = WIN_BOOT_CDROM
	case WIN_BOOT_CDROM, WIN_BOOT_DISK_FIRST:
	default:
		return errors.Wrapf(cloudprovider.ErrInputParameter, "boot order %q (cdrom or disk-first)", o.BootOrder)
	}
	if len(o.ProfileName) == 0 {
		o.ProfileName = DEFAULT_IMPORT_PROFILE
	}
	if o.CpuCores <= 0 {
		o.CpuCores = DEFAULT_WIN_CPU
	}
	if o.MemoryMB <= 0 {
		o.MemoryMB = DEFAULT_WIN_MEMORY_MB
	}
	if o.MemoryMB < 2048 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "memory %d MB: Windows Setup needs at least 2048", o.MemoryMB)
	}
	if o.InstallTimeout <= 0 {
		o.InstallTimeout = DEFAULT_WIN_INSTALL_TIMEOUT
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DEFAULT_WIN_POLL_INTERVAL
	}
	if len(o.TempVMName) == 0 {
		o.TempVMName = winTempVMPrefix + sanitizeImportName(o.TemplateName)
	}
	if !importNameRe.MatchString(o.TempVMName) || len(o.TempVMName)+len(winAnswerSuffix) > 64 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "temporary VM name %q (letters, digits, . _ -; derived file names must stay within 64 chars)", o.TempVMName)
	}
	if o.TempVMName == o.TemplateName {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "temporary VM name must differ from the template name")
	}
	if o.AnswerStager == nil && len(o.AnswerIsoFile) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "the answer ISO needs an upload mode (http or ssh), or build it with cmx image-import-windows-answer-iso, copy it into the ISO pool and pass its name")
	}
	if o.AnswerStager != nil && len(o.AnswerIsoFile) > 0 {
		return errors.Wrap(cloudprovider.ErrInputParameter, "give either an answer upload mode or a pre-placed answer ISO, not both")
	}
	if len(o.AnswerIsoFile) > 0 && !strings.HasSuffix(strings.ToLower(o.AnswerIsoFile), ".iso") {
		return errors.Wrapf(cloudprovider.ErrNotSupported, "answer media %q is not an .iso", o.AnswerIsoFile)
	}
	if o.now == nil {
		o.now = time.Now
	}
	return nil
}

// checkInstallISO refuses media CAS lists as a disk image format.
func checkInstallISO(vol *SStorageVolume, p string) ([]string, error) {
	switch f := strings.ToLower(vol.Format); f {
	case "qcow2", "vmdk", "vhd", "vhdx":
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "%s has format %q; an ISO is needed", p, vol.Format)
	case "iso", "raw", "":
		return nil, nil
	default:
		return []string{fmt.Sprintf("%s is listed with format %q (expected iso or raw); continuing", p, vol.Format)}, nil
	}
}

// GetCastoolStatus is GET vm/{id}/castoolStatus (api.yaml A:23690,
// "running" or "not running"; a bare string, JSON-quoted or not).
func (cli *SCasClient) GetCastoolStatus(vmId int64) (string, error) {
	data, ce, err := cli.rawRequest(httputils.GET, fmt.Sprintf("vm/%d/castoolStatus", vmId), nil, nil, "")
	if err != nil {
		return "", err
	}
	if ce != nil {
		return "", ce.mapped()
	}
	return strings.Trim(strings.TrimSpace(string(data)), `"`), nil
}

// SVmModifyMedia is RsDomainModifyBasic with only id plus cdrom
// (VMModifyCdromConfig, A:58562) or floppy (VMModifyFloppyConfig,
// A:58555); PUT vm/modify returns RsTaskMsg.
type SVmModifyMedia struct {
	Id     int64                `json:"id"`
	Cdrom  *SVmModifyCdromCfg   `json:"cdrom,omitempty"`
	Floppy *SVmModifyFloppyConf `json:"floppy,omitempty"`
}

type SVmModifyCdromCfg struct {
	Device    string `json:"device"`
	Operation string `json:"operation"`
	Type      string `json:"type"`
	Path      string `json:"path,omitempty"`
}

type SVmModifyFloppyConf struct {
	DeviceName string `json:"deviceName"`
	Operation  string `json:"operation"`
}

// SWindowsDomainAdd is the DomainAddEntityRsDomain subset for the
// Windows install VM (separate from SDomainAdd: other storage items and
// autoLoadVirtio).
type SWindowsDomainAdd struct {
	Name                 string                `json:"name"`
	Title                string                `json:"title,omitempty"`
	Description          string                `json:"description,omitempty"`
	HostId               int64                 `json:"hostId"`
	AutoMigrate          int                   `json:"autoMigrate"`
	Memory               int                   `json:"memory"`
	MemoryInit           float64               `json:"memoryInit"`
	MemoryUnit           string                `json:"memoryUnit"`
	MemoryBacking        int                   `json:"memoryBacking"`
	MemoryPriority       int                   `json:"memoryPriority"`
	AutoMem              int                   `json:"autoMem"`
	Hugepage             bool                  `json:"hugepage"`
	Cpu                  int                   `json:"cpu"`
	CpuSockets           int                   `json:"cpuSockets"`
	CpuCores             int                   `json:"cpuCores"`
	MaxCpuSocket         int                   `json:"maxCpuSocket"`
	FormatEnable         int                   `json:"formatEnable"`
	CpuMode              string                `json:"cpuMode"`
	CpuShares            int                   `json:"cpuShares"`
	Blkiotune            int                   `json:"blkiotune"`
	OsBit                string                `json:"osBit"`
	System               int                   `json:"system"`
	OsVersion            string                `json:"osVersion"`
	BootingDevice        int                   `json:"bootingDevice"`
	AutoBooting          int                   `json:"autoBooting"`
	AutoLoadVirtio       bool                  `json:"autoLoadVirtio"`
	EnableReduceCPU      bool                  `json:"enableReduceCPU"`
	DomainCpuGlobalQuota bool                  `json:"domainCpuGlobalQuota"`
	Network              []SDomainAddNic       `json:"network"`
	Storage              []SWindowsDomainMedia `json:"storage"`
}

type SWindowsDomainMedia struct {
	Device    string `json:"device"`
	StoreFile string `json:"storeFile"`
	Capacity  int64  `json:"capacity,omitempty"`
	CacheType string `json:"cacheType"`
	TargetBus string `json:"targetBus"`
	Type      string `json:"type,omitempty"`
	DriveType string `json:"driveType,omitempty"`
	Mode      int    `json:"mode,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	PoolName  string `json:"poolName,omitempty"`
	DiskMode  string `json:"diskMode,omitempty"`
}

const (
	// WIN_VIRTIO_STOREFILE is the api.yaml storeFile value for the
	// OS-dependent virtio floppy.
	WIN_VIRTIO_STOREFILE = "autoVirtio"
	// VOLUME_MODE_THIN_ADD is vm/add storage.mode 2 ("精简").
	VOLUME_MODE_THIN_ADD = 2
)

func buildWindowsDomainAdd(o *SWindowsImportOptions, host *SHost, diskPool *SStoragePool, diskName string, media []string, vs *SVSwitch, profile *SPortProfile) *SWindowsDomainAdd {
	mtu := vs.Mtu
	if mtu <= 0 {
		mtu = 1500
	}
	boot := 2 // cdrom
	if o.BootOrder == WIN_BOOT_DISK_FIRST {
		boot = 1
	}
	osBit := "x86_64"
	if windowsArch(o.OsVersion) == "x86" {
		osBit = "i686"
	}
	st := []SWindowsDomainMedia{{
		Device: "disk", StoreFile: diskName, Capacity: int64(o.DiskSizeGB) * 1024, CacheType: "directsync",
		TargetBus: o.DiskBus, Type: "file", DriveType: "qcow2", Mode: VOLUME_MODE_THIN_ADD,
		Priority: STORAGE_PRIORITY_FILE_ONLY, PoolName: diskPool.Name, DiskMode: "subordinate",
	}}
	for _, m := range media {
		st = append(st, SWindowsDomainMedia{Device: "cdrom", StoreFile: m, CacheType: "directsync", TargetBus: "ide"})
	}
	if o.VirtioFloppy != WIN_VIRTIO_AUTO {
		// UNVERIFIED: the floppy bus name; "fdc" is the bus RsDomain lists.
		st = append(st, SWindowsDomainMedia{Device: "floppy", StoreFile: WIN_VIRTIO_STOREFILE, CacheType: "directsync", TargetBus: "fdc"})
	}
	nic := SDomainAddNic{VsId: vs.Id, VsName: vs.Name, Mtu: mtu, ProfileId: profile.Id, DeviceModel: o.NicModel}
	return &SWindowsDomainAdd{
		Name: o.TempVMName, Title: o.TempVMName, Description: winTempVMDescription(o), HostId: host.Id,
		Memory: o.MemoryMB, MemoryInit: float64(o.MemoryMB), MemoryUnit: "MB",
		Cpu: o.CpuCores, CpuSockets: 1, CpuCores: o.CpuCores, MaxCpuSocket: 1, FormatEnable: 1,
		CpuMode: "custom", CpuShares: 512, Blkiotune: 300, OsBit: osBit,
		System: 0, OsVersion: o.OsVersion, BootingDevice: boot, AutoBooting: 0,
		AutoLoadVirtio: o.VirtioFloppy != WIN_VIRTIO_FLOPPY,
		Network:        []SDomainAddNic{nic}, Storage: st,
	}
}

func winTempVMDescription(o *SWindowsImportOptions) string {
	return "temporary Windows install VM for template " + o.TemplateName + "; safe to delete"
}

// stageIntoPool runs one stager into pool as fileName: refresh, check the
// name is free, Plan, Stage (not in dry-run), refresh. Same rules as the
// qcow2 importer.
func (c *SCasClient) stageIntoPool(ctx context.Context, res *SWindowsImportResult, host *SHost, pool *SStoragePool, fileName, label string, st ImportSourceStager, dryRun bool) error {
	refreshPath := fmt.Sprintf("storage/refresh?id=%d&poolName=%s", host.Id, pool.Name)
	if dryRun {
		res.add("refresh-pool-before-"+label, "GET", refreshPath, "rescan so the name-free check sees unlisted files (state-changing; skipped in dry-run)", true, false)
	} else {
		if err := ctx.Err(); err != nil {
			return errors.Wrapf(err, "interrupted before uploading the %s", label)
		}
		if err := c.RefreshStoragePool(host.Id, pool.Name); err != nil {
			return errors.Wrapf(err, "refresh pool %s", pool.Name)
		}
		res.add("refresh-pool-before-"+label, "GET", refreshPath, "", true, true)
	}
	if _, err := c.GetStorageVolume(host.Id, pool.Name, fileName); err == nil {
		return errors.Wrapf(cloudprovider.ErrDuplicateId, "%s already exists in pool %s; refusing to upload over it", fileName, pool.Name)
	} else if errors.Cause(err) != cloudprovider.ErrNotFound {
		return errors.Wrapf(err, "check %s", fileName)
	}
	target := SImportTarget{HostId: host.Id, HostIp: host.Ip, HostPoolId: host.HostPoolId, PoolName: pool.Name, PoolPath: pool.Path, FileName: fileName, cli: c, ctx: ctx}
	plan, err := st.Plan(target)
	if err != nil {
		return err
	}
	res.Warnings = append(res.Warnings, plan.Warnings...)
	if dryRun {
		res.add(plan.Step+"-"+label, plan.Method, target.Path(), plan.Detail, true, false)
		return nil
	}
	warns, err := st.Stage(target)
	res.Warnings = append(res.Warnings, warns...)
	if err != nil {
		return errors.Wrapf(err, "%s of the %s", plan.Step, label)
	}
	res.add(plan.Step+"-"+label, plan.Method, target.Path(), plan.Detail, true, true)
	if err := c.RefreshStoragePool(host.Id, pool.Name); err != nil {
		return errors.Wrapf(err, "refresh pool %s after the %s", pool.Name, label)
	}
	res.add("refresh-pool-after-"+label, "GET", refreshPath, "", true, true)
	return nil
}

// ImportWindowsISOAsTemplate runs the pipeline at the top of this file.
func (cli *SCasClient) ImportWindowsISOAsTemplate(opts SWindowsImportOptions) (*SWindowsImportResult, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	res := &SWindowsImportResult{DryRun: opts.DryRun, TempVM: opts.TempVMName}
	c := cli
	if opts.DryRun {
		c = cli.ReadOnlyClone()
	} else if err := cli.checkWritable("ImportWindowsISOAsTemplate"); err != nil {
		return nil, err
	}
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	interrupted := func(before string) error {
		if err := ctx.Err(); err != nil {
			return errors.Wrapf(err, "interrupted before %s", before)
		}
		return nil
	}

	// the answer file first: bad parameters fail before any request
	redacted, err := opts.Unattend.RenderRedacted()
	if err != nil {
		return nil, err
	}
	res.AnswerXML = string(redacted)
	var answerISO []byte
	if opts.AnswerStager != nil {
		answerISO, err = opts.Unattend.BuildAnswerISO(opts.now())
		if err != nil {
			return nil, err
		}
	}

	// ---- reads and checks ----
	host, err := c.GetHost(fmt.Sprintf("%d", opts.HostId))
	if err != nil {
		return nil, errors.Wrapf(err, "GetHost %d", opts.HostId)
	}
	isoPool, err := c.findImportPool(host.Id, opts.IsoPool)
	if err != nil {
		return nil, errors.Wrapf(err, "ISO pool")
	}
	diskPool, err := c.findImportPool(host.Id, opts.DiskPool)
	if err != nil {
		return nil, errors.Wrapf(err, "disk pool")
	}
	isoName, err := importFileName(isoPool, opts.IsoFile)
	if err != nil {
		return nil, err
	}
	isoPath := path.Join(isoPool.Path, isoName)
	answerName := opts.TempVMName + winAnswerSuffix
	if len(opts.AnswerIsoFile) > 0 {
		if answerName, err = importFileName(isoPool, opts.AnswerIsoFile); err != nil {
			return nil, err
		}
	}
	if answerName == isoName {
		return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "answer ISO and install ISO are both %s", isoName)
	}
	answerPath := path.Join(isoPool.Path, answerName)
	diskName := opts.TempVMName + importOverlaySuffix
	diskPath := path.Join(diskPool.Path, diskName)
	res.InstallISO, res.AnswerISO, res.SystemDisk = isoPath, answerPath, diskPath

	templates, err := c.GetVmTemplates()
	if err != nil {
		return nil, errors.Wrapf(err, "GetVmTemplates")
	}
	if dup := findTemplatesByName(templates, opts.TemplateName); len(dup) > 0 {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "template name %q already used by template %d", opts.TemplateName, dup[0].Id)
	}
	if vms, err := c.findVMsByName(opts.TempVMName); err != nil {
		return nil, errors.Wrapf(err, "list VMs")
	} else if len(vms) > 0 {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "a VM named %s already exists (%s; maybe kept by an earlier run): pick another --temp-vm-name", opts.TempVMName, vms[0].Uuid)
	}
	if _, err := c.GetStorageVolume(host.Id, diskPool.Name, diskName); err == nil {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "system disk %s already exists in pool %s: pick another --temp-vm-name", diskName, diskPool.Name)
	} else if errors.Cause(err) != cloudprovider.ErrNotFound {
		return nil, errors.Wrapf(err, "check system disk %s", diskName)
	}
	vswitch, err := findVSwitch(c, host.Id, opts.VSwitchName)
	if err != nil {
		return nil, err
	}
	profile, err := findProfile(c, opts.ProfileName)
	if err != nil {
		return nil, err
	}

	// ---- install ISO ----
	if opts.IsoStager != nil {
		if err := c.stageIntoPool(ctx, res, host, isoPool, isoName, "install-iso", opts.IsoStager, opts.DryRun); err != nil {
			return res, err
		}
	} else if opts.Refresh {
		if opts.DryRun {
			res.add("refresh-iso-pool", "GET", fmt.Sprintf("storage/refresh?id=%d&poolName=%s", host.Id, isoPool.Name), "state-changing; skipped in dry-run", true, false)
		} else if err := c.RefreshStoragePool(host.Id, isoPool.Name); err != nil {
			return res, errors.Wrapf(err, "refresh pool %s", isoPool.Name)
		}
	}
	if vol, err := c.GetStorageVolume(host.Id, isoPool.Name, isoName); err == nil {
		warns, err := checkInstallISO(vol, isoPath)
		if err != nil {
			return res, err
		}
		res.Warnings = append(res.Warnings, warns...)
	} else if !(opts.DryRun && opts.IsoStager != nil && errors.Cause(err) == cloudprovider.ErrNotFound) {
		return res, errors.Wrapf(err, "install ISO %s", isoPath)
	}

	// ---- answer ISO ----
	answerOurs := false
	if opts.AnswerStager != nil {
		sum := sha256.Sum256(answerISO)
		if opts.DryRun {
			res.add("refresh-pool-before-answer-iso", "GET", fmt.Sprintf("storage/refresh?id=%d&poolName=%s", host.Id, isoPool.Name), "state-changing; skipped in dry-run", true, false)
			if _, err := c.GetStorageVolume(host.Id, isoPool.Name, answerName); err == nil {
				return res, errors.Wrapf(cloudprovider.ErrDuplicateId, "%s already exists in pool %s", answerName, isoPool.Name)
			}
			res.add("upload-answer-iso", opts.AnswerMode, answerPath, fmt.Sprintf("generated ISO 9660/Joliet with autounattend.xml (%d bytes, sha256 %s); a temp file (0600) is uploaded and removed", len(answerISO), hex.EncodeToString(sum[:])), true, false)
		} else {
			if err := interrupted("the answer ISO upload"); err != nil {
				return res, err
			}
			local, err := writeSecretTemp(answerISO)
			if err != nil {
				return res, err
			}
			defer os.Remove(local)
			st, err := opts.AnswerStager(local)
			if err != nil {
				return res, err
			}
			answerOurs = true // from here on the file may exist
			if err := c.stageIntoPool(ctx, res, host, isoPool, answerName, "answer-iso", st, false); err != nil {
				if errors.Cause(err) == cloudprovider.ErrDuplicateId {
					answerOurs = false
				}
				res.Warnings = append(res.Warnings, answerWarning(answerPath))
				return res, err
			}
		}
	}
	if !opts.DryRun || opts.AnswerStager == nil {
		if _, err := c.GetStorageVolume(host.Id, isoPool.Name, answerName); err != nil {
			if answerOurs {
				res.Warnings = append(res.Warnings, answerWarning(answerPath))
			}
			return res, errors.Wrapf(err, "answer ISO %s", answerPath)
		}
	}
	if len(opts.AnswerIsoFile) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("answer ISO %s was placed by hand: its content is not checked", answerPath))
	}

	media := []string{isoPath, answerPath}
	if opts.Castools {
		media = append(media, opts.CastoolsISO)
	}
	addBody := buildWindowsDomainAdd(&opts, host, diskPool, diskName, media, vswitch, profile)
	addJSON := jsonutils.Marshal(addBody).String()
	if opts.DryRun {
		res.add("create-vm", "POST", "vm/add", addJSON, true, false)
		res.add("check-vm-disks", "GET", "vm/detail/<id>", "exactly one disk, the new "+diskPath+"; else refuse to start (the answer file wipes disk 0)", false, false)
		res.add("start-vm", "PUT", "vm/start/<id>", "", true, false)
		res.add("wait-install", "GET", "vm/vmList (+ vm/<id>/castoolStatus)", fmt.Sprintf("until the guest shuts itself off after Sysprep; poll %s, timeout %s", opts.PollInterval, opts.InstallTimeout), false, false)
		if !opts.KeepMedia {
			res.add("detach-media", "PUT", "vm/modify", "cdrom/floppy operation=disconnect for "+strings.Join(media, ", ")+" and the virtio floppy", true, false)
		}
		res.add("clone-to-template", "POST", "vmTemplate/addVmTemplate", fmt.Sprintf("id=<temp vm id>&domainName=%s&opType=%s", opts.TemplateName, TEMPLATE_OP_CLONE), true, false)
		res.add("check-template-flat", "GET", "vmTemplate/storage?vmId=<template id> + storage/volume", "fail if a template disk has a backingStore", false, false)
		res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/<temp vm uuid>", deleteTempVMDetail, true, false)
		if opts.AnswerStager != nil {
			res.add("delete-answer-iso", "DELETE", fmt.Sprintf("storage/volume/delete/%d/%s/%s", host.Id, isoPool.Name, answerName), "only once it is detached", true, false)
		}
		return res, nil
	}

	// ---- writes: the VM ----
	region := &SRegion{cli: c}
	var vm *SInstance
	vmAddSent, vmAddRejected := false, false
	keepAnswer := func() {
		if answerOurs {
			res.Warnings = append(res.Warnings, answerWarning(answerPath))
		}
	}
	keepVM := func(cause error, why string) (*SWindowsImportResult, error) {
		keepAnswer()
		name, id := opts.TempVMName, "unknown uuid"
		if vm != nil {
			id = vm.Uuid
		}
		return res, errors.NewAggregate([]error{cause, errors.Errorf("temporary VM %s (%s) was kept (%s); system disk %s, install ISO %s, answer ISO %s. Inspect it on the CAS console, then remove it by hand", name, id, why, diskPath, isoPath, answerPath)})
	}
	// failBeforeStart: the guest never ran, so recycling a VM that is
	// certainly ours is safe (qcow2 importer rules).
	failBeforeStart := func(cause error) (*SWindowsImportResult, error) {
		errs := []error{cause}
		if vm == nil && vmAddSent && !vmAddRejected {
			if isTimeout(cause) {
				return keepVM(cause, "vm/add did not finish in time and may still be running")
			}
			vms, err := c.findVMsByName(opts.TempVMName)
			switch {
			case err != nil:
				return keepVM(cause, fmt.Sprintf("VMs cannot be listed (%v)", err))
			case len(vms) == 1:
				vm = &vms[0]
				vm.region = region
			case len(vms) > 1:
				return keepVM(cause, fmt.Sprintf("%d VMs are named %s; refusing to pick one", len(vms), opts.TempVMName))
			}
		}
		if vm == nil && vmAddSent {
			// R3: even after a definite rejection, a VM with our name means
			// something was created
			if vms, err := c.findVMsByName(opts.TempVMName); err != nil || len(vms) > 0 {
				return keepVM(cause, "vm/add was rejected but VMs named like it exist or cannot be listed")
			}
		}
		if vm != nil {
			if err := vm.DeleteVM(context.Background()); err != nil {
				errs = append(errs, errors.Wrapf(err, "cleanup: delete temporary VM %s (%s); remove it by hand", vm.Name, vm.Uuid))
			} else {
				res.add("cleanup-delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, "recycle bin (never started)", true, true)
			}
			keepAnswer() // the recycled VM still references it
			return res, errors.NewAggregate(errs)
		}
		// no VM: the answer ISO is unused, unless vm/add may still be
		// creating one
		if answerOurs && (!vmAddSent || vmAddRejected) {
			if err := c.deleteAnswerISO(res, host.Id, isoPool.Name, answerName, answerPath); err != nil {
				errs = append(errs, err)
				keepAnswer()
			}
		} else {
			keepAnswer()
		}
		if _, err := c.GetStorageVolume(host.Id, diskPool.Name, diskName); err == nil {
			errs = append(errs, errors.Errorf("system disk %s exists although no VM was created; remove it by hand if unused", diskPath))
		}
		return res, errors.NewAggregate(errs)
	}

	if err := interrupted("vm/add"); err != nil {
		return failBeforeStart(err)
	}
	vmAddSent = true
	addData, addErr, err := c.rawRequest(httputils.POST, "vm/add", nil, []byte(addJSON), "application/json; charset=UTF-8")
	if err == nil && addErr != nil {
		err = addErr
	}
	if err != nil {
		vmAddRejected = isDefiniteRejection(err)
		return failBeforeStart(errors.Wrapf(err, "vm/add %s", opts.TempVMName))
	}
	addResp, err := parseJSONBody(httputils.POST, "vm/add", addData)
	if err != nil {
		return failBeforeStart(errors.Wrapf(err, "vm/add %s", opts.TempVMName))
	}
	if _, err := c.waitAsync(httputils.POST, "vm/add", addResp); err != nil {
		return failBeforeStart(errors.Wrapf(err, "vm/add %s", opts.TempVMName))
	}
	res.add("create-vm", "POST", "vm/add", addJSON, true, true)
	vms, err := c.findVMsByName(opts.TempVMName)
	if err != nil {
		return failBeforeStart(errors.Wrapf(err, "find temporary VM"))
	}
	if len(vms) != 1 {
		return failBeforeStart(errors.Errorf("expected 1 VM named %s after vm/add, found %d", opts.TempVMName, len(vms)))
	}
	vm = &vms[0]
	vm.region = region
	res.TempVMUuid = vm.Uuid
	if st := vm.GetStatus(); st != api.VM_READY {
		return keepVM(errors.Wrapf(cloudprovider.ErrInvalidStatus, "temporary VM %s has status %s right after vm/add (autoBooting=0)", vm.Name, vm.VmStatus), "it may already be running Setup")
	}

	// the answer file wipes disk 0: the VM must have exactly one disk and
	// it must be the new file in the disk pool
	detail, err := c.GetInstanceDetail(vm.Id)
	if err != nil {
		return failBeforeStart(errors.Wrapf(err, "read the disks of %s before starting it", vm.Name))
	}
	if err := checkWindowsVMDisks(detail, diskPool.Path, opts.TempVMName); err != nil {
		return failBeforeStart(err)
	}
	if vol, err := c.GetStorageVolume(host.Id, diskPool.Name, path.Base(detail.firstDisk().Path)); err != nil {
		return failBeforeStart(errors.Wrapf(err, "system disk %s is not listed in pool %s", detail.firstDisk().Path, diskPool.Name))
	} else if u := strings.TrimSpace(vol.Users); len(u) > 0 && u != vm.Name {
		return failBeforeStart(errors.Errorf("system disk %s is used by %q, not only by %s; refusing to install onto it", detail.firstDisk().Path, u, vm.Name))
	}
	res.SystemDisk = detail.firstDisk().Path
	res.add("check-vm-disks", "GET", fmt.Sprintf("vm/detail/%d", vm.Id), "one disk: "+res.SystemDisk, false, true)

	if err := interrupted("starting the VM"); err != nil {
		return failBeforeStart(err)
	}
	if err := vm.StartVM(ctx); err != nil {
		if isDefiniteRejection(err) {
			return failBeforeStart(errors.Wrapf(err, "start %s", vm.Name))
		}
		return keepVM(errors.Wrapf(err, "start %s", vm.Name), "the start may have happened")
	}
	res.add("start-vm", "PUT", fmt.Sprintf("vm/start/%d", vm.Id), "", true, true)
	log.Infof("cas windows import: %s (%s) started; waiting up to %s for Setup and Sysprep to shut it off", vm.Name, vm.Uuid, opts.InstallTimeout)

	castoolsSeen, err := c.waitWindowsInstall(ctx, vm, &opts)
	if err != nil {
		return keepVM(err, "the install did not finish as expected")
	}
	res.add("wait-install", "GET", "vm/vmList", "guest shut itself off", false, true)
	if opts.WaitCastools && !castoolsSeen {
		res.Warnings = append(res.Warnings, fmt.Sprintf("CAStools was never reported running on %s; the template may lack CAStools", vm.Name))
	}

	// ---- media ----
	if opts.KeepMedia {
		res.Warnings = append(res.Warnings, "media were not disconnected (keep-media): the template references the install ISO and the answer ISO, which holds the encoded Administrator password")
	} else if err := c.detachWindowsMedia(res, vm, media); err != nil {
		return keepVM(err, "media could not be disconnected safely")
	}

	// ---- template ----
	if err := interrupted("addVmTemplate"); err != nil {
		return keepVM(err, "interrupted after the install")
	}
	tpl, err := c.CreateVmTemplateFromVM(vm, opts.TemplateName, opts.Description)
	if err != nil {
		errs := []error{errors.Wrapf(err, "clone temporary VM to template")}
		if found, lerr := c.findTemplatesNamed(opts.TemplateName); lerr != nil {
			errs = append(errs, errors.Wrapf(lerr, "could not re-list templates; check by hand whether a template %q was left", opts.TemplateName))
		} else {
			for i := range found {
				errs = append(errs, errors.Errorf("template %d %q exists after the failure and may be incomplete: check it", found[i].Id, found[i].Name))
			}
		}
		return keepVM(errors.NewAggregate(errs), "it holds the finished install; clone it to a template by hand or rerun addVmTemplate")
	}
	res.Template = tpl
	res.add("clone-to-template", "POST", "vmTemplate/addVmTemplate", fmt.Sprintf("id=%d&domainName=%s&opType=%s", vm.Id, opts.TemplateName, TEMPLATE_OP_CLONE), true, true)
	flat, err := c.checkTemplateFlat(host.Id, tpl, res.SystemDisk, res.SystemDisk)
	if err != nil {
		return keepVM(err, "kept live (not recycled) because the template may depend on its disk")
	}
	res.add("check-template-flat", "GET", fmt.Sprintf("vmTemplate/storage?vmId=%d", tpl.Id), flat, false, true)

	errs := []error{}
	if err := vm.DeleteVM(context.Background()); err != nil {
		errs = append(errs, errors.Wrapf(err, "template %d created, but deleting temporary VM %s (%s) failed; remove it by hand", tpl.Id, vm.Name, vm.Uuid))
		keepAnswer()
	} else {
		res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, deleteTempVMDetail+"; its system disk stays with it", true, true)
		if answerOurs && !opts.KeepMedia {
			if err := c.deleteAnswerISO(res, host.Id, isoPool.Name, answerName, answerPath); err != nil {
				errs = append(errs, err)
				keepAnswer()
			}
		} else {
			keepAnswer()
		}
	}
	if opts.IsoStager != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("install ISO %s uploaded by this run was kept for reuse", isoPath))
	}
	if len(errs) > 0 {
		return res, errors.NewAggregate(errs)
	}
	return res, nil
}

func answerWarning(p string) string {
	return fmt.Sprintf("answer ISO %s contains the encoded Administrator password (and product key): delete it as soon as no VM uses it", p)
}

// writeSecretTemp writes data to a new 0600 temp file.
func writeSecretTemp(data []byte) (string, error) {
	f, err := os.CreateTemp("", "cas-answer-*.iso")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Chmod(0600); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func (d *SInstanceDetail) firstDisk() SInstanceStorage {
	for _, st := range d.disks {
		if st.Device == "disk" {
			return st
		}
	}
	return SInstanceStorage{}
}

// checkWindowsVMDisks: exactly one storage item of device "disk", in the
// disk pool directory, named after the temp VM (CAS may add a suffix to a
// bare storeFile; UNVERIFIED).
func checkWindowsVMDisks(d *SInstanceDetail, poolPath, tempVM string) error {
	disks := []SInstanceStorage{}
	for _, st := range d.disks {
		if st.Device == "disk" {
			disks = append(disks, st)
		}
	}
	if len(disks) != 1 {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s lists %d disks, want exactly the new system disk; refusing to start (the answer file wipes disk 0)", d.Name, len(disks))
	}
	p := disks[0].Path
	if path.Clean(path.Dir(p)) != path.Clean(poolPath) || !strings.HasPrefix(path.Base(p), tempVM) {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s disk is %q, not a new file of %s in %s; refusing to start (the answer file wipes disk 0)", d.Name, p, tempVM, poolPath)
	}
	return nil
}

// waitWindowsInstall polls until the guest shut itself off after having
// been seen running. Returns whether CAStools was seen running.
func (c *SCasClient) waitWindowsInstall(ctx context.Context, vm *SInstance, o *SWindowsImportOptions) (bool, error) {
	deadline := o.now().Add(o.InstallTimeout)
	sawRunning, castools := false, false
	offPolls, listErrs := 0, 0
	for {
		vms, err := c.findVMsByName(vm.Name)
		status := ""
		if err != nil {
			listErrs++
			if listErrs >= winListErrors {
				return castools, errors.Wrapf(err, "vm/vmList failed %d times in a row while waiting", listErrs)
			}
		} else {
			listErrs = 0
			found := false
			for i := range vms {
				if vms[i].Uuid == vm.Uuid {
					found, status = true, vms[i].VmStatus
					vm.VmStatus = status
				}
			}
			if !found {
				return castools, errors.Wrapf(cloudprovider.ErrNotFound, "VM %s (%s) disappeared while installing", vm.Name, vm.Uuid)
			}
		}
		switch {
		case err != nil:
		case vm.GetStatus() == api.VM_RUNNING:
			sawRunning, offPolls = true, 0
			if o.WaitCastools && !castools {
				if st, err := c.GetCastoolStatus(vm.Id); err == nil && st == "running" {
					castools = true
				}
			}
		case vm.GetStatus() == api.VM_READY && sawRunning:
			return castools, nil
		case vm.GetStatus() == api.VM_READY:
			offPolls++
			if offPolls >= winStoppedGrace {
				return castools, errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s is shut off and was never seen running; Setup did not run", vm.Name)
			}
		default:
			return castools, errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s has status %q while installing", vm.Name, status)
		}
		if !o.now().Before(deadline) {
			return castools, errors.Wrapf(cloudprovider.ErrTimeout, "VM %s did not shut off within %s (status %s); it may still be installing, or Setup may be waiting on its console", vm.Name, o.InstallTimeout, vm.VmStatus)
		}
		select {
		case <-ctx.Done():
			return castools, errors.Wrapf(ctx.Err(), "interrupted while %s was installing (it keeps running)", vm.Name)
		case <-time.After(o.PollInterval):
		}
	}
}

// detachWindowsMedia disconnects every CD whose path is one of media and
// every floppy, then re-reads the VM and fails if any of those CDs is
// still attached.
func (c *SCasClient) detachWindowsMedia(res *SWindowsImportResult, vm *SInstance, media []string) error {
	want := map[string]bool{}
	for _, m := range media {
		want[path.Clean(m)] = true
	}
	detail, err := c.GetInstanceDetail(vm.Id)
	if err != nil {
		return errors.Wrapf(err, "read media of %s", vm.Name)
	}
	for _, st := range detail.disks {
		var body *SVmModifyMedia
		switch {
		case st.Device == "cdrom" && len(st.Path) > 0 && want[path.Clean(st.Path)]:
			body = &SVmModifyMedia{Id: vm.Id, Cdrom: &SVmModifyCdromCfg{Device: st.DeviceName, Operation: "disconnect", Type: "file", Path: st.Path}}
		case st.Device == "floppy" && len(st.Path) > 0:
			body = &SVmModifyMedia{Id: vm.Id, Floppy: &SVmModifyFloppyConf{DeviceName: st.DeviceName, Operation: "disconnect"}}
		default:
			continue
		}
		if len(st.DeviceName) == 0 {
			return errors.Errorf("%s %s of %s has no device name; cannot disconnect it", st.Device, st.Path, vm.Name)
		}
		js := jsonutils.Marshal(body)
		if _, err := c.doAsync(httputils.PUT, "vm/modify", nil, js); err != nil {
			return errors.Wrapf(err, "disconnect %s %s (%s)", st.Device, st.DeviceName, st.Path)
		}
		res.add("detach-media", "PUT", "vm/modify", js.String(), true, true)
	}
	detail, err = c.GetInstanceDetail(vm.Id)
	if err != nil {
		return errors.Wrapf(err, "re-read media of %s", vm.Name)
	}
	for _, st := range detail.disks {
		if st.Device == "cdrom" && len(st.Path) > 0 && want[path.Clean(st.Path)] {
			return errors.Errorf("%s still has %s attached at %s after disconnect", vm.Name, st.Path, st.DeviceName)
		}
		if st.Device == "floppy" && len(st.Path) > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("the virtio floppy %s is still listed on %s; the template may keep it", st.Path, vm.Name))
		}
	}
	return nil
}

// deleteAnswerISO removes the answer ISO this run uploaded, once no VM
// uses it.
func (c *SCasClient) deleteAnswerISO(res *SWindowsImportResult, hostId int64, pool, name, p string) error {
	vol, err := c.GetStorageVolume(hostId, pool, name)
	if errors.Cause(err) == cloudprovider.ErrNotFound {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "re-read answer ISO %s before deleting it", p)
	}
	if vol.isAttached() {
		return errors.Errorf("answer ISO %s is still used by %s; not deleting it", p, vol.Users)
	}
	if err := c.DeleteStorageVolume(hostId, pool, name); err != nil {
		return errors.Wrapf(err, "delete answer ISO %s", p)
	}
	res.add("delete-answer-iso", "DELETE", fmt.Sprintf("storage/volume/delete/%d/%s/%s", hostId, pool, name), "contained the encoded Administrator password", true, true)
	return nil
}
