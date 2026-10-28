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

// VM template management: create from VM, rename, delete. Upload/import
// and template storage pool management are out of scope.

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	// TEMPLATE_OP_CLONE is addVmTemplate opType 1 ("克隆"): the source VM
	// is kept. opType 2 converts the VM into the template and removes the
	// VM, so it is never sent from here.
	TEMPLATE_OP_CLONE = "1"

	// maxDependentsInError caps how many dependent VM names a refused
	// delete lists.
	maxDependentsInError = 5
)

// SVmTemplateInfo is GET vmTemplate/vmTemplateInfo?id= (api.yaml, xml
// root "domain").
type SVmTemplateInfo struct {
	Id              int64   `json:"id"`
	Name            string  `json:"name"`
	Title           string  `json:"title"`
	Description     string  `json:"description"`
	Deployed        bool    `json:"deployed"`
	System          int     `json:"system"`
	Memory          int64   `json:"memory"` // MB per api.yaml
	MemoryInit      float64 `json:"memoryInit"`
	MemoryUnit      string  `json:"memoryUnit"`
	CpuSockets      int     `json:"cpuSockets"`
	CpuCores        int     `json:"cpuCores"`
	CpuMax          int     `json:"cpuMax"`
	StorageCapacity int64   `json:"storageCapacity"`
	CreateDate      string  `json:"createDate"`

	// storages is the raw "storages" value of the response, kept as parsed
	// JSON so a rename can send it back unchanged, unknown fields included.
	// nil when the key is absent. api.yaml does not document storages on
	// vmTemplateInfo, so whether a real CAS returns it is UNVERIFIED.
	storages jsonutils.JSONObject
}

// renameStorages returns the template storages to echo in a rename body.
// It fails closed: only a non-empty JSON array whose every element is an
// object is accepted. Missing, null, empty, a bare object or any other
// shape is an error, because api.yaml does not say what modifyVmTemplate
// does when storages is omitted or different (the CAS UI uses the same
// call to change disk size).
func (info *SVmTemplateInfo) renameStorages() (jsonutils.JSONObject, error) {
	if info.storages == nil {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "template %d: vmTemplateInfo has no storages; refusing to rename without them", info.Id)
	}
	arr, ok := info.storages.(*jsonutils.JSONArray)
	if !ok {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "template %d: storages is %T, not an array; refusing to rename", info.Id, info.storages)
	}
	items, err := arr.GetArray()
	if err != nil || len(items) == 0 {
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "template %d: storages is empty; refusing to rename", info.Id)
	}
	for i := range items {
		if _, ok := items[i].(*jsonutils.JSONDict); !ok {
			return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "template %d: storages[%d] is %T, not an object; refusing to rename", info.Id, i, items[i])
		}
	}
	return info.storages, nil
}

// SVmTemplateModify is the body of PUT vmTemplate/modifyVmTemplate
// (api.yaml body parameter "domain"; sent unwrapped like vm/deploy).
// RenameVmTemplate adds "storages" to it, copied unchanged from
// vmTemplateInfo.
type SVmTemplateModify struct {
	Id          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Memory      int64   `json:"memory,omitempty"`
	MemoryInit  float64 `json:"memoryInit,omitempty"`
	MemoryUnit  string  `json:"memoryUnit,omitempty"`
	Cpu         int     `json:"cpu,omitempty"`
	CpuSocket   int     `json:"cpuSocket,omitempty"`
	CpuCore     int     `json:"cpuCore,omitempty"`
}

// GetVmTemplateInfo is GET vmTemplate/vmTemplateInfo?id=. 404, or a 409
// with Error-Code 1000 / "不存在", is ErrNotFound (see request()).
func (cli *SCasClient) GetVmTemplateInfo(id int64) (*SVmTemplateInfo, error) {
	params := url.Values{}
	params.Set("id", fmt.Sprintf("%d", id))
	resp, err := cli.request(httputils.GET, "vmTemplate/vmTemplateInfo", params, nil)
	if err != nil {
		return nil, err
	}
	resp = unwrapRoot(resp)
	// The JSON form of the "domain" xml root is not documented; accept
	// both {"domain": {...}} and the bare object.
	if inner, err := resp.Get("domain"); err == nil {
		if _, ok := inner.(*jsonutils.JSONDict); ok {
			resp = inner
		}
	}
	info := &SVmTemplateInfo{}
	if err := resp.Unmarshal(info); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal vmTemplateInfo %d", id)
	}
	if resp.Contains("storages") {
		info.storages, _ = resp.Get("storages")
	}
	if info.Id != id {
		return nil, errors.Errorf("vmTemplateInfo?id=%d returned template %d", id, info.Id)
	}
	return info, nil
}

