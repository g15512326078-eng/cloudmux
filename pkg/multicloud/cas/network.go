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

	"yunion.io/x/pkg/util/netutils"
	"yunion.io/x/pkg/util/rbacscope"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SNetwork is the placeholder network of a wire, as in the nutanix provider
// for a VPC without IP pools: id "<wire id>/default", the VPC's CIDR
// (0.0.0.0/0) as range, emulated. Guest NICs on that vswitch in that host
// pool point at it via SInstanceNic.GetINetworkId.
type SNetwork struct {
	multicloud.SNetworkBase
	multicloud.STagBase

	wire *SWire
}

func networkId(wireId string) string {
	return fmt.Sprintf("%s/default", wireId)
}

func (self *SNetwork) GetId() string {
	return networkId(self.wire.GetId())
}

func (self *SNetwork) GetGlobalId() string {
	return self.GetId()
}

func (self *SNetwork) GetName() string {
	return self.wire.GetName()
}

func (self *SNetwork) GetStatus() string {
	return api.NETWORK_STATUS_AVAILABLE
}

func (self *SNetwork) Refresh() error {
	return nil
}

func (self *SNetwork) IsEmulated() bool {
	return true
}

func (self *SNetwork) GetIWire() cloudprovider.ICloudWire {
	return self.wire
}

func (self *SNetwork) prefix() netutils.IPV4Prefix {
	cidr := self.wire.vpc.GetCidrBlock()
	pref, err := netutils.NewIPV4Prefix(cidr)
	if err != nil {
		pref, _ = netutils.NewIPV4Prefix("0.0.0.0/0")
	}
	return pref
}

func (self *SNetwork) GetIpStart() string {
	return self.prefix().ToIPRange().StartIp().StepUp().String()
}

func (self *SNetwork) GetIpEnd() string {
	return self.prefix().ToIPRange().EndIp().StepDown().String()
}

func (self *SNetwork) GetIpMask() int8 {
	return self.prefix().MaskLen
}

func (self *SNetwork) GetGateway() string {
	return ""
}

func (self *SNetwork) GetServerType() string {
	return api.NETWORK_TYPE_GUEST
}

func (self *SNetwork) GetPublicScope() rbacscope.TRbacScope {
	return rbacscope.ScopeDomain
}

func (self *SNetwork) GetProjectId() string {
	return ""
}

func (self *SNetwork) GetAllocTimeoutSeconds() int {
	return 120
}

func (self *SNetwork) Delete() error {
	return cloudprovider.ErrNotSupported
}
