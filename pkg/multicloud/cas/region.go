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

	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SRegion is a single pseudo region per CAS management endpoint; host pools
// are exposed as zones under it.
type SRegion struct {
	multicloud.SRegion
	multicloud.SNoObjectStorageRegion
	multicloud.SNoLbRegion

	cli *SCasClient
}

func (self *SRegion) GetClient() *SCasClient {
	return self.cli
}

func (self *SRegion) GetId() string {
	return self.cli.cpcfg.Id
}

func (self *SRegion) GetGlobalId() string {
	return fmt.Sprintf("%s/%s", CLOUD_PROVIDER_CAS, self.cli.cpcfg.Id)
}

func (self *SRegion) GetName() string {
	return self.cli.cpcfg.Name
}

func (self *SRegion) GetI18n() cloudprovider.SModelI18nTable {
	table := cloudprovider.SModelI18nTable{}
	table["name"] = cloudprovider.NewSModelI18nEntry(self.GetName()).CN(self.GetName())
	return table
}

func (self *SRegion) GetProvider() string {
	return CLOUD_PROVIDER_CAS
}

func (self *SRegion) GetCloudEnv() string {
	return ""
}

func (self *SRegion) GetStatus() string {
	return api.CLOUD_REGION_STATUS_INSERVER
}

func (self *SRegion) GetGeographicInfo() cloudprovider.SGeographicInfo {
	return cloudprovider.SGeographicInfo{}
}

func (self *SRegion) GetCapabilities() []string {
	return self.cli.GetCapabilities()
}

func (self *SRegion) Refresh() error {
	return nil
}

func (self *SRegion) IsEmulated() bool {
	return true
}

func (self *SRegion) CreateEIP(opts *cloudprovider.SEip) (cloudprovider.ICloudEIP, error) {
	return nil, cloudprovider.ErrNotSupported
}

func (self *SRegion) GetIEips() ([]cloudprovider.ICloudEIP, error) {
	return []cloudprovider.ICloudEIP{}, nil
}

func (self *SRegion) GetIEipById(id string) (cloudprovider.ICloudEIP, error) {
	return nil, cloudprovider.ErrNotFound
}

func (self *SRegion) GetISecurityGroupById(secgroupId string) (cloudprovider.ICloudSecurityGroup, error) {
	return nil, cloudprovider.ErrNotSupported
}

func (self *SRegion) CreateISecurityGroup(conf *cloudprovider.SecurityGroupCreateInput) (cloudprovider.ICloudSecurityGroup, error) {
	return nil, cloudprovider.ErrNotSupported
}

func (self *SRegion) CreateIVpc(opts *cloudprovider.VpcCreateOptions) (cloudprovider.ICloudVpc, error) {
	return nil, cloudprovider.ErrNotSupported
}

func (self *SRegion) GetIZones() ([]cloudprovider.ICloudZone, error) {
	pools, err := self.cli.GetHostPools()
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostPools")
	}
	ret := []cloudprovider.ICloudZone{}
	for i := range pools {
		ret = append(ret, &SZone{SHostPool: pools[i], region: self})
	}
	return ret, nil
}

func (self *SRegion) GetIZoneById(id string) (cloudprovider.ICloudZone, error) {
	zones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	for i := range zones {
		if zones[i].GetGlobalId() == id {
			return zones[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "zone %s", id)
}

func (self *SRegion) getZone(hostPoolId int64) (*SZone, error) {
	zone, err := self.GetIZoneById(fmt.Sprintf("%d", hostPoolId))
	if err != nil {
		return nil, err
	}
	return zone.(*SZone), nil
}

func (self *SRegion) GetIHosts() ([]cloudprovider.ICloudHost, error) {
	zones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudHost{}
	for i := range zones {
		hosts, err := zones[i].GetIHosts()
		if err != nil {
			return nil, errors.Wrapf(err, "zone %s GetIHosts", zones[i].GetName())
		}
		ret = append(ret, hosts...)
	}
	return ret, nil
}

func (self *SRegion) getHost(id string) (*SHost, error) {
	host, err := self.cli.GetHost(id)
	if err != nil {
		return nil, err
	}
	zone, err := self.getZone(host.HostPoolId)
	if err != nil {
		return nil, errors.Wrapf(err, "host %s zone %d", id, host.HostPoolId)
	}
	host.zone = zone
	return host, nil
}

func (self *SRegion) GetIHostById(id string) (cloudprovider.ICloudHost, error) {
	return self.getHost(id)
}

func (self *SRegion) GetIStorages() ([]cloudprovider.ICloudStorage, error) {
	zones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudStorage{}
	for i := range zones {
		storages, err := zones[i].GetIStorages()
		if err != nil {
			return nil, errors.Wrapf(err, "zone %s GetIStorages", zones[i].GetName())
		}
		ret = append(ret, storages...)
	}
	return ret, nil
}

func (self *SRegion) GetIStorageById(id string) (cloudprovider.ICloudStorage, error) {
	storages, err := self.GetIStorages()
	if err != nil {
		return nil, err
	}
	for i := range storages {
		if storages[i].GetGlobalId() == id {
			return storages[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "storage %s", id)
}

func (self *SRegion) GetIVMs() ([]cloudprovider.ICloudVM, error) {
	vms, err := self.cli.GetInstances(nil)
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudVM{}
	for i := range vms {
		vms[i].region = self
		ret = append(ret, &vms[i])
	}
	return ret, nil
}

func (self *SRegion) GetIVMById(id string) (cloudprovider.ICloudVM, error) {
	vm, err := self.cli.GetInstance(id)
	if err != nil {
		return nil, err
	}
	vm.region = self
	return vm, nil
}

func (self *SRegion) GetIVpcs() ([]cloudprovider.ICloudVpc, error) {
	return []cloudprovider.ICloudVpc{self.getVpc()}, nil
}

func (self *SRegion) GetIVpcById(id string) (cloudprovider.ICloudVpc, error) {
	vpc := self.getVpc()
	if vpc.GetGlobalId() != id {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "vpc %s", id)
	}
	return vpc, nil
}

func (self *SRegion) GetIStoragecaches() ([]cloudprovider.ICloudStoragecache, error) {
	return []cloudprovider.ICloudStoragecache{self.getStoragecache()}, nil
}

func (self *SRegion) GetIStoragecacheById(id string) (cloudprovider.ICloudStoragecache, error) {
	cache := self.getStoragecache()
	if cache.GetGlobalId() != id {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "storagecache %s", id)
	}
	return cache, nil
}