func (cli *SCasClient) getVmTemplateById(id int64) (*SVmTemplate, error) {
	templates, err := cli.GetVmTemplates()
	if err != nil {
		return nil, err
	}
	for i := range templates {
		if templates[i].Id == id {
			return &templates[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "template %d", id)
}

func findTemplatesByName(templates []SVmTemplate, name string) []SVmTemplate {
	ret := []SVmTemplate{}
	for i := range templates {
		if templates[i].Name == name {
			ret = append(ret, templates[i])
		}
	}
	return ret
}

// templateDependents returns the VMs in the unfiltered vm/vmList whose
// templateId ("通过模板部署的虚拟机，部署它的模板id") is id. Recycled VMs
// count too: they can be restored and may still need the template.
func (cli *SCasClient) templateDependents(id int64) ([]SInstance, error) {
	vms, err := cli.listInstances(nil)
	if err != nil {
		return nil, err
	}
	ret := []SInstance{}
	for i := range vms {
		if vms[i].TemplateId == id {
			ret = append(ret, vms[i])
		}
	}
	return ret, nil
}

func dependentNames(vms []SInstance) string {
	names := []string{}
	for i := range vms {
		if i >= maxDependentsInError {
			names = append(names, fmt.Sprintf("... %d more", len(vms)-i))
			break
		}
		names = append(names, fmt.Sprintf("%s(%s)", vms[i].Name, vms[i].Uuid))
	}
	return strings.Join(names, ", ")
}

// CreateVmTemplateFromVM clones a VM into a new template:
// POST vmTemplate/addVmTemplate?id=&uuid=&domainName=&desc=&opType=1
// (all query parameters, no body), waits for the RsTaskMsg, then finds
// the new template by name in vmTemplate/all. It refuses a name that an
// existing template already uses. scope, disConnect and
// templetStoragePath are not sent, so CAS defaults apply.
func (cli *SCasClient) CreateVmTemplateFromVM(vm *SInstance, name, desc string) (*SVmTemplate, error) {
	if err := cli.checkWritable("CreateVmTemplateFromVM"); err != nil {
		return nil, err
	}
	if vm == nil || vm.Id == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "source vm")
	}
	name = strings.TrimSpace(name)
	if len(name) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "template name")
	}
	templates, err := cli.GetVmTemplates()
	if err != nil {
		return nil, errors.Wrapf(err, "GetVmTemplates")
	}
	if dup := findTemplatesByName(templates, name); len(dup) > 0 {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "template name %q already used by template %d", name, dup[0].Id)
	}
	params := url.Values{}
	params.Set("id", fmt.Sprintf("%d", vm.Id))
	if len(vm.Uuid) > 0 {
		params.Set("uuid", vm.Uuid)
	}
	params.Set("domainName", name)
	if len(desc) > 0 {
		params.Set("desc", desc)
	}
	params.Set("opType", TEMPLATE_OP_CLONE)
	if _, err := cli.doAsync(httputils.POST, "vmTemplate/addVmTemplate", params, nil); err != nil {
		return nil, errors.Wrapf(err, "addVmTemplate from vm %d", vm.Id)
	}
	templates, err = cli.GetVmTemplates()
	if err != nil {
		return nil, errors.Wrapf(err, "GetVmTemplates after addVmTemplate")
	}
	found := findTemplatesByName(templates, name)
	switch len(found) {
	case 0:
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "template %q not listed after addVmTemplate", name)
	case 1:
		return &found[0], nil
	default:
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "%d templates named %q after addVmTemplate", len(found), name)
	}
}

