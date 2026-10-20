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

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// SDomainDeploy is the subset of VMRsDomainDeploy used for Host.CreateVM
// (POST /cas/casrs/vm/deploy). Many fields are omitted on purpose; unknown
// defaults are flagged in comments and in the P1 report.
type SDomainDeploy struct {
	Id           int64               `json:"id"` // template id
	Title        string              `json:"title,omitempty"`
	DomainName   string              `json:"domainName"`
	Desc         string              `json:"desc,omitempty"`
	DeployMode   int                 `json:"deployMode"` // 0 normal, 1 fast
	DeployType   int                 `json:"deployType"` // 1 from template list
	TargetHostId int64               `json:"targetHostId"`
	CpuCore      int                 `json:"cpuCore,omitempty"`
	CpuSocket    int                 `json:"cpuSocket,omitempty"`
	Memory       int                 `json:"memory,omitempty"`
	MemoryUnit   string              `json:"memoryUnit,omitempty"` // always "MB" with Memory
	UserData     string              `json:"userData,omitempty"`
	DiskFormat   string              `json:"diskFormat,omitempty"`
	Network      []SDomainDeployNic  `json:"network,omitempty"`
	Storage      []SDomainDeployDisk `json:"storage,omitempty"`
}

type SDomainDeployNic struct {
	Mac         string `json:"mac,omitempty"`
	VSwitchName string `json:"vSwitchName,omitempty"`
	Ip          string `json:"ip,omitempty"`
	SysMask     string `json:"sysMask,omitempty"`
	SysGateway  string `json:"sysGateway,omitempty"`
	SysDns      string `json:"sysDns,omitempty"`
	IsBindIp    bool   `json:"isBindIp,omitempty"`
	SysIpType   int    `json:"sysIpType,omitempty"` // 0 default 1 auto 2 manual
}

type SDomainDeployDisk struct {
	Pool string `json:"pool,omitempty"`
	Dest string `json:"dest,omitempty"`
	Src  string `json:"src,omitempty"`
}

// parseTemplateId turns an image external id into the numeric template id
// expected by VMRsDomainDeploy.id. Accepts "123", "template/123", or the
// account-scoped "CAS/<acc>/template/123".
func parseTemplateId(externalId string) (int64, error) {
	s := strings.TrimSpace(externalId)
	if len(s) == 0 {
		return 0, errors.Wrap(cloudprovider.ErrMissingParameter, "ExternalImageId")
	}
	if i := strings.LastIndex(s, "/template/"); i >= 0 {
		s = s[i+len("/template/"):]
	} else if strings.HasPrefix(s, "template/") {
		s = strings.TrimPrefix(s, "template/")
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.Wrapf(cloudprovider.ErrInputParameter, "ExternalImageId %q", externalId)
	}
	return id, nil
}

// vswitchNameFromNetworkId extracts the vswitch name from a placeholder
// network id "vswitch/<name>/hostpool/<hp>/default".
func vswitchNameFromNetworkId(networkId string) string {
	parts := strings.Split(networkId, "/")
	if len(parts) >= 2 && parts[0] == "vswitch" {
		return parts[1]
	}
	return ""
}

// storagePoolNameFromId extracts the pool name from a storage id
// ("host/<id>/<name>" or "hostpool/<id>/<name>").
func storagePoolNameFromId(storageId string) string {
	parts := strings.Split(storageId, "/")
	if len(parts) >= 3 {
		return parts[len(parts)-1]
	}
	return storageId
}

