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
	"net/url"
	"strconv"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"
	"yunion.io/x/pkg/util/timeutils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SInstance is one element of GET /cas/casrs/vm/vmList (key "domain").
// Disks and NICs are only in GET /cas/casrs/vm/detail/{id}, fetched lazily.
type SInstance struct {
	multicloud.SInstanceBase
	multicloud.STagBase

	region *SRegion
	host   *SHost
	detail *SInstanceDetail

	Id           int64   `json:"id"`
	HostId       int64   `json:"hostId"`
	ClusterId    int64   `json:"clusterId"`
	HostPoolId   int64   `json:"hostPoolId"`
	TemplateId   int64   `json:"templateId"`
	Name         string  `json:"name"`
	ComputerName string  `json:"computerName"`
	Title        string  `json:"title"`
	HostName     string  `json:"hostName"`
	Description  string  `json:"description"`
	Memory       int64   `json:"memory"` // MB
	CpuRate      float64 `json:"cpuRate"`
	MemRate      float64 `json:"memRate"`
	Cpu          int     `json:"cpu"` // sockets * cores
	VmStatus     string  `json:"vmStatus"`
	Uuid         string  `json:"uuid"`
	System       int     `json:"system"` // 0 Windows, 1 Linux, 2 BSD
	OsDesc       string  `json:"osDesc"`
	CreateDate   string  `json:"createDate"`
	// LastOffTime is epoch seconds (example 1608551670) when present.
	// UNVERIFIED whether vmList returns it; summary/new does (api.yaml).
	LastOffTime int64 `json:"lastOffTime"`
	Type        int   `json:"type"` // 0 CAS, 1 VMware
	// DelTime is DomainRsDomainvmList.delTime ("虚拟机逻辑回收时间").
	// api.yaml types it as a date string; observed CAS returns integer 0
	// (or "0") for live VMs. See isRecycled. POST /vm/restoreVm restores
	// a recycled VM.
	DelTime string `json:"delTime"`
}

// SInstanceDetail is the subset of VMRsDomainDetail used for read sync.
type SInstanceDetail struct {
	Id     int64  `json:"id"`
	Name   string `json:"name"`
	Uuid   string `json:"uuid"`
	Status string `json:"status"`

	disks []SInstanceStorage
	nics  []SInstanceNetwork
}

// SInstanceStorage is VMRsDomainDetail.storage[].
type SInstanceStorage struct {
	Device         string `json:"device"` // disk, cdrom, floppy
	DeviceName     string `json:"deviceName"`
	FileType       string `json:"fileType"`
	Path           string `json:"path"`
	Format         string `json:"format"`
	TargetBus      string `json:"targetBus"`
	Size           int64  `json:"size"` // MB (assumed, see report)
	CacheType      string `json:"cacheType"`
	BackingFile    string `json:"backingFile"`
	IsShareStorage bool   `json:"isShareStorage"`
}

// SInstanceNetwork is VMRsDomainDetail.network[].
type SInstanceNetwork struct {
	NetType     int    `json:"netType"`
	Mac         string `json:"mac"`
	IpAddr      string `json:"ipAddr"`
	Ipv6        string `json:"ipv6"`
	VsId        int64  `json:"vsId"`
	VsName      string `json:"vsName"`
	DeviceModel string `json:"deviceModel"`
	Vlan        int    `json:"vlan"`
	ProfileName string `json:"profileName"`
}

// VM_TYPE_VMWARE is vmList "type" 1 ("VMware虚拟机"), a VM CAS manages on a
// VMware host; those are not CAS-hypervisor guests and are not synced.
const VM_TYPE_VMWARE = 1

// isRecycled reports whether the VM is in the CAS recycle bin, from
// delTime. Live values: empty/null, or a zero time — an integer <= 0 (the
// CVM column default is 0) or a date in 1970 or earlier (what a
// serialized 0 looks like in any time zone). Anything else, including an unparseable
// non-empty string, counts as recycled. UNVERIFIED on a real CAS: which of
// the int / date-string forms vm/vmList actually emits.
func (self *SInstance) isRecycled() bool {
	return isRecycledDelTime(self.DelTime)
}

func isRecycledDelTime(delTime string) bool {
	s := strings.TrimSpace(delTime)
	if len(s) == 0 || strings.EqualFold(s, "null") {
		return false
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		return n > 0
	}
	if t, err := timeutils.ParseTimeStr(s); err == nil {
		// a zero int rendered as a local date is 1970-01-01 in any zone
		return t.Year() > 1970
	}
	return true
}