// RenameVmTemplate changes the template name, and the description when
// desc is not nil, with PUT vmTemplate/modifyVmTemplate. The body echoes
// the current cpu/memory and the storages value from vmTemplateInfo
// unchanged, so a server that treats the body as a full update keeps
// them. It refuses, without sending a write: a read-only account; an
// empty name; a template whose storages cannot be echoed (see
// renameStorages); a name used by another template; a template with
// deployed=true (the CAS UI says a fast-deployed template cannot change
// its name).
func (cli *SCasClient) RenameVmTemplate(id int64, name string, desc *string) (*SVmTemplate, error) {
	if err := cli.checkWritable("RenameVmTemplate"); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if len(name) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "template name")
	}
	info, err := cli.GetVmTemplateInfo(id)
	if err != nil {
		return nil, errors.Wrapf(err, "GetVmTemplateInfo %d", id)
	}
	newDesc := info.Description
	if desc != nil {
		newDesc = *desc
	}
	if info.Name == name && info.Description == newDesc {
		return cli.getVmTemplateById(id)
	}
	storages, err := info.renameStorages()
	if err != nil {
		return nil, err
	}
	if info.Name != name && info.Deployed {
		return nil, errors.Wrapf(cloudprovider.ErrInvalidStatus, "template %d (%s) has been fast-deployed; CAS does not allow renaming it", id, info.Name)
	}
	if info.Name != name {
		templates, err := cli.GetVmTemplates()
		if err != nil {
			return nil, errors.Wrapf(err, "GetVmTemplates")
		}
		for _, t := range findTemplatesByName(templates, name) {
			if t.Id != id {
				return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "template name %q already used by template %d", name, t.Id)
			}
		}
	}
	body := SVmTemplateModify{
		Id:          id,
		Name:        name,
		Description: newDesc,
		Memory:      info.Memory,
		MemoryInit:  info.MemoryInit,
		MemoryUnit:  info.MemoryUnit,
		CpuSocket:   info.CpuSockets,
		CpuCore:     info.CpuCores,
	}
	if info.CpuSockets > 0 && info.CpuCores > 0 {
		body.Cpu = info.CpuSockets * info.CpuCores
	}
	payload := jsonutils.Marshal(body).(*jsonutils.JSONDict)
	payload.Set("storages", storages)
	if _, err := cli.doAsync(httputils.PUT, "vmTemplate/modifyVmTemplate", nil, payload); err != nil {
		return nil, errors.Wrapf(err, "modifyVmTemplate %d", id)
	}
	tpl, err := cli.getVmTemplateById(id)
	if err != nil {
		return nil, errors.Wrapf(err, "template %d after modifyVmTemplate", id)
	}
	if tpl.Name != name {
		return nil, errors.Errorf("template %d still named %q after modifyVmTemplate to %q", id, tpl.Name, name)
	}
	return tpl, nil
}

// DeleteVmTemplate deletes a template with the task-based
// DELETE vmTemplate/deleteVmTemplateInfo?id= (RsTaskMsg; the plain
// deleteVmTemplate returns 204 with no task to follow). It refuses, with
// ErrInvalidStatus and no write, when:
//   - vmTemplateInfo says deployed=true (fast deploy: VMs may run on the
//     template's base image), or
//   - any VM in vm/vmList, recycled ones included, has templateId == id.
//
// The VM list cannot tell full from linked clones, so every VM deployed
// from the template blocks the delete. A template that is already gone
// returns nil. If the DELETE itself answers not-found (a concurrent
// delete), that is success only when vmTemplate/all confirms the template
// is gone. Afterwards the template must be absent from vmTemplate/all.
// No storage pool or volume endpoint is called.
//
// The dependents check and the DELETE are not atomic: a VM deployed from
// the template in between is not seen here (CAS may still refuse).
func (cli *SCasClient) DeleteVmTemplate(id int64) error {
	if err := cli.checkWritable("DeleteVmTemplate"); err != nil {
		return err
	}
	info, err := cli.GetVmTemplateInfo(id)
	if err != nil {
		if errors.Cause(err) == cloudprovider.ErrNotFound {
			return nil
		}
		return errors.Wrapf(err, "GetVmTemplateInfo %d", id)
	}
	if info.Deployed {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "template %d (%s) has been fast-deployed; refusing to delete", id, info.Name)
	}
	deps, err := cli.templateDependents(id)
	if err != nil {
		return errors.Wrapf(err, "list VMs deployed from template %d", id)
	}
	if len(deps) > 0 {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "template %d (%s) is used by %d VM(s): %s; refusing to delete", id, info.Name, len(deps), dependentNames(deps))
	}
	params := url.Values{}
	params.Set("id", fmt.Sprintf("%d", id))
	if _, err := cli.doAsync(httputils.DELETE, "vmTemplate/deleteVmTemplateInfo", params, nil); err != nil {
		if errors.Cause(err) != cloudprovider.ErrNotFound {
			return errors.Wrapf(err, "deleteVmTemplateInfo %d", id)
		}
		if _, lerr := cli.getVmTemplateById(id); errors.Cause(lerr) != cloudprovider.ErrNotFound {
			return errors.Wrapf(err, "deleteVmTemplateInfo %d returned not-found but vmTemplate/all does not confirm it is gone (%v)", id, lerr)
		}
		return nil
	}
	_, err = cli.getVmTemplateById(id)
	if err == nil {
		return errors.Errorf("template %d still listed after deleteVmTemplateInfo", id)
	}
	if errors.Cause(err) != cloudprovider.ErrNotFound {
		return errors.Wrapf(err, "verify template %d deleted", id)
	}
	return nil
}

