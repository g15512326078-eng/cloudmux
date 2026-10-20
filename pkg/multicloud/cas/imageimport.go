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

// Import of a Linux cloud image (qcow2) that sits in, or is first copied
// into, a CAS storage pool, turned into a CAS VM template:
//
//  1. (optional, mode b) copy a local file into the pool directory over
//     SSH, see sshupload.go; then GET storage/refresh so CAS lists it.
//  2. POST storage/volume with baseFile=<source>: a thin qcow2 overlay.
//  3. POST vm/add with the overlay as the only disk; the VM is never
//     started (autoBooting=0).
//  4. Make sure the VM is shut off, then addVmTemplate opType=1
//     (CreateVmTemplateFromVM).
//  5. Delete the temporary VM into the recycle bin (DeleteVM, type=3,
//     isWipeVolume=false).
//
// Copy strategy: an overlay (storage/volume + baseFile) rather than
// cinder/volume/copy. The overlay endpoint is the one P2 already uses for
// data disks and documents baseFile directly; cinder/volume/copy is
// documented as a Cinder snapshot helper keyed by host-pool name with an
// unclear srcName/flag contract. With an overlay the source file is only
// ever a read-only backing file: no request names it as a target, and
// the temporary VM writes (if ever booted) would land in the overlay.
// Because the VM is never booted the overlay stays empty, so the template
// content is exactly the source image. Whether addVmTemplate flattens the
// backing chain into the template is UNVERIFIED (live check): if it does
// not, the template would depend on the source file.
//
// cloud-init / configDrive is not used: the VM is never booted, so the
// template stays as clean as the source image and cloud-init runs on the
// first boot of each VM deployed from it. nova/vm/configDrive compatibility
// with stock cloud-init is unverified.

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	DEFAULT_IMPORT_OS_VERSION = "Other Linux(64-bit)"
	DEFAULT_IMPORT_PROFILE    = "Default"
	DEFAULT_IMPORT_CPU        = 1
	DEFAULT_IMPORT_MEMORY_MB  = 1024
	importTempVMPrefix        = "tplimp-"
	importOverlaySuffix       = "-sys.qcow2"
)

// importPoolTypes are the pool types whose volumes are files a qcow2
// overlay can reference (local dir, NFS, shared file system). Block pools
// (iscsi, rbd, lvm, ...) are refused. UNVERIFIED: the exact type strings
// of shared file system pools on every CAS version.
var importPoolTypes = map[string]bool{"dir": true, "netfs": true, "fs": true}

// importNameRe limits file and VM names to what CAS accepts for volumes
// and VM names, and what is safe in a remote shell command.
var importNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// SImageImportOptions describes one qcow2 -> template import.
type SImageImportOptions struct {
	HostId   int64
	PoolName string
	// File is the file name in the pool, or an absolute path whose
	// directory is the pool path.
	File         string
	TemplateName string
	Description  string
	OsVersion    string
	CpuCores     int
	MemoryMB     int
	VSwitchName  string
	ProfileName  string
	// TempVMName defaults to "tplimp-<template name>".
	TempVMName string
	// Refresh runs GET storage/refresh before looking the file up (always
	// done after an SSH upload).
	Refresh bool
	// DryRun only reads (through a read-only client) and returns the plan.
	DryRun bool
	// Upload, when set, copies a local file into the pool first (mode b).
	Upload *SSSHUploadOptions
}

