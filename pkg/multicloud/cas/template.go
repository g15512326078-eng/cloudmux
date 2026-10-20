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
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

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
}

// SVmTemplateModify is the body of PUT vmTemplate/modifyVmTemplate
// (api.yaml body parameter "domain"; sent unwrapped like vm/deploy).
// storages is never sent: rename must not touch template disks.
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

// checkWritable fails fast on a read-only account, before any lookup.
// The transport guard in getDefaultClient still blocks the write itself.
func (cli *SCasClient) checkWritable(op string) error {
	if cli.cpcfg.ReadOnly {
		return errors.Wrapf(cloudprovider.ErrAccountReadOnly, "%s", op)
	}
	return nil
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
// the current cpu/memory from vmTemplateInfo so a server that treats the
// body as a full update keeps them; storages is never sent. It refuses:
// a read-only account; an empty name; a name used by another template;
// a template with deployed=true (the CAS UI says a fast-deployed template
// cannot change its name).
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
	if _, err := cli.doAsync(httputils.PUT, "vmTemplate/modifyVmTemplate", nil, jsonutils.Marshal(body)); err != nil {
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
// returns nil. Afterwards the template must be absent from vmTemplate/all.
// No storage pool or volume endpoint is called.
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
		return errors.Wrapf(err, "deleteVmTemplateInfo %d", id)
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
func (self *SInstance) SaveImage(opts *cloudprovider.SaveImageOptions) (cloudprovider.ICloudImage, error) {
	if opts == nil {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "SaveImageOptions")
	}
	tpl, err := self.region.cli.CreateVmTemplateFromVM(self, opts.Name, opts.Notes)
	if err != nil {
		return nil, err
	}
	return &SImage{SVmTemplate: *tpl, cache: self.region.getStoragecache()}, nil
}