// isLiveGuest is true for CAS-hypervisor VMs that are not in the recycle bin.
func (self *SInstance) isLiveGuest() bool {
	return self.Type != VM_TYPE_VMWARE && !self.isRecycled()
}

// GetInstances is the paged vm/vmList without VMware-type or recycled VMs.
// cachedInstances / host.GetIVMs / storage disk scans all go through this.
func (cli *SCasClient) GetInstances(params url.Values) ([]SInstance, error) {
	vms, err := cli.listInstances(params)
	if err != nil {
		return nil, err
	}
	ret := []SInstance{}
	for i := range vms {
		if vms[i].isLiveGuest() {
			ret = append(ret, vms[i])
		}
	}
	return ret, nil
}

func (cli *SCasClient) listInstances(params url.Values) ([]SInstance, error) {
	items, err := cli.listPaged("vm/vmList", params, "domain", "uuid")
	if err != nil {
		return nil, err
	}
	ret := []SInstance{}
	return ret, jsonutils.NewArray(items...).Unmarshal(&ret)
}

// waitVMNotLive polls the raw vm/vmList until uuid is missing or recycled
// (delTime set). Uses the same poll interval/timeout as waitTask.
func (cli *SCasClient) waitVMNotLive(uuid string) error {
	interval, timeout := cli.taskPollInterval, cli.taskTimeout
	if interval <= 0 {
		interval = DEFAULT_TASK_POLL_INTERVAL
	}
	if timeout <= 0 {
		timeout = DEFAULT_TASK_TIMEOUT
	}
	err := cloudprovider.Wait(interval, timeout, func() (bool, error) {
		if err := cli.ctxErr(); err != nil {
			return false, err
		}
		live, err := cli.vmIsLiveInList(uuid)
		if err != nil {
			// A cancel during the read surfaces as a transport error.
			if cerr := cli.ctxErr(); cerr != nil {
				return false, errors.Wrap(cerr, "cancelled while waiting")
			}
			return false, err
		}
		return !live, nil
	})
	if err != nil {
		return errors.Wrapf(err, "waitVMNotLive %s (still present as a live guest after delete)", uuid)
	}
	return nil
}

// vmIsLiveInList reports whether uuid appears in vm/vmList as a live
// (non-VMware, non-recycled) guest. Uses the unfiltered list so a recycled
// entry with delTime is visible and treated as not live.
func (cli *SCasClient) vmIsLiveInList(uuid string) (bool, error) {
	vms, err := cli.listInstances(nil)
	if err != nil {
		return false, err
	}
	for i := range vms {
		if vms[i].Uuid == uuid {
			return vms[i].isLiveGuest(), nil
		}
	}
	return false, nil
}

// GetInstance looks a VM up by uuid (its external id):
//  1. GET vm/detail/uuid/{uuid} (path-based, api.yaml returns a single
//     VMRsDomainDetail); 404, or 409 whose message says "不存在", is
//     ErrNotFound via request(); a detail for another uuid is an error.
//  2. The detail has no hostId/hostPoolId, so the list fields come from
//     GET vm/vmList?hpId=<hp>&domainIdList=<id> for each host pool
//     (api.yaml: domainIdList only works together with hpId).
//  3. If no pool returned it, the full paged vm/vmList.
//
// A VMware-type VM is ErrNotFound. If the detail exists but no list has
// the VM, that is an error, not ErrNotFound, so cloudpods does not mark
// the VM lost.
func (cli *SCasClient) GetInstance(uuid string) (*SInstance, error) {
	if len(uuid) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrNotFound, "empty vm uuid")
	}
	resp, err := cli.request(httputils.GET, "vm/detail/uuid/"+url.PathEscape(uuid), nil, nil)
	if err != nil {
		return nil, err
	}
	detail, err := parseInstanceDetail(resp)
	if err != nil {
		return nil, errors.Wrapf(err, "vm/detail/uuid/%s", uuid)
	}
	if detail.Uuid != uuid {
		return nil, errors.Errorf("vm/detail/uuid/%s returned vm %s", uuid, detail.Uuid)
	}
	var notFound error
	find := func(vms []SInstance) *SInstance {
		for i := range vms {
			if vms[i].Uuid == uuid {
				if !vms[i].isLiveGuest() {
					if vms[i].isRecycled() {
						notFound = errors.Wrapf(cloudprovider.ErrNotFound, "vm %s is in the recycle bin (delTime=%s)", uuid, vms[i].DelTime)
					} else {
						notFound = errors.Wrapf(cloudprovider.ErrNotFound, "vm %s is a VMware VM", uuid)
					}
					return nil
				}
				vms[i].detail = detail
				return &vms[i]
			}
		}
		return nil
	}
	pools, err := cli.GetHostPools()
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostPools")
	}
	for i := range pools {
		params := url.Values{}
		params.Set("hpId", fmt.Sprintf("%d", pools[i].Id))
		params.Set("domainIdList", fmt.Sprintf("%d", detail.Id))
		vms := []SInstance{}
		err = cli.list("vm/vmList", params, "domain", &vms)
		if err != nil {
			return nil, err
		}
		if vm := find(vms); vm != nil || notFound != nil {
			return vm, notFound
		}
	}
	vms, err := cli.listInstances(nil)
	if err != nil {
		return nil, err
	}
	if vm := find(vms); vm != nil || notFound != nil {
		return vm, notFound
	}
	return nil, errors.Errorf("vm %s has a detail (id %d) but is not in vm/vmList", uuid, detail.Id)
}