// SImportStep is one planned or executed call.
type SImportStep struct {
	Step   string `json:"step"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
	Write  bool   `json:"write"`
	Done   bool   `json:"done"`
}

type SImageImportResult struct {
	DryRun     bool          `json:"dry_run"`
	SourcePath string        `json:"source_path"`
	Overlay    string        `json:"overlay"`
	TempVM     string        `json:"temp_vm"`
	Steps      []SImportStep `json:"steps"`
	Template   *SVmTemplate  `json:"template,omitempty"`
	Warnings   []string      `json:"warnings,omitempty"`
}

func (r *SImageImportResult) add(step, method, p, detail string, write, done bool) {
	r.Steps = append(r.Steps, SImportStep{Step: step, Method: method, Path: p, Detail: detail, Write: write, Done: done})
}

// SPortProfile is one element of GET profile/all (key "portProfile").
type SPortProfile struct {
	Id   int64  `json:"id"`
	Name string `json:"name"`
}

func (cli *SCasClient) GetPortProfiles() ([]SPortProfile, error) {
	ret := []SPortProfile{}
	return ret, cli.list("profile/all", nil, "portProfile", &ret)
}

// SStorageRefreshResult is the rsResult body of GET storage/refresh.
type SStorageRefreshResult struct {
	State     int    `json:"state"`
	ErrorCode int    `json:"errorCode"`
	Message   string `json:"message"`
}

// RefreshStoragePool is GET storage/refresh?id=<hostId>&poolName= (CAS
// rescans the pool directory). It is a GET but changes CAS state, so the
// importer treats it as a write: skipped in dry-run and on read-only
// accounts. state != 0 is an error.
func (cli *SCasClient) RefreshStoragePool(hostId int64, poolName string) error {
	params := url.Values{}
	params.Set("id", fmt.Sprintf("%d", hostId))
	params.Set("poolName", poolName)
	resp, err := cli.request(httputils.GET, "storage/refresh", params, nil)
	if err != nil {
		return err
	}
	resp = unwrapRoot(resp)
	if inner, err := resp.Get("rsResult"); err == nil {
		resp = inner
	}
	ret := &SStorageRefreshResult{}
	if err := resp.Unmarshal(ret); err != nil {
		return errors.Wrapf(err, "Unmarshal storage/refresh")
	}
	if ret.State != 0 {
		return errors.Errorf("storage/refresh %s on host %d: state=%d errorCode=%d %s", poolName, hostId, ret.State, ret.ErrorCode, ret.Message)
	}
	return nil
}

// SDomainAdd is the subset of DomainAddEntityRsDomain (POST vm/add) the
// importer sends. Fields api.yaml marks as required are always set.
type SDomainAdd struct {
	Name                 string             `json:"name"`
	Title                string             `json:"title,omitempty"`
	Description          string             `json:"description,omitempty"`
	HostId               int64              `json:"hostId"`
	AutoMigrate          int                `json:"autoMigrate"`
	Memory               int                `json:"memory"`
	MemoryInit           float64            `json:"memoryInit"`
	MemoryUnit           string             `json:"memoryUnit"`
	MemoryBacking        int                `json:"memoryBacking"`
	MemoryPriority       int                `json:"memoryPriority"`
	AutoMem              int                `json:"autoMem"`
	Hugepage             bool               `json:"hugepage"`
	Cpu                  int                `json:"cpu"`
	CpuSockets           int                `json:"cpuSockets"`
	CpuCores             int                `json:"cpuCores"`
	MaxCpuSocket         int                `json:"maxCpuSocket"`
	FormatEnable         int                `json:"formatEnable"`
	CpuMode              string             `json:"cpuMode"`
	CpuShares            int                `json:"cpuShares"`
	Blkiotune            int                `json:"blkiotune"`
	OsBit                string             `json:"osBit"`
	System               int                `json:"system"`
	OsVersion            string             `json:"osVersion"`
	BootingDevice        int                `json:"bootingDevice"`
	AutoBooting          int                `json:"autoBooting"`
	EnableReduceCPU      bool               `json:"enableReduceCPU"`
	DomainCpuGlobalQuota bool               `json:"domainCpuGlobalQuota"`
	Network              []SDomainAddNic    `json:"network"`
	Storage              []SDomainAddVolume `json:"storage"`
}

type SDomainAddNic struct {
	VsId                int64  `json:"vsId"`
	VsName              string `json:"vsName"`
	Mtu                 int    `json:"mtu"`
	ProfileId           int64  `json:"profileId"`
	DeviceModel         string `json:"deviceModel"`
	IsKernelAccelerated int    `json:"isKernelAccelerated"`
}

type SDomainAddVolume struct {
	Device    string `json:"device"`
	StoreFile string `json:"storeFile"`
	CacheType string `json:"cacheType"`
	TargetBus string `json:"targetBus"`
	Type      string `json:"type"`
	DriveType string `json:"driveType"`
	Priority  int    `json:"priority"`
	PoolName  string `json:"poolName"`
	DiskMode  string `json:"diskMode"`
}

// STORAGE_PRIORITY_FILE_ONLY is vm/add storage.priority 3 ("只用file").
const STORAGE_PRIORITY_FILE_ONLY = 3

func (o *SImageImportOptions) normalize() error {
	if o.HostId <= 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "host id")
	}
	if len(o.PoolName) == 0 || len(o.File) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "pool and file")
	}
	o.TemplateName = strings.TrimSpace(o.TemplateName)
	if len(o.TemplateName) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "template name")
	}
	if len(o.VSwitchName) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "vswitch name")
	}
	if len(o.OsVersion) == 0 {
		o.OsVersion = DEFAULT_IMPORT_OS_VERSION
	}
	if len(o.ProfileName) == 0 {
		o.ProfileName = DEFAULT_IMPORT_PROFILE
	}
	if o.CpuCores <= 0 {
		o.CpuCores = DEFAULT_IMPORT_CPU
	}
	if o.MemoryMB <= 0 {
		o.MemoryMB = DEFAULT_IMPORT_MEMORY_MB
	}
	if o.MemoryMB < 512 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "memory %d MB is below the CAS minimum of 512", o.MemoryMB)
	}
	if len(o.TempVMName) == 0 {
		o.TempVMName = importTempVMPrefix + sanitizeImportName(o.TemplateName)
	}
	if !importNameRe.MatchString(o.TempVMName) || len(o.TempVMName)+len(importOverlaySuffix) > 64 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "temporary VM name %q (letters, digits, . _ -; overlay name must stay within 64 chars)", o.TempVMName)
	}
	if o.TempVMName == o.TemplateName {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "temporary VM name must differ from the template name")
	}
	return nil
}

func sanitizeImportName(s string) string {
	b := strings.Builder{}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// importFileName returns the bare file name of opts.File within the pool.
// An absolute path must sit directly in the pool directory.
func importFileName(pool *SStoragePool, file string) (string, error) {
	name := file
	if strings.Contains(file, "/") {
		if !path.IsAbs(file) || path.Clean(path.Dir(file)) != path.Clean(pool.Path) {
			return "", errors.Wrapf(cloudprovider.ErrInputParameter, "file %q is not directly in pool %s (%s)", file, pool.Name, pool.Path)
		}
		name = path.Base(file)
	}
	if !importNameRe.MatchString(name) {
		return "", errors.Wrapf(cloudprovider.ErrInputParameter, "file name %q (letters, digits, . _ -)", name)
	}
	return name, nil
}

func (cli *SCasClient) findImportPool(hostId int64, name string) (*SStoragePool, error) {
	pools, err := cli.GetStoragePools(hostId)
	if err != nil {
		return nil, errors.Wrapf(err, "GetStoragePools")
	}
	shared, err := cli.GetSharedStoragePools(hostId)
	if err != nil {
		return nil, errors.Wrapf(err, "GetSharedStoragePools")
	}
	for _, p := range append(pools, shared...) {
		if p.Name == name {
			pool := p
			if !importPoolTypes[strings.ToLower(pool.Type)] {
				return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "pool %s has type %q; only file pools (dir, netfs, fs) can hold a qcow2 overlay", name, pool.Type)
			}
			if !path.IsAbs(pool.Path) {
				return nil, errors.Errorf("pool %s has no absolute path (%q)", name, pool.Path)
			}
			return &pool, nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "pool %s on host %d", name, hostId)
}

// findLiveVMsByName returns VMs in the unfiltered vm/vmList with the name.
func (cli *SCasClient) findVMsByName(name string) ([]SInstance, error) {
	vms, err := cli.listInstances(nil)
	if err != nil {
		return nil, err
	}
	ret := []SInstance{}
	for i := range vms {
		if vms[i].Name == name {
			ret = append(ret, vms[i])
		}
	}
	return ret, nil
}

func checkImportSource(vol *SStorageVolume, srcPath string) error {
	if strings.ToLower(vol.Format) != "qcow2" {
		return errors.Wrapf(cloudprovider.ErrNotSupported, "source %s has format %q; only qcow2 is supported", srcPath, vol.Format)
	}
	if vol.isAttached() {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "source %s is used by VM(s) %s; refusing to build on a disk in use", srcPath, vol.Users)
	}
	if vol.Size <= 0 {
		return errors.Errorf("source %s reports size %v MB", srcPath, vol.Size)
	}
	return nil
}

// ImportQcow2AsTemplate runs the pipeline described at the top of this
// file. Every write waits for its task. On failure it removes what it
// created (the temporary VM into the recycle bin, or the overlay when no
// VM exists yet) and never sends a request that targets the source file.
// DryRun uses a read-only client: the reads run, every write is only
// listed in the result.
func (cli *SCasClient) ImportQcow2AsTemplate(opts SImageImportOptions) (*SImageImportResult, error) {
	if err := opts.normalize(); err != nil {
		return nil, err
	}
	res := &SImageImportResult{DryRun: opts.DryRun, TempVM: opts.TempVMName}
	c := cli
	if opts.DryRun {
		c = cli.ReadOnlyClone()
	} else if err := cli.checkWritable("ImportQcow2AsTemplate"); err != nil {
		return nil, err
	}

	// ---- reads and checks, before any write ----
	host, err := c.GetHost(fmt.Sprintf("%d", opts.HostId))
	if err != nil {
		return nil, errors.Wrapf(err, "GetHost %d", opts.HostId)
	}
	pool, err := c.findImportPool(host.Id, opts.PoolName)
	if err != nil {
		return nil, err
	}
	fileName, err := importFileName(pool, opts.File)
	if err != nil {
		return nil, err
	}
	srcPath := path.Join(pool.Path, fileName)
	overlay := opts.TempVMName + importOverlaySuffix
	if overlay == fileName {
		return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "overlay name %s equals the source file name", overlay)
	}
	overlayPath := path.Join(pool.Path, overlay)
	res.SourcePath, res.Overlay = srcPath, overlayPath

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
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "a VM named %s already exists (%s)", opts.TempVMName, vms[0].Uuid)
	}
	if _, err := c.GetStorageVolume(host.Id, pool.Name, overlay); err == nil {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "overlay volume %s already exists in pool %s", overlay, pool.Name)
	} else if errors.Cause(err) != cloudprovider.ErrNotFound {
		return nil, errors.Wrapf(err, "check overlay %s", overlay)
	}
	vswitch, err := findVSwitch(c, host.Id, opts.VSwitchName)
	if err != nil {
		return nil, err
	}
	profile, err := findProfile(c, opts.ProfileName)
	if err != nil {
		return nil, err
	}

	// ---- (b) SSH upload, then refresh ----
	if opts.Upload != nil {
		if _, err := c.GetStorageVolume(host.Id, pool.Name, fileName); err == nil {
			return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "%s already exists in pool %s; refusing to upload over it", fileName, pool.Name)
		} else if errors.Cause(err) != cloudprovider.ErrNotFound {
			return nil, errors.Wrapf(err, "check %s", fileName)
		}
		up := *opts.Upload
		up.RemoteDir, up.RemoteName = pool.Path, fileName
		if len(up.Addr) == 0 {
			up.Addr = fmt.Sprintf("%s:22", host.Ip)
		}
		if err := up.validate(); err != nil {
			return nil, err
		}
		sum, size, err := localFileSHA256(up.LocalPath)
		if err != nil {
			return nil, err
		}
		detail := fmt.Sprintf("%s (%d bytes, sha256 %s) -> %s@%s:%s, no overwrite, sha256 verified on host", up.LocalPath, size, sum, up.User, up.Addr, srcPath)
		if opts.DryRun {
			res.add("ssh-upload", "SSH", srcPath, detail, true, false)
		} else {
			if _, err := uploadFileSSH(&up); err != nil {
				return res, errors.Wrapf(err, "ssh upload")
			}
			res.add("ssh-upload", "SSH", srcPath, detail, true, true)
		}
		opts.Refresh = true
	}
	refreshPath := fmt.Sprintf("storage/refresh?id=%d&poolName=%s", host.Id, url.QueryEscape(pool.Name))
	if opts.Refresh {
		if opts.DryRun {
			res.add("refresh-pool", "GET", refreshPath, "rescan pool (state-changing; skipped in dry-run)", true, false)
		} else {
			if err := c.RefreshStoragePool(host.Id, pool.Name); err != nil {
				return res, errors.Wrapf(err, "refresh pool %s", pool.Name)
			}
			res.add("refresh-pool", "GET", refreshPath, "", true, true)
		}
	}

	var capacity int64
	src, err := c.GetStorageVolume(host.Id, pool.Name, fileName)
	switch {
	case err == nil:
		if err := checkImportSource(src, srcPath); err != nil {
			return res, err
		}
		capacity = int64(math.Ceil(src.Size))
	case errors.Cause(err) == cloudprovider.ErrNotFound && opts.DryRun && opts.Refresh:
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s is not listed yet; it is checked after the refresh in a real run", srcPath))
	case errors.Cause(err) == cloudprovider.ErrNotFound && !opts.Refresh:
		return res, errors.Wrapf(err, "source %s is not listed in pool %s (if it was just copied in, retry with refresh)", srcPath, pool.Name)
	default:
		return res, errors.Wrapf(err, "source %s", srcPath)
	}

	volBody := &SStorageVolumeCreate{HostId: host.Id, SpName: pool.Name, VolName: overlay, Capacity: capacity, Format: "qcow2", Mode: VOLUME_MODE_THIN, BaseFile: srcPath}
	addBody := buildImportDomainAdd(&opts, host, pool, overlayPath, vswitch, profile)

	if opts.DryRun {
		res.add("create-overlay", "POST", "storage/volume", jsonutils.Marshal(volBody).String(), true, false)
		res.add("create-vm", "POST", "vm/add", jsonutils.Marshal(addBody).String(), true, false)
		res.add("clone-to-template", "POST", "vmTemplate/addVmTemplate", fmt.Sprintf("id=<temp vm id>&domainName=%s&opType=%s", opts.TemplateName, TEMPLATE_OP_CLONE), true, false)
		res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/<temp vm uuid>", fmt.Sprintf("type=%s&isWipeVolume=%s (recycle bin)", DELETE_VM_TYPE, DELETE_VM_WIPE_VOLUME), true, false)
		return res, nil
	}

	// ---- writes ----
	region := &SRegion{cli: c}
	var vm *SInstance
	overlayCreated := false
	fail := func(cause error) (*SImageImportResult, error) {
		errs := []error{cause}
		if vm == nil && overlayCreated {
			// vm/add may have created the VM even though it reported an error
			if vms, err := c.findVMsByName(opts.TempVMName); err == nil && len(vms) == 1 {
				vm = &vms[0]
				vm.region = region
			}
		}
		switch {
		case vm != nil:
			if err := vm.DeleteVM(context.Background()); err != nil {
				errs = append(errs, errors.Wrapf(err, "cleanup: delete temporary VM %s (%s); remove it by hand", vm.Name, vm.Uuid))
			} else {
				res.add("cleanup-delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, "recycle bin; the overlay stays with the recycled VM", true, true)
			}
		case overlayCreated:
			if overlay == fileName {
				errs = append(errs, errors.Errorf("cleanup: refusing to delete %s, it is the source", overlay))
				break
			}
			if err := c.DeleteStorageVolume(host.Id, pool.Name, overlay); err != nil {
				errs = append(errs, errors.Wrapf(err, "cleanup: delete overlay %s; remove it by hand", overlayPath))
			} else {
				res.add("cleanup-delete-overlay", "DELETE", fmt.Sprintf("storage/volume/delete/%d/%s/%s", host.Id, pool.Name, overlay), "isWipeVolume=false", true, true)
			}
		}
		return res, errors.NewAggregate(errs)
	}

	if err := c.CreateStorageVolume(volBody); err != nil {
		// the volume may exist even if the call failed
		if _, gerr := c.GetStorageVolume(host.Id, pool.Name, overlay); gerr == nil {
			overlayCreated = true
		}
		return fail(errors.Wrapf(err, "create overlay %s", overlayPath))
	}
	overlayCreated = true
	res.add("create-overlay", "POST", "storage/volume", jsonutils.Marshal(volBody).String(), true, true)
	ov, err := c.waitStorageVolume(host.Id, pool.Name, overlay)
	if err != nil {
		return fail(errors.Wrapf(err, "wait for overlay %s", overlayPath))
	}
	if path.Clean(ov.BackingStore) != srcPath {
		return fail(errors.Errorf("overlay %s reports backing store %q, want %s", overlayPath, ov.BackingStore, srcPath))
	}

	if _, err := c.doAsync(httputils.POST, "vm/add", nil, jsonutils.Marshal(addBody)); err != nil {
		return fail(errors.Wrapf(err, "vm/add %s", opts.TempVMName))
	}
	res.add("create-vm", "POST", "vm/add", jsonutils.Marshal(addBody).String(), true, true)
	vms, err := c.findVMsByName(opts.TempVMName)
	if err != nil {
		return fail(errors.Wrapf(err, "find temporary VM"))
	}
	if len(vms) != 1 {
		return fail(errors.Errorf("expected 1 VM named %s after vm/add, found %d", opts.TempVMName, len(vms)))
	}
	vm = &vms[0]
	vm.region = region

	switch st := vm.GetStatus(); st {
	case api.VM_READY:
	case api.VM_RUNNING, api.VM_SUSPEND:
		if err := vm.StopVM(context.Background(), &cloudprovider.ServerStopOptions{IsForce: true}); err != nil {
			return fail(errors.Wrapf(err, "power off temporary VM (status %s)", st))
		}
		res.add("power-off-temp-vm", "PUT", fmt.Sprintf("vm/powerOff/%d", vm.Id), "status was "+st, true, true)
		if err := c.waitVMStatus(vm, "shutOff"); err != nil {
			return fail(err)
		}
	default:
		return fail(errors.Errorf("temporary VM %s has unexpected status %q", vm.Name, vm.VmStatus))
	}

	tpl, err := c.CreateVmTemplateFromVM(vm, opts.TemplateName, opts.Description)
	if err != nil {
		return fail(errors.Wrapf(err, "clone temporary VM to template"))
	}
	res.Template = tpl
	res.add("clone-to-template", "POST", "vmTemplate/addVmTemplate", fmt.Sprintf("id=%d&domainName=%s&opType=%s", vm.Id, opts.TemplateName, TEMPLATE_OP_CLONE), true, true)
	res.Warnings = append(res.Warnings, "UNVERIFIED: whether the template disk was flattened or still uses "+srcPath+" as backing file; do not delete the source until checked")

	if err := vm.DeleteVM(context.Background()); err != nil {
		return res, errors.Wrapf(err, "template %d created, but deleting temporary VM %s (%s) failed; remove it by hand", tpl.Id, vm.Name, vm.Uuid)
	}
	res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, fmt.Sprintf("type=%s&isWipeVolume=%s (recycle bin; overlay stays with it)", DELETE_VM_TYPE, DELETE_VM_WIPE_VOLUME), true, true)
	return res, nil
}

// waitVMStatus polls vm/vmList until the VM reports vmStatus want.
func (c *SCasClient) waitVMStatus(vm *SInstance, want string) error {
	interval, timeout := c.taskPollInterval, c.taskTimeout
	if interval <= 0 {
		interval = DEFAULT_TASK_POLL_INTERVAL
	}
	if timeout <= 0 {
		timeout = DEFAULT_TASK_TIMEOUT
	}
	err := cloudprovider.Wait(interval, timeout, func() (bool, error) {
		vms, err := c.findVMsByName(vm.Name)
		if err != nil {
			return false, err
		}
		for i := range vms {
			if vms[i].Uuid == vm.Uuid {
				vm.VmStatus = vms[i].VmStatus
				return vms[i].VmStatus == want, nil
			}
		}
		return false, errors.Wrapf(cloudprovider.ErrNotFound, "vm %s", vm.Uuid)
	})
	return errors.Wrapf(err, "wait for VM %s to be %s", vm.Name, want)
}

func findVSwitch(c *SCasClient, hostId int64, name string) (*SVSwitch, error) {
	vss, err := c.GetHostVSwitches(hostId)
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostVSwitches")
	}
	for i := range vss {
		if vss[i].Name == name {
			return &vss[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "vswitch %s on host %d", name, hostId)
}

func findProfile(c *SCasClient, name string) (*SPortProfile, error) {
	profiles, err := c.GetPortProfiles()
	if err != nil {
		return nil, errors.Wrapf(err, "GetPortProfiles")
	}
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "network profile %s", name)
}

func buildImportDomainAdd(o *SImageImportOptions, host *SHost, pool *SStoragePool, overlayPath string, vs *SVSwitch, profile *SPortProfile) *SDomainAdd {
	mtu := vs.Mtu
	if mtu <= 0 {
		mtu = 1500
	}
	return &SDomainAdd{
		Name:           o.TempVMName,
		Title:          o.TempVMName,
		Description:    "temporary VM for template " + o.TemplateName + "; safe to delete",
		HostId:         host.Id,
		AutoMigrate:    0,
		Memory:         o.MemoryMB,
		MemoryInit:     float64(o.MemoryMB),
		MemoryUnit:     "MB",
		MemoryBacking:  0,
		MemoryPriority: 0,
		AutoMem:        0,
		Cpu:            o.CpuCores,
		CpuSockets:     1,
		CpuCores:       o.CpuCores,
		MaxCpuSocket:   1,
		FormatEnable:   1,
		CpuMode:        "custom",
		CpuShares:      512,
		Blkiotune:      300,
		OsBit:          "x86_64",
		System:         1, // Linux
		OsVersion:      o.OsVersion,
		BootingDevice:  1, // disk
		AutoBooting:    0, // never started
		Network: []SDomainAddNic{{
			VsId: vs.Id, VsName: vs.Name, Mtu: mtu, ProfileId: profile.Id,
			DeviceModel: "virtio", IsKernelAccelerated: 0,
		}},
		Storage: []SDomainAddVolume{{
			Device: "disk", StoreFile: overlayPath, CacheType: "directsync", TargetBus: "virtio",
			Type: "file", DriveType: "qcow2", Priority: STORAGE_PRIORITY_FILE_ONLY, PoolName: pool.Name,
			DiskMode: "subordinate",
		}},
	}
}