// SaveImage clones the VM into a CAS template (opType 1, the VM is kept).
// opts.Name is the template name, opts.Notes its description.
//
// The same rules as the import pipelines. The VM is re-read from
// vm/vmList and must be shut off (a running guest gives an inconsistent
// disk; a VM whose status cannot be read is refused). After the clone,
// checkTemplateFlat must prove that no template disk has a backingStore
// (a template from a linked-clone or overlay VM would depend on a file it
// does not own). If it cannot, an error is returned and the template is
// kept and named in it, never deleted.
func (self *SInstance) SaveImage(opts *cloudprovider.SaveImageOptions) (cloudprovider.ICloudImage, error) {
	if opts == nil {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "SaveImageOptions")
	}
	cli := self.region.cli
	if err := cli.checkWritable("SaveImage"); err != nil {
		return nil, err
	}
	cur, err := cli.findVMByUuid(self.Uuid)
	if err != nil {
		return nil, errors.Wrapf(err, "re-read VM %s before saving it as a template", self.Uuid)
	}
	if !cur.isLiveGuest() {
		return nil, errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s (%s) is recycled or not a CAS guest", cur.Name, cur.Uuid)
	}
	if cur.GetStatus() != api.VM_READY {
		return nil, errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s (%s) is %q; shut it off before saving it as a template", cur.Name, cur.Uuid, cur.VmStatus)
	}
	if cur.HostId == 0 {
		return nil, errors.Wrapf(cloudprovider.ErrInvalidStatus, "VM %s (%s) lists no host; its template disks could not be checked", cur.Name, cur.Uuid)
	}
	tpl, err := cli.CreateVmTemplateFromVM(cur, opts.Name, opts.Notes)
	if err != nil {
		return nil, err
	}
	src := fmt.Sprintf("the disks of VM %s", cur.Name)
	flat, err := cli.checkTemplateFlat(cur.HostId, tpl, src, src)
	if err != nil {
		return nil, errors.Wrap(err, "SaveImage")
	}
	log.Infof("cas SaveImage: template %d %q from VM %s: %s", tpl.Id, tpl.Name, cur.Uuid, flat)
	return &SImage{SVmTemplate: *tpl, cache: self.region.getStoragecache()}, nil
}

// findVMByUuid re-reads one VM from the unfiltered vm/vmList.
func (cli *SCasClient) findVMByUuid(uuid string) (*SInstance, error) {
	if len(uuid) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "vm uuid")
	}
	vms, err := cli.listInstances(nil)
	if err != nil {
		return nil, err
	}
	for i := range vms {
		if vms[i].Uuid == uuid {
			vm := vms[i]
			return &vm, nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "vm %s not in vm/vmList", uuid)
}