func (cli *SCasClient) GetInstanceDetail(id int64) (*SInstanceDetail, error) {
	resp, err := cli.request(httputils.GET, fmt.Sprintf("vm/detail/%d", id), nil, nil)
	if err != nil {
		return nil, err
	}
	return parseInstanceDetail(resp)
}

// parseInstanceDetails decodes a {"domain": [VMRsDomainDetail]} list as
// returned by vm/list/detail and vm/detail/all.
func parseInstanceDetails(resp jsonutils.JSONObject) ([]SInstanceDetail, error) {
	items, err := listItems(resp, "domain")
	if err != nil {
		return nil, err
	}
	ret := []SInstanceDetail{}
	for i := range items {
		detail, err := parseInstanceDetail(items[i])
		if err != nil {
			return nil, errors.Wrapf(err, "domain[%d]", i)
		}
		ret = append(ret, *detail)
	}
	return ret, nil
}

func parseInstanceDetail(resp jsonutils.JSONObject) (*SInstanceDetail, error) {
	resp = unwrapRoot(resp)
	detail := &SInstanceDetail{}
	err := resp.Unmarshal(detail)
	if err != nil {
		return nil, errors.Wrapf(err, "Unmarshal detail")
	}
	if len(detail.Uuid) == 0 {
		return nil, errors.Wrapf(ErrEmptyBody, "vm detail without uuid")
	}
	err = unmarshalList(resp, "storage", &detail.disks)
	if err != nil {
		return nil, errors.Wrapf(err, "storage")
	}
	err = unmarshalList(resp, "network", &detail.nics)
	if err != nil {
		return nil, errors.Wrapf(err, "network")
	}
	return detail, nil
}

// GetHostInstanceDetails is GET vm/detail/all?hostId=: the details of every
// VM on a host in one call (hostId is required by api.yaml).
func (cli *SCasClient) GetHostInstanceDetails(hostId int64) ([]SInstanceDetail, error) {
	params := url.Values{}
	params.Set("hostId", fmt.Sprintf("%d", hostId))
	resp, err := cli.request(httputils.GET, "vm/detail/all", params, nil)
	if err != nil {
		return nil, err
	}
	return parseInstanceDetails(resp)
}

// getDetail takes the VM's detail from the cached per-host bulk detail
// (vm/detail/all?hostId=) and only falls back to vm/detail/{id} for a VM
// that is not in its host's list (e.g. it migrated mid-sync).
func (self *SInstance) getDetail() (*SInstanceDetail, error) {
	if self.detail != nil {
		return self.detail, nil
	}
	details, err := self.region.cachedHostDetails(self.HostId)
	if err != nil {
		return nil, err
	}
	if detail, ok := details[self.Uuid]; ok {
		self.detail = detail
		return detail, nil
	}
	detail, err := self.region.cli.GetInstanceDetail(self.Id)
	if err != nil {
		return nil, errors.Wrapf(err, "GetInstanceDetail %d", self.Id)
	}
	self.detail = detail
	return detail, nil
}

