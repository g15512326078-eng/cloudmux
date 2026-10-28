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
//  1. (optional) an ImportSourceStager makes the file present in the pool
//     directory first (today only SSH, mode b, see sshupload.go); then
//     GET storage/refresh so CAS lists it.
//  2. POST storage/volume with baseFile=<source>: a thin qcow2 overlay.
//  3. POST vm/add with the overlay as the only disk; the VM is never
//     started (autoBooting=0).
//  4. Make sure the VM is shut off, then addVmTemplate opType=1
//     (CreateVmTemplateFromVM).
//  5. Post-check: list the template's disks (GET vmTemplate/storage) and
//     their volumes in the template pool (GET storage/volume). A disk with
//     a non-empty backingStore, or one that cannot be checked, makes the
//     import fail (the template is kept and named in the error).
//  6. Delete the temporary VM into the recycle bin (DeleteVM, type=3,
//     isWipeVolume=false, delBase0=false).
//
// Copy strategy: an overlay (storage/volume + baseFile) rather than
// cinder/volume/copy. The overlay endpoint is the one already used for
// data disks and documents baseFile directly; cinder/volume/copy is
// documented as a Cinder snapshot helper keyed by host-pool name with an
// unclear srcName/flag contract. With an overlay the source file is only
// ever a read-only backing file: no request names it as a target, and
// the temporary VM writes (if ever booted) would land in the overlay.
// Because the VM is never booted the overlay stays empty, so the template
// content is exactly the source image. Whether addVmTemplate flattens the
// backing chain into the template is UNVERIFIED (live check), hence the
// post-check in step 5. vm/mergeDomainVol is deliberately not used: its
// merge direction is undocumented and a commit-style merge would write
// into the source. If the lab shows opType=1 does not flatten, the
// documented fallback is a full copy before templating: POST vm/clone
// with cloneMode=0 (not a fast clone) of the temp VM, then template the
// clone. Not built yet: it doubles copy time/space and adds a second VM
// with its own cleanup paths, all unverified.
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
	// done after a Stager ran).
	Refresh bool
	// DryRun only reads (through a read-only client) and returns the plan.
	DryRun bool
	// Stager, when set, makes the file present in the pool first. nil is
	// mode a (the file is already there). NewSSHStager is mode b.
	Stager ImportSourceStager
	// Context, when cancelled (cmx: Ctrl-C), stops the run between steps
	// and stops the HTTP stub. A CAS task already sent is still waited
	// for, and cleanup follows the usual rules. nil = never cancelled.
	Context context.Context
}

// SImportTarget is where a stager must put the source file.
type SImportTarget struct {
	HostId     int64
	HostIp     string
	HostPoolId int64
	PoolName   string
	PoolPath   string
	FileName   string

	// cli is the importer's client (the read-only clone in dry-run), for
	// stagers that drive CAS themselves (HTTP mode).
	cli *SCasClient
	// ctx is SImageImportOptions.Context (interrupts).
	ctx context.Context
}

func (t SImportTarget) Path() string { return path.Join(t.PoolPath, t.FileName) }

func (t SImportTarget) context() context.Context {
	if t.ctx == nil {
		return context.Background()
	}
	return t.ctx
}

// SImportStagePlan describes a staging step without performing it.
type SImportStagePlan struct {
	Step     string
	Method   string
	Detail   string
	Warnings []string
}

// ImportSourceStager makes the source file exist in the pool directory
// (the "upload" step). The importer has already checked that CAS does not
// list the file and that the account is writable; it calls Plan first
// (also in dry-run), then Stage once (never in dry-run), then refreshes
// the pool. Implementations must never overwrite an existing file and
// must not log secrets. Modes: nil stager = pool-file (the file is already
// there), SSSHStager (ssh, sshupload.go), SHTTPStager (http pull through
// nova/download, httpupload.go).
type ImportSourceStager interface {
	// Plan validates the options against t and describes the step. No
	// writes, no listener, no outbound connection.
	Plan(t SImportTarget) (*SImportStagePlan, error)
	// Stage makes t.Path() exist with the planned content. The returned
	// warnings (for example files left for manual cleanup) are added to
	// the result, also when an error is returned.
	Stage(t SImportTarget) ([]string, error)
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

// STORAGE_PRIORITY_FILE_ONLY is vm/add storage.priority 3 ("file only").
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

// checkImportSource refuses a source that is not a standalone, unused
// qcow2. A source with its own backing file would extend the dependency
// chain of the overlay (and maybe the template) to a file we never
// checked. It returns warnings for a level>0 source (CAS marks it as the
// base of other volumes; it is only read here, but deleting it later
// would break those volumes as well).
func checkImportSource(vol *SStorageVolume, srcPath string) ([]string, error) {
	if strings.ToLower(vol.Format) != "qcow2" {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "source %s has format %q; only qcow2 is supported", srcPath, vol.Format)
	}
	if b := strings.TrimSpace(vol.BackingStore); len(b) > 0 {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "source %s has its own backing file %s; import a standalone qcow2 (for example qemu-img convert -O qcow2 the chain into a new file)", srcPath, b)
	}
	if vol.isAttached() {
		return nil, errors.Wrapf(cloudprovider.ErrInvalidStatus, "source %s is used by VM(s) %s; refusing to build on a disk in use", srcPath, vol.Users)
	}
	if vol.Size <= 0 {
		return nil, errors.Errorf("source %s reports size %v MB", srcPath, vol.Size)
	}
	if vol.Level > 0 {
		return []string{fmt.Sprintf("source %s is level %d (CAS marks it as a base file of other volumes); it is only read, but do not delete it", srcPath, vol.Level)}, nil
	}
	return nil, nil
}