// checkTemplateFlat is the flat-template post-check. The template's own backingStore
// is documented as not valid on template queries, so it reads the
// template's disk files (vmTemplate/storage) and looks each one up in the
// storage pool of the import host whose path is the file's directory
// (storage/volume lists backingStore). A disk with a backing file returns
// ErrTemplateHasBackingFile; anything that cannot be checked returns
// ErrTemplateUnverified. Either way the template is kept and named.
func (c *SCasClient) checkTemplateFlat(hostId int64, tpl *SVmTemplate, srcPath, overlayPath string) (string, error) {
	keep := fmt.Sprintf("template %d %q was created and is kept", tpl.Id, tpl.Name)
	unverified := func(format string, args ...interface{}) error {
		return errors.Wrapf(ErrTemplateUnverified, "%s, but %s; check its disk with qemu-img info on the host before deleting or moving %s", keep, fmt.Sprintf(format, args...), srcPath)
	}
	storages, err := c.GetVmTemplateStorages(tpl.Id)
	if err != nil {
		return "", unverified("listing its disks failed (%v)", err)
	}
	checked := []string{}
	for _, st := range storages {
		if !st.isDisk() {
			continue
		}
		if len(st.StoreFile) == 0 {
			return "", unverified("disk %d has no storeFile", st.Id)
		}
		dir := tpl.TempletStoragePath
		if path.IsAbs(st.StoreFile) {
			dir = path.Dir(st.StoreFile)
		}
		name := path.Base(st.StoreFile)
		if len(dir) == 0 {
			return "", unverified("the directory of disk %s is unknown", st.StoreFile)
		}
		pool, err := c.findPoolByPath(hostId, dir)
		if err != nil {
			return "", unverified("no pool on host %d covers %s (%v)", hostId, dir, err)
		}
		raw, err := c.getStorageVolumeRaw(hostId, pool.Name, name)
		if err != nil {
			return "", unverified("volume %s in pool %s could not be read (%v)", name, pool.Name, err)
		}
		// A missing key is not "flat"
		if !raw.Contains("backingStore") {
			return "", unverified("volume %s in pool %s has no backingStore field", name, pool.Name)
		}
		// Only a string is a path; null, numbers, arrays, objects
		// are an unknown shape, not a backing file called "null"
		bv, _ := raw.Get("backingStore")
		bs, ok := bv.(*jsonutils.JSONString)
		if !ok {
			return "", unverified("volume %s in pool %s has a non-string backingStore (%s)", name, pool.Name, bv.String())
		}
		b, _ := bs.GetString()
		if b = strings.TrimSpace(b); len(b) > 0 {
			return "", errors.Wrapf(ErrTemplateHasBackingFile, "%s, but its disk %s/%s has backing file %s, so the template depends on it (source %s, overlay %s): do NOT delete, move or overwrite those files. addVmTemplate did not flatten; the fallback is a full copy (vm/clone cloneMode=0), not implemented yet", keep, path.Clean(dir), name, b, srcPath, overlayPath)
		}
		checked = append(checked, path.Join(dir, name))
	}
	if len(checked) == 0 {
		return "", unverified("no disk was listed for it")
	}
	return "no backingStore on " + strings.Join(checked, ", "), nil
}

// STemplateStorage is api.yaml RsTemplateStorage, an element of
// GET vmTemplate/storage?vmId=<template id> (key "data"). Per api.yaml:
// diskDevice is the kind ("disk cdrom floppy"), device the device name
// ("hda hdb hdc"), driveType the volume type ("如raw"), and format the
// access mode ("Read-Write Read-ForceWrite Read-Only"), not a file format.
type STemplateStorage struct {
	Id         int64   `json:"id"`
	StoreFile  string  `json:"storeFile"`
	Capacity   float64 `json:"capacity"`
	Device     string  `json:"device"`
	DiskDevice string  `json:"diskDevice"`
	DriveType  string  `json:"driveType"`
	Format     string  `json:"format"`
}

// isDisk classifies by diskDevice ("disk", "cdrom", "floppy"). Without it
// (device is only a name like vda/hdc) the entry is checked as a disk, so
// an unknown shape fails closed (Unverified) rather than being skipped.
func (st *STemplateStorage) isDisk() bool {
	if kind := strings.ToLower(strings.TrimSpace(st.DiskDevice)); len(kind) > 0 {
		return kind == "disk"
	}
	return true
}

func (cli *SCasClient) GetVmTemplateStorages(id int64) ([]STemplateStorage, error) {
	params := url.Values{}
	params.Set("vmId", fmt.Sprintf("%d", id))
	ret := []STemplateStorage{}
	return ret, cli.list("vmTemplate/storage", params, "data", &ret)
}

const (
	// ErrTemplateHasBackingFile: a disk of the new template still has a
	// backing file (addVmTemplate did not flatten).
	ErrTemplateHasBackingFile = errors.Error("TemplateHasBackingFile")
	// ErrTemplateUnverified: the template's disks could not be checked.
	ErrTemplateUnverified = errors.Error("TemplateBackingUnverified")
)

func (cli *SCasClient) findPoolByPath(hostId int64, dir string) (*SStoragePool, error) {
	pools, err := cli.GetStoragePools(hostId)
	if err != nil {
		return nil, err
	}
	shared, err := cli.GetSharedStoragePools(hostId)
	if err != nil {
		return nil, err
	}
	for _, p := range append(pools, shared...) {
		if len(p.Path) > 0 && path.Clean(p.Path) == path.Clean(dir) {
			pool := p
			return &pool, nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "no storage pool with path %s on host %d", dir, hostId)
}
