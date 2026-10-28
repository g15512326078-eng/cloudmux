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

// SVSwitch is HostRsVSwitch, one element of
// GET /cas/casrs/host/id/{hostId}/vswitch (key "vSwitch").
type SVSwitch struct {
	Id        int64  `json:"id"`
	HostId    int64  `json:"hostId"`
	Name      string `json:"name"`
	PortNum   int    `json:"portNum"`
	Mode      int    `json:"mode"`
	Pnic      string `json:"pnic"`
	Address   string `json:"address"`
	Netmask   string `json:"netmask"`
	Gateway   string `json:"gateway"`
	Status    int    `json:"status"`
	IsManage  int    `json:"isManage"`
	Mtu       int    `json:"mtu"`
	VlanId    int    `json:"vlanId"`
	ClusterId int64  `json:"clusterId"`
	Flag      int    `json:"flag"` // 0 added on host, 1 added on cluster
}

func (cli *SCasClient) GetHostVSwitches(hostId int64) ([]SVSwitch, error) {
	ret := []SVSwitch{}
	return ret, cli.list(fmt.Sprintf("host/id/%d/vswitch", hostId), nil, "vSwitch", &ret)
}

// SWire is one (VPC, zone) pair: the vswitches named vpc.VSwitchName on the
// hosts of one host pool.
type SWire struct {
	multicloud.SResourceBase
	multicloud.STagBase

	vpc  *SVpc
	zone *SZone
}

// wireId is shared with SInstanceNic.GetINetworkId.
func wireId(vswitchName string, hostPoolId int64) string {
	return fmt.Sprintf("vswitch/%s/hostpool/%d", vswitchName, hostPoolId)
}

func (self *SWire) GetId() string {
	return wireId(self.vpc.VSwitchName, self.zone.Id)
}

func (self *SWire) GetGlobalId() string {
	return self.GetId()
}

func (self *SWire) GetName() string {
	return fmt.Sprintf("%s-%s", self.zone.Name, self.vpc.VSwitchName)
}

func (self *SWire) GetStatus() string {
	return api.WIRE_STATUS_AVAILABLE
}

func (self *SWire) IsEmulated() bool {
	return true
}

func (self *SWire) GetIVpc() cloudprovider.ICloudVpc {
	return self.vpc
}

func (self *SWire) GetIZone() cloudprovider.ICloudZone {
	return self.zone
}

func (self *SWire) GetBandwidth() int {
	return 10000
}

// GetINetworks returns the wire's single placeholder network (see
// network.go); CAS vswitches carry no guest IP pool.
func (self *SWire) GetINetworks() ([]cloudprovider.ICloudNetwork, error) {
	return []cloudprovider.ICloudNetwork{&SNetwork{wire: self}}, nil
}

func (self *SWire) GetINetworkById(netid string) (cloudprovider.ICloudNetwork, error) {
	network := &SNetwork{wire: self}
	if network.GetGlobalId() != netid {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "network %s", netid)
	}
	return network, nil
}

func (self *SWire) CreateINetwork(opts *cloudprovider.SNetworkCreateOptions) (cloudprovider.ICloudNetwork, error) {
	return nil, cloudprovider.ErrNotSupported
}