// overlayCapacityMB is the overlay virtual size for a source of sizeMB:
// the next whole GiB strictly above the source size. Margin, because
// (1) storage/volume reports size as MB in a double whose rounding on a
// real CAS is unverified, and the unit of capacity on create is
// undocumented (assumed MB); an overlay even slightly smaller than its
// base truncates the guest disk (a GPT backup header at the end is lost);
// (2) the overlay is thin, so up to 1 GiB extra costs no space, and
// cloud-init growpart simply sees a slightly larger disk.
func overlayCapacityMB(sizeMB float64) int64 {
	return (int64(math.Floor(sizeMB/1024)) + 1) * 1024
}

func isTimeout(err error) bool {
	return errors.Cause(err) == cloudprovider.ErrTimeout
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
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "a VM named %s already exists (%s; maybe a recycled temp VM of an earlier run): pick another temporary name with --temp-vm-name", opts.TempVMName, vms[0].Uuid)
	}
	if _, err := c.GetStorageVolume(host.Id, pool.Name, overlay); err == nil {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "overlay volume %s already exists in pool %s (maybe left by an earlier run): pick another temporary name with --temp-vm-name", overlay, pool.Name)
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

	// ---- stage the file (ssh / http), then refresh ----
	refreshPath := fmt.Sprintf("storage/refresh?id=%d&poolName=%s", host.Id, url.QueryEscape(pool.Name))
	if opts.Stager != nil {
		// Rescan first, so a file that is on disk but not listed yet
		// is seen by the name-free check below and never written over.
		if opts.DryRun {
			res.add("refresh-pool-before-upload", "GET", refreshPath, "rescan so the name-free check sees unlisted files (state-changing; skipped in dry-run, so the check below only sees listed files)", true, false)
		} else {
			if err := interrupted("the upload"); err != nil {
				return res, err
			}
			if err := c.RefreshStoragePool(host.Id, pool.Name); err != nil {
				return res, errors.Wrapf(err, "refresh pool %s before the upload", pool.Name)
			}
			res.add("refresh-pool-before-upload", "GET", refreshPath, "", true, true)
		}
		if _, err := c.GetStorageVolume(host.Id, pool.Name, fileName); err == nil {
			return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "%s already exists in pool %s; refusing to upload over it. If it is your earlier upload, rerun with --file %s and without the upload option", fileName, pool.Name, fileName)
		} else if errors.Cause(err) != cloudprovider.ErrNotFound {
			return nil, errors.Wrapf(err, "check %s", fileName)
		}
		target := SImportTarget{HostId: host.Id, HostIp: host.Ip, HostPoolId: host.HostPoolId, PoolName: pool.Name, PoolPath: pool.Path, FileName: fileName, cli: c, ctx: ctx}
		plan, err := opts.Stager.Plan(target)
		if err != nil {
			return nil, err
		}
		res.Warnings = append(res.Warnings, plan.Warnings...)
		if opts.DryRun {
			res.add(plan.Step, plan.Method, srcPath, plan.Detail, true, false)
		} else {
			warns, err := opts.Stager.Stage(target)
			res.Warnings = append(res.Warnings, warns...)
			if err != nil {
				return res, errors.Wrapf(err, "%s", plan.Step)
			}
			res.add(plan.Step, plan.Method, srcPath, plan.Detail, true, true)
		}
		opts.Refresh = true
	}
	if !opts.DryRun {
		if err := interrupted("the pool refresh"); err != nil {
			return res, err
		}
	}
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
		warns, err := checkImportSource(src, srcPath)
		if err != nil {
			return res, err
		}
		res.Warnings = append(res.Warnings, warns...)
		capacity = overlayCapacityMB(src.Size)
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
		res.add("check-template-flat", "GET", "vmTemplate/storage?vmId=<template id> + storage/volume on the template pool", "fail if a template disk has a backingStore", false, false)
		res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/<temp vm uuid>", deleteTempVMDetail, true, false)
		return res, nil
	}

	// ---- writes ----
	region := &SRegion{cli: c}
	var vm *SInstance
	overlayCreated := false
	// vmAddSent/vmAddRejected: the overlay may only be deleted while no VM
	// can be using it, i.e. vm/add was never sent or CAS rejected it
	// outright (isDefiniteRejection).
	vmAddSent, vmAddRejected := false, false
	byHand := func(format string, args ...interface{}) error {
		return errors.Errorf("cleanup: "+format+"; remove by hand: VM %s, overlay %s (the source %s is not affected)",
			append(args, opts.TempVMName, overlayPath, srcPath)...)
	}
	fail := func(cause error) (*SImageImportResult, error) {
		errs := []error{cause}
		if vm == nil && vmAddSent && !vmAddRejected {
			// vm/add was accepted or its outcome is unknown, so the VM may
			// exist and use the overlay: never delete the overlay here.
			// Adopt the VM only when exactly one VM has our name and no
			// task may still be running.
			if isTimeout(cause) {
				errs = append(errs, byHand("vm/add did not finish in time and may still be running; nothing was cleaned up"))
				return res, errors.NewAggregate(errs)
			}
			vms, err := c.findVMsByName(opts.TempVMName)
			switch {
			case err != nil:
				errs = append(errs, byHand("cannot list VMs (%v), so the temporary VM cannot be identified; nothing was cleaned up", err))
			case len(vms) == 1:
				vm = &vms[0]
				vm.region = region
			case len(vms) == 0:
				errs = append(errs, byHand("vm/add outcome unknown and no VM named %s is listed; the overlay was kept in case CAS attaches it later", opts.TempVMName))
			default:
				errs = append(errs, byHand("%d VMs are named %s, refusing to pick one (ours has description %q); nothing was cleaned up", len(vms), opts.TempVMName, importTempVMDescription(&opts)))
			}
			if vm == nil {
				return res, errors.NewAggregate(errs)
			}
		}
		switch {
		case vm != nil:
			if err := vm.DeleteVM(context.Background()); err != nil {
				errs = append(errs, errors.Wrapf(err, "cleanup: delete temporary VM %s (%s); remove it by hand", vm.Name, vm.Uuid))
			} else {
				res.add("cleanup-delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, "recycle bin; the overlay stays with the recycled VM", true, true)
			}
		case overlayCreated && vmAddSent:
			// Even after a definite rejection, keep the overlay if any
			// VM with our name exists or the list cannot be read.
			if vms, err := c.findVMsByName(opts.TempVMName); err != nil {
				errs = append(errs, byHand("vm/add was rejected but VMs cannot be listed (%v); the overlay was kept", err))
			} else if len(vms) > 0 {
				errs = append(errs, byHand("vm/add was rejected but %d VM(s) named %s exist; the overlay was kept", len(vms), opts.TempVMName))
			} else if err := c.deleteImportOverlay(res, host.Id, pool.Name, overlay, overlayPath, fileName); err != nil {
				errs = append(errs, err)
			}
		case overlayCreated:
			if err := c.deleteImportOverlay(res, host.Id, pool.Name, overlay, overlayPath, fileName); err != nil {
				errs = append(errs, err)
			}
		}
		return res, errors.NewAggregate(errs)
	}

	if err := interrupted("creating the overlay"); err != nil {
		return res, err
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

	if err := interrupted("vm/add"); err != nil {
		return fail(err)
	}
	vmAddSent = true
	if vmAddRejected, err = c.postTempVM(opts.TempVMName, []byte(jsonutils.Marshal(addBody).String())); err != nil {
		return fail(err)
	}
	res.add("create-vm", "POST", "vm/add", jsonutils.Marshal(addBody).String(), true, true)
	if vm, err = c.findTempVM(region, opts.TempVMName); err != nil {
		return fail(err)
	}

	switch st := vm.GetStatus(); st {
	case api.VM_READY:
	case api.VM_RUNNING, api.VM_SUSPEND:
		// CAS ignored autoBooting=0: the first boot (cloud-init) may already
		// have written machine-id, host keys etc. into the overlay, so the
		// template would not be clean. Refuse; cleanup powers it off and
		// recycles it.
		return fail(errors.Wrapf(cloudprovider.ErrInvalidStatus, "temporary VM %s was started by CAS (status %s) although autoBooting=0; refusing to template a booted image", vm.Name, st))
	default:
		return fail(errors.Errorf("temporary VM %s has unexpected status %q", vm.Name, vm.VmStatus))
	}

	if err := interrupted("addVmTemplate"); err != nil {
		return fail(err)
	}
	tpl, err := c.CreateVmTemplateFromVM(vm, opts.TemplateName, opts.Description)
	if err != nil {
		found, lerrs := c.templateLeftovers(opts.TemplateName, "check it, and delete it by hand if broken")
		if len(found) == 1 {
			res.Template = &found[0]
		}
		errs := append([]error{errors.Wrapf(err, "clone temporary VM to template")}, lerrs...)
		if isTimeout(err) {
			// The clone task may still be copying the VM's disk: do not
			// delete the VM under it.
			errs = append(errs, errors.Errorf("addVmTemplate did not finish in time (see the task/message id above) and may still be copying; temporary VM %s (%s) and overlay %s were kept. When the task has ended, check the template and remove them by hand", vm.Name, vm.Uuid, overlayPath))
			return res, errors.NewAggregate(errs)
		}
		return fail(errors.NewAggregate(errs))
	}
	res.Template = tpl
	res.add("clone-to-template", "POST", "vmTemplate/addVmTemplate", fmt.Sprintf("id=%d&domainName=%s&opType=%s", vm.Id, opts.TemplateName, TEMPLATE_OP_CLONE), true, true)

	errs := []error{}
	detail, err := c.checkTemplateFlat(host.Id, tpl, srcPath, overlayPath)
	if err != nil {
		// The template may depend on the overlay, and a recycle-bin
		// purge (retention) would remove it without anyone acting. Keep
		// the temp VM live so the overlay stays referenced.
		return res, errors.NewAggregate([]error{err, errors.Errorf("temporary VM %s (%s) was kept live (not recycled) with its overlay %s, because the template may depend on them; delete it only after the template disk is confirmed flat (qemu-img info --backing-chain)", vm.Name, vm.Uuid, overlayPath)})
	}
	res.add("check-template-flat", "GET", fmt.Sprintf("vmTemplate/storage?vmId=%d", tpl.Id), detail, false, true)

	if err := vm.DeleteVM(context.Background()); err != nil {
		errs = append(errs, errors.Wrapf(err, "template %d created, but deleting temporary VM %s (%s) failed; remove it by hand", tpl.Id, vm.Name, vm.Uuid))
	} else {
		res.add("delete-temp-vm", "DELETE", "vm/delete/uuid/"+vm.Uuid, deleteTempVMDetail+"; overlay stays with it", true, true)
	}
	if len(errs) > 0 {
		return res, errors.NewAggregate(errs)
	}
	return res, nil
}

var deleteTempVMDetail = fmt.Sprintf("type=%s&isWipeVolume=%s&delBase0=%s (recycle bin)", DELETE_VM_TYPE, DELETE_VM_WIPE_VOLUME, DELETE_VM_DEL_BASE0)

func importTempVMDescription(o *SImageImportOptions) string {
	return "temporary VM for template " + o.TemplateName + "; safe to delete"
}

func (c *SCasClient) findTemplatesNamed(name string) ([]SVmTemplate, error) {
	templates, err := c.GetVmTemplates()
	if err != nil {
		return nil, err
	}
	return findTemplatesByName(templates, name), nil
}

// deleteImportOverlay removes the overlay after a failure in which no VM
// can be using it. It re-reads the volume first and refuses when CAS
// lists any user, or when the name is the source's.
func (c *SCasClient) deleteImportOverlay(res *SImageImportResult, hostId int64, poolName, overlay, overlayPath, fileName string) error {
	if overlay == fileName {
		return errors.Errorf("cleanup: refusing to delete %s, it is the source", overlay)
	}
	vol, err := c.GetStorageVolume(hostId, poolName, overlay)
	if errors.Cause(err) == cloudprovider.ErrNotFound {
		return nil
	}
	if err != nil {
		return errors.Wrapf(err, "cleanup: re-read overlay %s before deleting it; remove it by hand once no VM uses it", overlayPath)
	}
	if vol.isAttached() {
		return errors.Errorf("cleanup: overlay %s is used by %s; not deleting it, remove it by hand once unused", overlayPath, vol.Users)
	}
	if err := c.DeleteStorageVolume(hostId, poolName, overlay); err != nil {
		return errors.Wrapf(err, "cleanup: delete overlay %s; remove it by hand", overlayPath)
	}
	res.add("cleanup-delete-overlay", "DELETE", fmt.Sprintf("storage/volume/delete/%d/%s/%s", hostId, poolName, overlay), "isWipeVolume=false", true, true)
	return nil
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
		Description:    importTempVMDescription(o),
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