func (self *SInstance) GetId() string {
	return self.Uuid
}

func (self *SInstance) GetGlobalId() string {
	return self.Uuid
}

func (self *SInstance) GetName() string {
	if len(self.Title) > 0 {
		return self.Title
	}
	return self.Name
}

func (self *SInstance) GetHostname() string {
	return self.ComputerName
}

func (self *SInstance) GetDescription() string {
	return self.Description
}

func (self *SInstance) GetCreatedAt() time.Time {
	t, err := time.Parse(time.RFC3339, self.CreateDate)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Refresh re-reads the VM bypassing the per-sync cache and keeps the fresh
// detail (jsonutils.Update only copies exported fields, and getDetail would
// otherwise fall back to the possibly older cached bulk detail).
func (self *SInstance) Refresh() error {
	vm, err := self.region.cli.GetInstance(self.Uuid)
	if err != nil {
		return err
	}
	err = jsonutils.Update(self, vm)
	if err != nil {
		return err
	}
	self.detail = vm.detail
	return nil
}

// GetStatus maps vmStatus: unknown, running, paused, shutOff, ha_exception.
func (self *SInstance) GetStatus() string {
	switch self.VmStatus {
	case "running":
		return api.VM_RUNNING
	case "shutOff":
		return api.VM_READY
	case "paused":
		return api.VM_SUSPEND
	}
	return api.VM_UNKNOWN
}

func (self *SInstance) GetHypervisor() string {
	return api.HYPERVISOR_CAS
}

func (self *SInstance) GetIHostId() string {
	return fmt.Sprintf("%d", self.HostId)
}

func (self *SInstance) getHost() (*SHost, error) {
	if self.host != nil {
		return self.host, nil
	}
	host, err := self.region.getHost(self.GetIHostId())
	if err != nil {
		return nil, err
	}
	self.host = host
	return host, nil
}

func (self *SInstance) GetIHost() cloudprovider.ICloudHost {
	host, err := self.getHost()
	if err != nil {
		return nil
	}
	return host
}

func (self *SInstance) GetVcpuCount() int {
	return self.Cpu
}

func (self *SInstance) GetVmemSizeMB() int {
	return int(self.Memory)
}

func (self *SInstance) GetInstanceType() string {
	return ""
}

func (self *SInstance) GetMachine() string {
	return "pc"
}

func (self *SInstance) GetBootOrder() string {
	return "dcn"
}

func (self *SInstance) GetVga() string {
	return "std"
}

func (self *SInstance) GetVdi() string {
	return "vnc"
}

func (self *SInstance) GetFullOsName() string {
	return self.OsDesc
}

func (self *SInstance) GetOsType() cloudprovider.TOsType {
	if self.System == 0 {
		return cloudprovider.OsTypeWindows
	}
	return cloudprovider.OsTypeLinux
}

func (self *SInstance) GetOsDist() string {
	return ""
}

func (self *SInstance) GetOsVersion() string {
	return ""
}

func (self *SInstance) GetOsLang() string {
	return ""
}

func (self *SInstance) GetOsArch() string {
	return ""
}

func (self *SInstance) GetBios() cloudprovider.TBiosType {
	return cloudprovider.BIOS
}

func (self *SInstance) GetProjectId() string {
	return ""
}

func (self *SInstance) getDisks() ([]SDisk, error) {
	detail, err := self.getDetail()
	if err != nil {
		return nil, err
	}
	ret := []SDisk{}
	for i := range detail.disks {
		if detail.disks[i].Device != "disk" {
			continue
		}
		ret = append(ret, SDisk{
			SInstanceStorage: detail.disks[i],
			ins:              self,
			index:            len(ret),
		})
	}
	return ret, nil
}

func (self *SInstance) GetIDisks() ([]cloudprovider.ICloudDisk, error) {
	disks, err := self.getDisks()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudDisk{}
	for i := range disks {
		if _, err := disks[i].getStorage(); err != nil {
			if errors.Cause(err) != cloudprovider.ErrNotFound {
				// Transient host/pool lookup failure must not flip the
				// disk id to the legacy form for one sync.
				return nil, errors.Wrapf(err, "resolve storage for disk %s of vm %s", disks[i].Path, self.GetName())
			}
			// Pool truly missing from the host listing: legacy id is OK.
		}
		ret = append(ret, &disks[i])
	}
	return ret, nil
}

func (self *SInstance) GetINics() ([]cloudprovider.ICloudNic, error) {
	detail, err := self.getDetail()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudNic{}
	for i := range detail.nics {
		// netType 2 is "none" (no network attached)
		if detail.nics[i].NetType == NIC_NET_TYPE_NONE {
			continue
		}
		ret = append(ret, &SInstanceNic{SInstanceNetwork: detail.nics[i], ins: self})
	}
	return ret, nil
}

func (self *SInstance) GetIEIP() (cloudprovider.ICloudEIP, error) {
	return nil, nil
}

func (self *SInstance) GetSecurityGroupIds() ([]string, error) {
	return []string{}, nil
}

func (self *SInstance) SetSecurityGroups(secgroupIds []string) error {
	return cloudprovider.ErrNotSupported
}

// StartVM is PUT /cas/casrs/vm/start/{id} (api.yaml: "uuid也适用", path type
// is integer — we send the numeric domain id). Returns RsTaskMsg; waitTask.
func (self *SInstance) StartVM(ctx context.Context) error {
	_, err := self.region.cli.WithContext(ctx).doAsync(httputils.PUT, fmt.Sprintf("vm/start/%d", self.Id), nil, nil)
	return err
}

// StopVM is PUT /cas/casrs/vm/stop/{id}. When opts.IsForce is set it uses
// PUT /cas/casrs/vm/powerOff/{id} instead ("关闭虚拟机电源"). Both return
// RsTaskMsg. UNVERIFIED: whether stop is ACPI-graceful and how long
// powerOff takes vs stop.
func (self *SInstance) StopVM(ctx context.Context, opts *cloudprovider.ServerStopOptions) error {
	path := fmt.Sprintf("vm/stop/%d", self.Id)
	if opts != nil && opts.IsForce {
		path = fmt.Sprintf("vm/powerOff/%d", self.Id)
	}
	_, err := self.region.cli.WithContext(ctx).doAsync(httputils.PUT, path, nil, nil)
	return err
}

// RestartVM is PUT /cas/casrs/vm/restart/{id} (checklist 1.3). ICloudVM has
// no Reboot method; cloudpods soft-reboot of managed guests typically
// stop+start. This is exposed for callers that want the native restart.
func (self *SInstance) RestartVM(ctx context.Context) error {
	_, err := self.region.cli.WithContext(ctx).doAsync(httputils.PUT, fmt.Sprintf("vm/restart/%d", self.Id), nil, nil)
	return err
}

// DELETE_VM_TYPE is the api.yaml "type" query on DELETE
// /vm/delete/uuid/{uuid}: 0=keep storage, 1=delete storage, 3=recycle bin.
// Deletes default to the recycle bin so a mistaken delete is reversible on CAS.
// Flip to "1" (and optionally set DELETE_VM_WIPE_VOLUME) only after a real
// CAS confirms wipe/delete-storage behaviour.
const DELETE_VM_TYPE = "3"

// DELETE_VM_WIPE_VOLUME is api.yaml isWipeVolume ("彻底删除数据"). Kept
// false; never combine with type=3 unless real CAS requires it.
const DELETE_VM_WIPE_VOLUME = "false"

// DELETE_VM_DEL_BASE0 is api.yaml delBase0 ("删除base0 true 是 false 否",
// delete base0). Always sent as "false" instead of leaving CAS to pick an
// undocumented default: a VM disk can be a qcow2 overlay whose base is a
// file someone else owns (the image importer's temp VM is backed by the
// user's cloud image). UNVERIFIED: whether CAS honours it when the
// recycle bin is purged later.
const DELETE_VM_DEL_BASE0 = "false"

// DeleteVM stops a running/paused guest first (force powerOff), then
// DELETE /cas/casrs/vm/delete/uuid/{uuid} with DELETE_VM_TYPE /
// DELETE_VM_WIPE_VOLUME / DELETE_VM_DEL_BASE0, then waits until the VM is absent from the live
// vmList (missing, or present with delTime set). waitTask completion alone
// is not enough: type=3 leaves the domain in the recycle bin and it can
// still appear in vm/vmList until delTime is set / it disappears.
// UNVERIFIED: whether CAS rejects delete while running, what delBase0
// does on purge, and recycle-bin restore. Shared by cloudpods DeleteVM,
// the deploy rollback and the image importer; delBase0=false is the
// conservative value for all of them.
func (self *SInstance) DeleteVM(ctx context.Context) error {
	status := self.GetStatus()
	if status == api.VM_RUNNING || status == api.VM_SUSPEND {
		if err := self.StopVM(ctx, &cloudprovider.ServerStopOptions{IsForce: true}); err != nil {
			return errors.Wrapf(err, "StopVM before DeleteVM (status was %s)", status)
		}
	}
	params := url.Values{}
	params.Set("type", DELETE_VM_TYPE)
	params.Set("isWipeVolume", DELETE_VM_WIPE_VOLUME)
	params.Set("delBase0", DELETE_VM_DEL_BASE0)
	cli := self.region.cli.WithContext(ctx)
	_, err := cli.doAsync(httputils.DELETE, "vm/delete/uuid/"+url.PathEscape(self.Uuid), params, nil)
	if err != nil {
		return err
	}
	return cli.waitVMNotLive(self.Uuid)
}

func (self *SInstance) UpdateVM(ctx context.Context, input cloudprovider.SInstanceUpdateOptions) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "UpdateVM")
}

