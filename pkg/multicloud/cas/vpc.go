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
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SVpc is a single pseudo VPC per account; CAS has no VPC concept.
type SVpc struct {
	multicloud.SVpc
	multicloud.STagBase

	region *SRegion
}

func (self *SRegion) getVpc() *SVpc {
	return &SVpc{region: self}
}

func (self *SVpc) GetId() string {
	return self.region.GetId()
}

func (self *SVpc) GetGlobalId() string {
	return self.region.GetGlobalId()
}

func (self *SVpc) GetName() string {
	return self.region.GetName()
}

func (self *SVpc) GetStatus() string {
	return api.VPC_STATUS_AVAILABLE
}

func (self *SVpc) Refresh() error {
	return nil
}

func (self *SVpc) IsEmulated() bool {
	return true
}

func (self *SVpc) GetRegion() cloudprovider.ICloudRegion {
	return self.region
}

func (self *SVpc) GetIsDefault() bool {
	return true
}

func (self *SVpc) GetCidrBlock() string {
	return "0.0.0.0/0"
}

func (self *SVpc) GetISecurityGroups() ([]cloudprovider.ICloudSecurityGroup, error) {
	return []cloudprovider.ICloudSecurityGroup{}, nil
}

func (self *SVpc) GetIRouteTables() ([]cloudprovider.ICloudRouteTable, error) {
	return []cloudprovider.ICloudRouteTable{}, nil
}

func (self *SVpc) GetIRouteTableById(routeTableId string) (cloudprovider.ICloudRouteTable, error) {
	return nil, cloudprovider.ErrNotFound
}

func (self *SVpc) Delete() error {
	return cloudprovider.ErrNotSupported
}

// GetIWires returns one wire per (host pool, vswitch name).
func (self *SVpc) GetIWires() ([]cloudprovider.ICloudWire, error) {
	zones, err := self.region.GetIZones()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudWire{}
	seen := map[string]bool{}
	for i := range zones {
		hosts, err := zones[i].(*SZone).getHosts()
		if err != nil {
			return nil, err
		}
		for j := range hosts {
			wires, err := hosts[j].getIWires()
			if err != nil {
				return nil, errors.Wrapf(err, "host %s wires", hosts[j].Name)
			}
			for k := range wires {
				if seen[wires[k].GetGlobalId()] {
					continue
				}
				seen[wires[k].GetGlobalId()] = true
				ret = append(ret, wires[k])
			}
		}
	}
	return ret, nil
}

func (self *SVpc) GetIWireById(id string) (cloudprovider.ICloudWire, error) {
	wires, err := self.GetIWires()
	if err != nil {
		return nil, err
	}
	for i := range wires {
		if wires[i].GetGlobalId() == id {
			return wires[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "wire %s", id)
}
