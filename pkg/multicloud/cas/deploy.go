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

// Package cas implements a read-only (P0) cloudmux provider for H3C CAS
// (CVM/CVK) virtualization, talking to the CAS REST API under
// {AccessUrl}/cas/casrs/ with HTTP Digest authentication.
//
// This is not the H3C public/CloudOS provider (pkg/multicloud/h3c) and has
// nothing to do with the Keystone CAS SSO driver.
package cas

import (
	"strconv"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

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
	Memory       int                 `json:"memory,omitempty"` // UNVERIFIED unit; we send MB
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
		// UNVERIFIED split: api.yaml has both cpuSocket and cpuCore. We put
		// the requested vCPU count in cpuSocket and leave cpuCore=1.
		CpuSocket: opts.Cpu,
		CpuCore:   1,
		Memory:    opts.MemoryMB,
		UserData:  opts.UserData,
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

// DeployVM is POST /cas/casrs/vm/deploy. Returns the finished SMessage
// (targetId is the new domain id when CAS fills it).
func (cli *SCasClient) DeployVM(body *SDomainDeploy) (*SMessage, error) {
	// api.yaml parameter name is "domain"; JSON body is the deploy object
	// itself (swagger body params are typically unwrapped to the schema).
	return cli.doAsync(httputils.POST, "vm/deploy", nil, jsonutils.Marshal(body))
}

func (self *SHost) CreateVM(opts *cloudprovider.SManagedVMCreateConfig) (cloudprovider.ICloudVM, error) {
	body, err := buildDeployBody(self, opts)
	if err != nil {
		return nil, err
	}
	msg, err := self.zone.region.cli.DeployVM(body)
	if err != nil {
		return nil, errors.Wrapf(err, "DeployVM")
	}
	vm, err := self.zone.region.findDeployedVM(msg, body.DomainName)
	if err != nil {
		return nil, err
	}
	vm.host = self
	vm.region = self.zone.region
	return vm, nil
}

// findDeployedVM resolves the VM created by deploy. Prefer message.targetId
// (numeric domain id) via vm/detail/{id}; fall back to scanning the VM list
// by name. UNVERIFIED: whether targetId is always set on deploy completion,
// and whether domainName is unique.
func (self *SRegion) findDeployedVM(msg *SMessage, name string) (*SInstance, error) {
	if msg != nil && msg.TargetId > 0 {
		detail, err := self.cli.GetInstanceDetail(msg.TargetId)
		if err == nil && len(detail.Uuid) > 0 {
			vm, err := self.cli.GetInstance(detail.Uuid)
			if err != nil {
				return nil, err
			}
			vm.region = self
			return vm, nil
		}
	}
	vms, err := self.cli.GetInstances(nil)
	if err != nil {
		return nil, err
	}
	var match *SInstance
	for i := range vms {
		if vms[i].Name == name || vms[i].Title == name {
			if match != nil {
				return nil, errors.Errorf("multiple VMs named %q after deploy; targetId missing from task message", name)
			}
			match = &vms[i]
		}
	}
	if match == nil {
		tid := int64(0)
		if msg != nil {
			tid = msg.TargetId
		}
		return nil, errors.Errorf("deploy finished (targetId=%d) but VM %q not found in vm/vmList", tid, name)
	}
	match.region = self
	return match, nil
}