func (self *SInstance) UpdateUserData(userData string) error {
	return cloudprovider.ErrNotSupported
}

func (self *SInstance) RebuildRoot(ctx context.Context, desc *cloudprovider.SManagedVMRebuildRootConfig) (string, error) {
	return "", errors.Wrap(cloudprovider.ErrNotImplemented, "RebuildRoot")
}

func (self *SInstance) DeployVM(ctx context.Context, opts *cloudprovider.SInstanceDeployOptions) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "DeployVM")
}

func (self *SInstance) ChangeConfig(ctx context.Context, config *cloudprovider.SManagedVMChangeConfig) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "ChangeConfig")
}

// GetVNCInfo is GET /cas/casrs/vmvnc/vnc/{id} (sync). novnc/{id} also exists
// (proxyIp + token) but is unused here — cloudpods' managed VNC path wants
// host/port/password. Password is returned to the caller; it is redacted
// in debug logs via isSecretKey.
func (self *SInstance) GetVNCInfo(input *cloudprovider.ServerVncInput) (*cloudprovider.ServerVncOutput, error) {
	info, err := self.region.cli.GetVnc(self.Id)
	if err != nil {
		return nil, err
	}
	return &cloudprovider.ServerVncOutput{
		Protocol:   "vnc",
		Host:       info.Ip,
		Port:       int64(info.Port),
		Password:   info.Password,
		Hypervisor: api.HYPERVISOR_CAS,
		InstanceId: self.Uuid,
	}, nil
}

