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
	"fmt"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SHostBrief is one element of GET /cas/casrs/host (key "host"). It lacks
// hostPoolId and resource figures, so the detail call is needed as well.
type SHostBrief struct {
	Type      string `json:"type"`
	Id        int64  `json:"id"`
	ClusterId int64  `json:"clusterId"`
	Name      string `json:"name"`
	Ip        string `json:"ip"`
	HostType  int    `json:"hostType"`
	Status    int    `json:"status"`
}

// SHost is GET /cas/casrs/host/id/{id}.
type SHost struct {
	multicloud.SHostBase
	multicloud.STagBase

	zone *SZone

	Type         string `json:"type"`
	Id           int64  `json:"id"`
	HostPoolId   int64  `json:"hostPoolId"`
	ClusterId    int64  `json:"clusterId"`
	Name         string `json:"name"`
	Ip           string `json:"ip"`
	Mask         string `json:"mask"`
	Model        string `json:"model"`
	Vendor       string `json:"vendor"`
	CpuCount     int    `json:"cpuCount"`
	CpuModel     string `json:"cpuModel"`
	HostType     int    `json:"hostType"`
	CpuFrequence int    `json:"cpuFrequence"` // MHz
	DiskSize     int64  `json:"diskSize"`     // MB
	MemorySize   int64  `json:"memorySize"`   // MB
	Status       int    `json:"status"`
	CpuSockets   int    `json:"cpuSockets"`
	CpuCores     int    `json:"cpuCores"`
	MaintainMode int    `json:"maintainMode"`
	SerialNumber string `json:"serialNumber"`
	CvkMaintain  int    `json:"cvkMaintain"`
	ILOs         string `json:"iLOs"`
}

func (cli *SCasClient) GetHostBriefs() ([]SHostBrief, error) {
	ret := []SHostBrief{}
	return ret, cli.list("host", nil, "host", &ret)
}

func (cli *SCasClient) GetHost(id string) (*SHost, error) {
	host := &SHost{}
	err := cli.get(fmt.Sprintf("host/id/%s", id), nil, host)
	if err != nil {
		return nil, err
	}
	if host.Id == 0 {
		return nil, errors.Wrapf(ErrEmptyBody, "host/id/%s returned no host id", id)
	}
	return host, nil
}

// GetHosts lists hosts and fetches the detail of each one (N+1 requests).
func (cli *SCasClient) GetHosts() ([]SHost, error) {
	briefs, err := cli.GetHostBriefs()
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostBriefs")
	}
	ret := []SHost{}
	for i := range briefs {
		host, err := cli.GetHost(fmt.Sprintf("%d", briefs[i].Id))
		if err != nil {
			return nil, errors.Wrapf(err, "GetHost %d", briefs[i].Id)
		}
		ret = append(ret, *host)
	}
	return ret, nil
}

func (self *SHost) GetId() string {
	return fmt.Sprintf("%d", self.Id)
}

func (self *SHost) GetGlobalId() string {
	return self.GetId()
}

func (self *SHost) GetName() string {
	return self.Name
}

func (self *SHost) GetStatus() string {
	return api.HOST_STATUS_RUNNING
}

func (self *SHost) Refresh() error {
	host, err := self.zone.region.cli.GetHost(self.GetId())
	if err != nil {
		return err
	}
	return jsonutils.Update(self, host)
}

func (self *SHost) IsEmulated() bool {
	return false
}

func (self *SHost) GetEnabled() bool {
	return true
}

// GetHostStatus: CAS documents status 1 as normal, 0 as abnormal.
func (self *SHost) GetHostStatus() string {
	if self.Status == 1 {
		return api.HOST_ONLINE
	}
	return api.HOST_OFFLINE
}

func (self *SHost) GetAccessIp() string {
	return self.Ip
}

func (self *SHost) GetAccessMac() string {
	return ""
}

func (self *SHost) GetSysInfo() jsonutils.JSONObject {
	info := jsonutils.NewDict()
	info.Add(jsonutils.NewString(self.Vendor), "manufacture")
	info.Add(jsonutils.NewString(self.Model), "model")
	return info
}

