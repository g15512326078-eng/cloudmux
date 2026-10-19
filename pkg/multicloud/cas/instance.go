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
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

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
	Type         int     `json:"type"` // 0 CAS, 1 VMware
}

// SInstanceDetail is the subset of VMRsDomainDetail used for P0.
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

// GetInstances pages through vm/vmList (see listPaged) and returns every VM.
// VM_TYPE_VMWARE is vmList "type" 1 ("VMware虚拟机"), a VM CAS manages on a
// VMware host; those are not CAS-hypervisor guests and are not synced.
const VM_TYPE_VMWARE = 1

// GetInstances is the paged vm/vmList without VMware-type VMs.
func (cli *SCasClient) GetInstances(params url.Values) ([]SInstance, error) {
	vms, err := cli.listInstances(params)
	if err != nil {
		return nil, err
	}
	ret := []SInstance{}
	for i := range vms {
		if vms[i].Type != VM_TYPE_VMWARE {
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

// GetInstance looks a VM up by uuid (its external id):
//  1. GET vm/list/detail?vmUuids=<uuid>: no element -> ErrNotFound;
//  2. GET vm/vmList?domainIdList=<id> for the list fields (hostId, ...);
//  3. if the filter was not honoured, fall back to the full paged list.
//
// If the detail exists but the list does not have it, that is an error,
// not ErrNotFound, so cloudpods does not mark the VM lost.
func (cli *SCasClient) GetInstance(uuid string) (*SInstance, error) {
	params := url.Values{}
	params.Set("vmUuids", uuid)
	resp, err := cli.request(httputils.GET, "vm/list/detail", params, nil)
	if err != nil {
		return nil, err
	}
	details, err := parseInstanceDetails(resp)
	if err != nil {
		return nil, err
	}
	var detail *SInstanceDetail
	for i := range details {
		if details[i].Uuid == uuid {
			detail = &details[i]
		}
	}
	if detail == nil {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "vm %s", uuid)
	}
	var notFound error
	find := func(vms []SInstance) *SInstance {
		for i := range vms {
			if vms[i].Uuid == uuid {
				if vms[i].Type == VM_TYPE_VMWARE {
					notFound = errors.Wrapf(cloudprovider.ErrNotFound, "vm %s is a VMware VM", uuid)
					return nil
				}
				vms[i].detail = detail
				return &vms[i]
			}
		}
		return nil
	}
	params = url.Values{}
	params.Set("domainIdList", fmt.Sprintf("%d", detail.Id))
	vms := []SInstance{}
	err = cli.list("vm/vmList", params, "domain", &vms)
	if err != nil {
		return nil, err
	}
	if vm := find(vms); vm != nil || notFound != nil {
		return vm, notFound
	}
	vms, err = cli.listInstances(nil)
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

func (self *SInstance) Refresh() error {
	vm, err := self.region.cli.GetInstance(self.Uuid)
	if err != nil {
		return err
	}
	self.detail = nil
	return jsonutils.Update(self, vm)
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

// Writes below are P1: CAS answers most of them with an async RsTaskMsg
// whose semantics are not yet verified, so they are not implemented.

func (self *SInstance) StartVM(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "StartVM")
}

func (self *SInstance) StopVM(ctx context.Context, opts *cloudprovider.ServerStopOptions) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "StopVM")
}

func (self *SInstance) DeleteVM(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "DeleteVM")
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

func (self *SInstance) GetVNCInfo(input *cloudprovider.ServerVncInput) (*cloudprovider.ServerVncOutput, error) {
	return nil, errors.Wrap(cloudprovider.ErrNotImplemented, "GetVNCInfo")
}

func (self *SInstance) AttachDisk(ctx context.Context, diskId string) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "AttachDisk")
}

func (self *SInstance) DetachDisk(ctx context.Context, diskId string) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "DetachDisk")
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

func (self *SInstanceNic) GetIP6() string {
	return self.Ipv6
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