func buildDeployBody(host *SHost, opts *cloudprovider.SManagedVMCreateConfig) (*SDomainDeploy, error) {
	tplId, err := parseTemplateId(opts.ExternalImageId)
	if err != nil {
		return nil, err
	}
	if opts.Cpu <= 0 || opts.MemoryMB <= 0 {
		return nil, errors.Wrap(cloudprovider.ErrInputParameter, "Cpu and MemoryMB are required")
	}
	body := &SDomainDeploy{
		Id:           tplId,
		Title:        opts.Name,
		DomainName:   opts.Name,
		Desc:         opts.Description,
		DeployMode:   0,
		DeployType:   1,
		TargetHostId: host.Id,
		// One socket with N cores: the split observed on real CAS deploys
		// for deploy (cpuSockets=1, cpuCores=vcpus).
		CpuSocket:  1,
		CpuCore:    opts.Cpu,
		Memory:     opts.MemoryMB,
		MemoryUnit: "MB",
		UserData:   opts.UserData,
	}
	nic := SDomainDeployNic{}
	if vs := vswitchNameFromNetworkId(opts.ExternalNetworkId); len(vs) > 0 {
		nic.VSwitchName = vs
	}
	if len(opts.IpAddr) > 0 {
		nic.Ip = opts.IpAddr
		nic.IsBindIp = true
		nic.SysIpType = 2
	}
	if len(opts.MacAddr) > 0 {
		nic.Mac = opts.MacAddr
	}
	if len(opts.Gateway) > 0 {
		nic.SysGateway = opts.Gateway
	}
	if opts.Masklen > 0 {
		// UNVERIFIED: sysMask format (dotted vs prefix length). Send dotted
		// only when we can derive it; otherwise leave empty.
	}
	if len(opts.Dns) > 0 {
		nic.SysDns = opts.Dns
	}
	if len(nic.VSwitchName) > 0 || len(nic.Ip) > 0 || len(nic.Mac) > 0 {
		body.Network = []SDomainDeployNic{nic}
	}
	if len(opts.SysDisk.StorageExternalId) > 0 {
		body.Storage = []SDomainDeployDisk{{
			Pool: storagePoolNameFromId(opts.SysDisk.StorageExternalId),
		}}
	}
	// DataDisks / SysDisk.SizeGB / Hostname / PublicKey: not mapped —
	// VMRsDomainDeploy has no clear size field (extendDiskCapacity is for
	// a separate cloud-disk flow) and no SSH key field.
	return body, nil
}

type dataDiskSpec struct {
	pool     string
	fileName string
	sizeMB   int64
}

// dataDiskPlan validates DataDisks before anything is deployed: each needs
// a positive size and a pool (its own StorageExternalId, else the system
// disk's). VMRsDomainDeploy has no data-disk size field, so data disks are
// added after deploy with vm/addDevice (new file), matching the observed
// CAS flow of attaching block devices after deploy.
func dataDiskPlan(opts *cloudprovider.SManagedVMCreateConfig) ([]dataDiskSpec, error) {
	ret := []dataDiskSpec{}
	for i, d := range opts.DataDisks {
		if d.SizeGB <= 0 {
			return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "data disk %d: SizeGB %d", i, d.SizeGB)
		}
		storageId := d.StorageExternalId
		if len(storageId) == 0 {
			storageId = opts.SysDisk.StorageExternalId
		}
		if len(storageId) == 0 {
			return nil, errors.Wrapf(cloudprovider.ErrMissingParameter, "data disk %d: no storage", i)
		}
		ret = append(ret, dataDiskSpec{
			pool:     storagePoolNameFromId(storageId),
			fileName: dataDiskFileName(opts.Name, i+1),
			sizeMB:   int64(d.SizeGB) * 1024,
		})
	}
	return ret, nil
}

// ensureDataDiskFilesFree refuses a deploy when any planned data-disk
// file name already exists in its pool (retry after a partial failure,
// or a leftover of the same name, must not reuse/overwrite).
func (self *SHost) ensureDataDiskFilesFree(plan []dataDiskSpec) error {
	cli := self.zone.region.cli
	for i, d := range plan {
		_, err := cli.GetStorageVolume(self.Id, d.pool, d.fileName)
		if err == nil {
			return errors.Wrapf(cloudprovider.ErrDuplicateId, "data disk file %s already exists in pool %s (disk %d)", d.fileName, d.pool, i+1)
		}
		if errors.Cause(err) != cloudprovider.ErrNotFound {
			return errors.Wrapf(err, "GetStorageVolume %s/%s before deploy", d.pool, d.fileName)
		}
	}
	return nil
}

// rollbackDeployedVM best-effort DeleteVM (type=3 recycle, no wipe) after
// a post-deploy data-disk failure. The VM was just created and holds no
// user data; recycling it avoids a leaked CAS guest that cloudpods would
// re-import on the next sync.
func rollbackDeployedVM(vm *SInstance, cause error) error {
	if vm == nil {
		return cause
	}
	rbErr := vm.DeleteVM(context.Background())
	if rbErr != nil {
		return errors.NewAggregate([]error{
			cause,
			errors.Wrapf(rbErr, "rollback DeleteVM of deployed vm %s (%s)", vm.GetName(), vm.Uuid),
		})
	}
	return errors.Wrapf(cause, "rolled back deployed vm %s (%s) into the CAS recycle bin", vm.GetName(), vm.Uuid)
}