func (self *SHost) GetSN() string {
	return self.SerialNumber
}

// GetCpuCount returns cpuCount ("CPU总个数"); in the api.yaml example it is
// sockets*cores*2, i.e. logical threads.
func (self *SHost) GetCpuCount() int {
	return self.CpuCount
}

func (self *SHost) GetNodeCount() int8 {
	return int8(self.CpuSockets)
}

func (self *SHost) GetCpuDesc() string {
	return self.CpuModel
}

func (self *SHost) GetCpuMhz() int {
	return self.CpuFrequence
}

func (self *SHost) GetMemSizeMB() int {
	return int(self.MemorySize)
}

func (self *SHost) GetStorageSizeMB() int64 {
	return self.DiskSize
}

func (self *SHost) GetStorageType() string {
	return api.DISK_TYPE_HYBRID
}

func (self *SHost) GetHostType() string {
	return api.HOST_TYPE_CAS
}

func (self *SHost) GetIsMaintenance() bool {
	return self.CvkMaintain == 1
}

func (self *SHost) GetVersion() string {
	return ""
}

// getWires returns one wire per distinct vswitch name on the host, in the
// host's pool. vpcs shares VPC objects between hosts (may be nil).
func (self *SHost) getWires(vpcs map[string]*SVpc) ([]*SWire, error) {
	vswitches, err := self.zone.region.cachedHostVSwitches(self.Id)
	if err != nil {
		return nil, err
	}
	if vpcs == nil {
		vpcs = map[string]*SVpc{}
	}
	ret := []*SWire{}
	seen := map[string]bool{}
	for i := range vswitches {
		name := vswitches[i].Name
		if len(name) == 0 || seen[name] {
			continue
		}
		seen[name] = true
		vpc, ok := vpcs[name]
		if !ok {
			vpc = &SVpc{region: self.zone.region, VSwitchName: name}
			vpcs[name] = vpc
		}
		ret = append(ret, &SWire{vpc: vpc, zone: self.zone})
	}
	return ret, nil
}

func (self *SHost) getIWires() ([]cloudprovider.ICloudWire, error) {
	wires, err := self.getWires(nil)
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudWire{}
	for i := range wires {
		ret = append(ret, wires[i])
	}
	return ret, nil
}

func (self *SHost) GetIHostNics() ([]cloudprovider.ICloudHostNetInterface, error) {
	wires, err := self.getIWires()
	if err != nil {
		return nil, errors.Wrapf(err, "getIWires")
	}
	return cloudprovider.GetHostNetifs(self, wires), nil
}

func (self *SHost) GetIStorages() ([]cloudprovider.ICloudStorage, error) {
	storages, err := self.getStorages()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudStorage{}
	for i := range storages {
		ret = append(ret, &storages[i])
	}
	return ret, nil
}

func (self *SHost) GetIStorageById(id string) (cloudprovider.ICloudStorage, error) {
	storages, err := self.getStorages()
	if err != nil {
		return nil, err
	}
	for i := range storages {
		if storages[i].GetGlobalId() == id {
			return &storages[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "storage %s on host %s", id, self.Name)
}

// GetIVMs filters the region's cached, paged VM list by hostId.
func (self *SHost) GetIVMs() ([]cloudprovider.ICloudVM, error) {
	vms, err := self.zone.region.cachedInstances()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudVM{}
	for i := range vms {
		if vms[i].HostId != self.Id {
			continue
		}
		vms[i].host = self
		ret = append(ret, &vms[i])
	}
	return ret, nil
}

func (self *SHost) GetIVMById(id string) (cloudprovider.ICloudVM, error) {
	vm, err := self.zone.region.cli.GetInstance(id)
	if err != nil {
		return nil, err
	}
	if vm.HostId != self.Id {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "vm %s is on host %d, not %d", id, vm.HostId, self.Id)
	}
	vm.region = self.zone.region
	vm.host = self
	return vm, nil
}

// CreateVM is not supported yet.
func (self *SHost) CreateVM(opts *cloudprovider.SManagedVMCreateConfig) (cloudprovider.ICloudVM, error) {
	return nil, cloudprovider.ErrNotImplemented
}
