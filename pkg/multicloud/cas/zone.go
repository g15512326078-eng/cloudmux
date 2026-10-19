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

// SHostPool is one element of GET /cas/casrs/hostpool/all (key "hostPool").
type SHostPool struct {
	Id                int64  `json:"id"`
	Name              string `json:"name"`
	OperatorGroupId   int64  `json:"operatorGroupId"`
	OperatorGroupCode string `json:"operatorGroupCode"`
	ChildNum          int64  `json:"childNum"`
}

func (cli *SCasClient) GetHostPools() ([]SHostPool, error) {
	ret := []SHostPool{}
	return ret, cli.list("hostpool/all", nil, "hostPool", &ret)
}

// SZone maps a CAS host pool to a zone.
type SZone struct {
	multicloud.SResourceBase
	multicloud.STagBase

	SHostPool
	region *SRegion
}

func (self *SZone) GetId() string {
	return fmt.Sprintf("%d", self.Id)
}

func (self *SZone) GetGlobalId() string {
	return self.GetId()
}

func (self *SZone) GetName() string {
	return self.Name
}

func (self *SZone) GetI18n() cloudprovider.SModelI18nTable {
	table := cloudprovider.SModelI18nTable{}
	table["name"] = cloudprovider.NewSModelI18nEntry(self.GetName()).CN(self.GetName())
	return table
}

func (self *SZone) GetStatus() string {
	return api.ZONE_ENABLE
}

func (self *SZone) GetIRegion() cloudprovider.ICloudRegion {
	return self.region
}

func (self *SZone) getHosts() ([]SHost, error) {
	hosts, err := self.region.cli.GetHosts()
	if err != nil {
		return nil, err
	}
	ret := []SHost{}
	for i := range hosts {
		if hosts[i].HostPoolId != self.Id {
			continue
		}
		hosts[i].zone = self
		ret = append(ret, hosts[i])
	}
	return ret, nil
}

func (self *SZone) GetIHosts() ([]cloudprovider.ICloudHost, error) {
	hosts, err := self.getHosts()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudHost{}
	for i := range hosts {
		ret = append(ret, &hosts[i])
	}
	return ret, nil
}

func (self *SZone) GetIHostById(id string) (cloudprovider.ICloudHost, error) {
	host, err := self.region.cli.GetHost(id)
	if err != nil {
		return nil, err
	}
	if host.HostPoolId != self.Id {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "host %s not in hostpool %d", id, self.Id)
	}
	host.zone = self
	return host, nil
}

// GetIStorages returns the storage pools of every host in the pool, with
// shared pools de-duplicated (see SStorage.GetGlobalId).
func (self *SZone) GetIStorages() ([]cloudprovider.ICloudStorage, error) {
	hosts, err := self.getHosts()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudStorage{}
	seen := map[string]bool{}
	for i := range hosts {
		storages, err := hosts[i].getStorages()
		if err != nil {
			return nil, errors.Wrapf(err, "host %s storages", hosts[i].Name)
		}
		for j := range storages {
			id := storages[j].GetGlobalId()
			if seen[id] {
				continue
			}
			seen[id] = true
			ret = append(ret, &storages[j])
		}
	}
	return ret, nil
}

func (self *SZone) GetIStorageById(id string) (cloudprovider.ICloudStorage, error) {
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