// DEFAULT_DEPLOY_TIMEOUT is the task wait for vm/deploy (a full copy of
// a large template can exceed the 10 min default task timeout).
// Override with CasClientConfig.DeployTimeout. UNVERIFIED on CAS how long
// a deploy takes.
const DEFAULT_DEPLOY_TIMEOUT = 30 * time.Minute

// DeployVM is POST /cas/casrs/vm/deploy. Returns the finished SMessage
// (targetId is the new domain id when CAS fills it).
func (cli *SCasClient) DeployVM(body *SDomainDeploy) (*SMessage, error) {
	c := cli.derive()
	c.taskTimeout = cli.deployTaskTimeout()
	// api.yaml parameter name is "domain"; JSON body is the deploy object
	// itself (swagger body params are typically unwrapped to the schema).
	return c.doAsync(httputils.POST, "vm/deploy", nil, jsonutils.Marshal(body))
}

// deployTaskTimeout: CasClientConfig.DeployTimeout, else
// DEFAULT_DEPLOY_TIMEOUT, unless a test shortened taskTimeout.
func (cli *SCasClient) deployTaskTimeout() time.Duration {
	switch {
	case cli.deployTimeout > 0:
		return cli.deployTimeout
	case cli.taskTimeout > 0:
		return cli.taskTimeout
	}
	return DEFAULT_DEPLOY_TIMEOUT
}

// S-1: a deploy must never be retried blind or leave a duplicate.
//
// UNVERIFIED on CAS: whether vm/deploy itself refuses a duplicate domain
// name, whether vm/vmList lists the VM while the deploy task still runs
// (a lookup right after a timeout may miss it; the message says so), and
// whether a deploy whose task wait timed out still completes later.

// deployNameCheck refuses a deploy when a live VM already has the name
// (a retry after an ambiguous deploy would otherwise create a second
// one) and returns the uuids of every VM with the name, recycled ones
// included, so a later lookup can tell a new VM from an old one.
func (cli *SCasClient) deployNameCheck(name string) (map[string]bool, error) {
	vms, err := cli.findVMsByName(name)
	if err != nil {
		return nil, errors.Wrapf(err, "look up VMs named %q before deploy", name)
	}
	before := map[string]bool{}
	for i := range vms {
		if vms[i].isLiveGuest() {
			return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "a VM named %q already exists on CAS (uuid %s, status %s); not deploying a second one. If it is left from an earlier failed or timed-out create, check it on CAS and delete it or adopt it by syncing", name, vms[i].Uuid, vms[i].VmStatus)
		}
		before[vms[i].Uuid] = true
	}
	return before, nil
}

// describeDeployLeftover looks for a VM the deploy may have created (a
// live VM with the name that did not exist before) and returns text for
// the error. It never adopts, stops or deletes it.
func (cli *SCasClient) describeDeployLeftover(name string, before map[string]bool) string {
	vms, err := cli.findVMsByName(name)
	if err != nil {
		return fmt.Sprintf("could not look for a VM named %q (%v): check CAS before retrying", name, err)
	}
	found := []string{}
	for i := range vms {
		if vms[i].isLiveGuest() && !before[vms[i].Uuid] {
			found = append(found, fmt.Sprintf("%s (id %d, status %s)", vms[i].Uuid, vms[i].Id, vms[i].VmStatus))
		}
	}
	if len(found) == 0 {
		return fmt.Sprintf("no new VM named %q is listed yet; CAS may still create it: check CAS before retrying (a retry refuses while one exists)", name)
	}
	return fmt.Sprintf("CAS lists new VM(s) named %q: %s. It was NOT adopted or deleted: check it on CAS, then delete it or let the next sync import it, before retrying", name, strings.Join(found, ", "))
}

