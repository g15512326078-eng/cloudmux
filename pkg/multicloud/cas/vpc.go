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

// SVpc is a pseudo VPC per vswitch name (nutanix style: the vswitch is the
// L2 segment). CAS has no VPC concept and vswitches are per host, so all
// vswitches with the same name in the account form one VPC; each host pool
// that has it contributes one wire (see SWire), each wire one placeholder
// network (see SNetwork).
type SVpc struct {
	multicloud.SVpc
	multicloud.STagBase

	region *SRegion

	VSwitchName string
}

func (self *SVpc) GetId() string {
	return fmt.Sprintf("vswitch/%s", self.VSwitchName)
}

func (self *SVpc) GetGlobalId() string {
	return self.GetId()
}

func (self *SVpc) GetName() string {
	return self.VSwitchName
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
	return false
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

// GetIWires returns one wire per host pool (zone) in which at least one
// host has a vswitch with this VPC's name.
func (self *SVpc) GetIWires() ([]cloudprovider.ICloudWire, error) {
	wires, err := self.region.getWires()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudWire{}
	for i := range wires {
		if wires[i].vpc.VSwitchName == self.VSwitchName {
			ret = append(ret, wires[i])
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

// getWires walks every host's (cached) vswitch list and returns one wire per
// (vswitch name, host pool), each pointing to the VPC of that name.
func (self *SRegion) getWires() ([]*SWire, error) {
	zones, err := self.GetIZones()
	if err != nil {
		return nil, err
	}
	vpcs := map[string]*SVpc{}
	ret := []*SWire{}
	seen := map[string]bool{}
	for i := range zones {
		zone := zones[i].(*SZone)
		hosts, err := zone.getHosts()
		if err != nil {
			return nil, errors.Wrapf(err, "zone %s hosts", zone.Name)
		}
		for j := range hosts {
			wires, err := hosts[j].getWires(vpcs)
			if err != nil {
				return nil, errors.Wrapf(err, "host %s wires", hosts[j].Name)
			}
			for _, wire := range wires {
				if !seen[wire.GetGlobalId()] {
					seen[wire.GetGlobalId()] = true
					ret = append(ret, wire)
				}
			}
		}
	}
	return ret, nil
}

func (self *SRegion) getVpcs() ([]*SVpc, error) {
	wires, err := self.getWires()
	if err != nil {
		return nil, err
	}
	ret := []*SVpc{}
	seen := map[string]bool{}
	for i := range wires {
		if !seen[wires[i].vpc.VSwitchName] {
			seen[wires[i].vpc.VSwitchName] = true
			ret = append(ret, wires[i].vpc)
		}
	}
	return ret, nil
}