// vm/detail network[].netType: 0 bridge, 1 NAT, 2 none, 3 passthrough,
// 4 SR-IOV. Only "none" is skipped; 3/4 are kept unverified.
const NIC_NET_TYPE_NONE = 2

// SInstanceNic wraps VMRsDomainDetail.network[].
type SInstanceNic struct {
	cloudprovider.DummyICloudNic
	SInstanceNetwork

	ins *SInstance
}

func (self *SInstanceNic) GetId() string {
	return fmt.Sprintf("%s/%s", self.ins.Uuid, self.Mac)
}

func (self *SInstanceNic) GetIP() string {
	return self.IpAddr
}

// GetIP6 is always "": the placeholder network (network.go) has no IPv6
// range, and cloudpods fails the whole NIC attach when it is asked to place
// a v6 address on a network without one. The v4 address and MAC still sync.
func (self *SInstanceNic) GetIP6() string {
	return ""
}

func (self *SInstanceNic) GetMAC() string {
	return strings.ToLower(self.Mac)
}

func (self *SInstanceNic) GetDriver() string {
	return self.DeviceModel
}

func (self *SInstanceNic) InClassicNetwork() bool {
	return false
}

// GetINetworkId points at the placeholder network of the wire for this
// NIC's vswitch in the VM's host pool: "vswitch/<vsName>/hostpool/<hpId>/default".
// A NIC without a vswitch (e.g. netType 2, "none") has no network.
func (self *SInstanceNic) GetINetworkId() string {
	if len(self.VsName) == 0 {
		return ""
	}
	return networkId(wireId(self.VsName, self.ins.HostPoolId))
}

func (self *SInstanceNic) GetSubAddress() ([]string, error) {
	return []string{}, nil
}