func (self *SHost) CreateVM(opts *cloudprovider.SManagedVMCreateConfig) (cloudprovider.ICloudVM, error) {
	// a read-only account sends nothing, not even the name lookup
	if err := self.zone.region.cli.checkWritable("CreateVM"); err != nil {
		return nil, err
	}
	body, err := buildDeployBody(self, opts)
	if err != nil {
		return nil, err
	}
	plan, err := dataDiskPlan(opts)
	if err != nil {
		return nil, err
	}
	if err := self.ensureDataDiskFilesFree(plan); err != nil {
		return nil, err
	}
	cli := self.zone.region.cli
	before, err := cli.deployNameCheck(body.DomainName)
	if err != nil {
		return nil, err
	}
	msg, err := cli.DeployVM(body)
	if err != nil {
		if isDefiniteRejection(err) {
			// refused before any work: nothing to look for
			return nil, errors.Wrapf(err, "DeployVM")
		}
		// a timeout, transport error, 409 or a failed task: CAS may have
		// created the VM. Report it; never retry or adopt here.
		return nil, errors.Wrapf(err, "DeployVM outcome unknown; %s", cli.describeDeployLeftover(body.DomainName, before))
	}
	vm, err := self.zone.region.findDeployedVM(msg)
	if err != nil {
		return nil, errors.Wrapf(err, "%s", cli.describeDeployLeftover(body.DomainName, before))
	}
	vm.host = self
	vm.region = self.zone.region
	if len(plan) > 0 && (vm.GetStatus() == api.VM_RUNNING || vm.GetStatus() == api.VM_SUSPEND) {
		// add disks to a stopped guest only
		if err := vm.StopVM(context.Background(), &cloudprovider.ServerStopOptions{IsForce: true}); err != nil {
			return nil, rollbackDeployedVM(vm, errors.Wrapf(err, "StopVM before adding data disks (status was %s)", vm.GetStatus()))
		}
	}
	for i, d := range plan {
		if err := vm.createDataDisk(d.pool, d.fileName, d.sizeMB); err != nil {
			return nil, rollbackDeployedVM(vm, errors.Wrapf(err, "vm %s (%s) deployed, but adding data disk %d (%d MB in %s as %s) failed", vm.GetName(), vm.Uuid, i+1, d.sizeMB, d.pool, d.fileName))
		}
	}
	if len(plan) > 0 {
		if err := vm.Refresh(); err != nil {
			return nil, rollbackDeployedVM(vm, errors.Wrapf(err, "Refresh after adding data disks"))
		}
		disks, err := vm.getDisks()
		if err != nil {
			return nil, rollbackDeployedVM(vm, errors.Wrapf(err, "getDisks after adding data disks"))
		}
		if len(disks) < len(plan)+1 {
			return nil, rollbackDeployedVM(vm, errors.Errorf("vm %s has %d disks after adding %d data disks, want %d", vm.Uuid, len(disks), len(plan), len(plan)+1))
		}
	}
	// cloudpods CAS guest driver expects VM_READY (shutOff) after create.
	// api.yaml is silent on whether deploy leaves the guest running; if it
	// does, stop so the initial-state wait succeeds.
	if vm.GetStatus() == api.VM_RUNNING || vm.GetStatus() == api.VM_SUSPEND {
		if err := vm.StopVM(context.Background(), &cloudprovider.ServerStopOptions{IsForce: true}); err != nil {
			return nil, errors.Wrapf(err, "StopVM after deploy (status was %s)", vm.GetStatus())
		}
		if err := vm.Refresh(); err != nil {
			return nil, errors.Wrapf(err, "Refresh after post-deploy StopVM")
		}
	}
	return vm, nil
}

// findDeployedVM resolves the VM created by deploy from message.targetId
// only (vm/detail/{id} → GetInstance by uuid). A missing targetId is an
// error: name-scan adoption is unsafe (can attach ExternalId to a leftover
// VM with the same name). UNVERIFIED: whether real CAS always sets targetId
// on deploy completion.
func (self *SRegion) findDeployedVM(msg *SMessage) (*SInstance, error) {
	if msg == nil || msg.TargetId == 0 {
		return nil, errors.Errorf("deploy finished without message.targetId; refusing name-scan adoption")
	}
	detail, err := self.cli.GetInstanceDetail(msg.TargetId)
	if err != nil {
		return nil, errors.Wrapf(err, "GetInstanceDetail %d after deploy", msg.TargetId)
	}
	vm, err := self.cli.GetInstance(detail.Uuid)
	if err != nil {
		return nil, errors.Wrapf(err, "GetInstance %s after deploy", detail.Uuid)
	}
	vm.region = self
	return vm, nil
}
