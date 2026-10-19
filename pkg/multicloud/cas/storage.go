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
	"net/url"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SStoragePool is one element of GET /cas/casrs/storage/pool?hostId=
// (key "storagePool"). Sizes are MB. CAS storage pools have no id of their
// own; they are named per host.
type SStoragePool struct {
	Name       string  `json:"name"`
	Title      string  `json:"title"`
	Type       string  `json:"type"` // dir, netfs, fs, cifs, iscsi, ...
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	TotalSize  int64   `json:"totalSize"`
	FreeSize   int64   `json:"freeSize"`
	Allocation float64 `json:"allocation"`
}

func (cli *SCasClient) GetStoragePools(hostId int64) ([]SStoragePool, error) {
	params := url.Values{}
	params.Set("hostId", fmt.Sprintf("%d", hostId))
	ret := []SStoragePool{}
	return ret, cli.list("storage/pool", params, "storagePool", &ret)
}

// GetSharedStoragePools is GET /cas/casrs/host/id/{hostId}/storage, which
// CAS documents as "all shared storage of the host".
func (cli *SCasClient) GetSharedStoragePools(hostId int64) ([]SStoragePool, error) {
	ret := []SStoragePool{}
	return ret, cli.list(fmt.Sprintf("host/id/%d/storage", hostId), nil, "storagePool", &ret)
}

type SStorage struct {
	multicloud.SStorageBase
	multicloud.STagBase

	SStoragePool

	host   *SHost
	shared bool
}

func (self *SHost) getStorages() ([]SStorage, error) {
	pools, shared, err := self.zone.region.cachedHostStorages(self.Id)
	if err != nil {
		return nil, err
	}
	return newStorages(self, pools, shared), nil
}

// fetchStorages is getStorages without the per-sync cache, for Refresh.
func (self *SHost) fetchStorages() ([]SStorage, error) {
	cli := self.zone.region.cli
	pools, err := cli.GetStoragePools(self.Id)
	if err != nil {
		return nil, errors.Wrapf(err, "GetStoragePools")
	}
	shared, err := cli.GetSharedStoragePools(self.Id)
	if err != nil {
		return nil, errors.Wrapf(err, "GetSharedStoragePools")
	}
	return newStorages(self, pools, shared), nil
}

func newStorages(host *SHost, pools, shared []SStoragePool) []SStorage {
	sharedNames := map[string]bool{}
	for i := range shared {
		sharedNames[shared[i].Name] = true
	}
	ret := []SStorage{}
	for i := range pools {
		ret = append(ret, SStorage{
			SStoragePool: pools[i],
			host:         host,
			shared:       sharedNames[pools[i].Name],
		})
	}
	return ret
}

func (self *SStorage) GetName() string {
	if self.shared {
		return self.Name
	}
	return fmt.Sprintf("%s-%s", self.host.Name, self.Name)
}

// GetGlobalId: a shared pool is assumed to have the same name on every
// host of the host pool and is keyed by host pool; a local pool is keyed
// by host.
func (self *SStorage) GetId() string {
	if self.shared {
		return fmt.Sprintf("hostpool/%d/%s", self.host.HostPoolId, self.Name)
	}
	return fmt.Sprintf("host/%d/%s", self.host.Id, self.Name)
}

func (self *SStorage) GetGlobalId() string {
	return self.GetId()
}

func (self *SStorage) GetStatus() string {
	if self.Status == 1 {
		return api.STORAGE_ONLINE
	}
	return api.STORAGE_OFFLINE
}

func (self *SStorage) Refresh() error {
	storages, err := self.host.fetchStorages()
	if err != nil {
		return err
	}
	for i := range storages {
		if storages[i].GetGlobalId() == self.GetGlobalId() {
			return jsonutils.Update(self, &storages[i])
		}
	}
	return errors.Wrapf(cloudprovider.ErrNotFound, "storage %s", self.GetGlobalId())
}

func (self *SStorage) IsEmulated() bool {
	return false
}

func (self *SStorage) GetIZone() cloudprovider.ICloudZone {
	return self.host.zone
}

func (self *SStorage) GetIStoragecache() cloudprovider.ICloudStoragecache {
	return self.host.zone.region.getStoragecache()
}

// GetStorageType maps the CAS pool type. Only dir/netfs/cifs have an
// obvious cloudpods counterpart; other types are passed through verbatim.
func (self *SStorage) GetStorageType() string {
	switch strings.ToLower(self.Type) {
	case "dir":
		return api.STORAGE_LOCAL
	case "netfs":
		return api.STORAGE_NFS
	case "cifs":
		return api.STORAGE_CIFS
	}
	return strings.ToLower(self.Type)
}

func (self *SStorage) GetMediumType() string {
	return api.DISK_TYPE_HYBRID
}

func (self *SStorage) GetCapacityMB() int64 {
	return self.TotalSize
}

func (self *SStorage) GetCapacityUsedMB() int64 {
	return self.TotalSize - self.FreeSize
}

func (self *SStorage) GetStorageConf() jsonutils.JSONObject {
	conf := jsonutils.NewDict()
	conf.Add(jsonutils.NewString(self.Path), "path")
	conf.Add(jsonutils.NewString(self.Type), "cas_type")
	return conf
}

func (self *SStorage) GetEnabled() bool {
	return true
}

func (self *SStorage) GetMountPoint() string {
	return self.Path
}

func (self *SStorage) IsSysDiskStore() bool {
	return true
}

func (self *SStorage) CreateIDisk(conf *cloudprovider.DiskCreateConfig) (cloudprovider.ICloudDisk, error) {
	return nil, errors.Wrap(cloudprovider.ErrNotImplemented, "CreateIDisk")
}

// contains reports whether a volume path lives in this pool.
func (self *SStorage) contains(path string) bool {
	if len(self.Path) == 0 {
		return false
	}
	return strings.HasPrefix(path, strings.TrimSuffix(self.Path, "/")+"/")
}

// GetIDisks walks the VMs that can use this pool (the host for a local pool,
// the host pool for a shared one) and keeps the disks whose path is in it.
func (self *SStorage) GetIDisks() ([]cloudprovider.ICloudDisk, error) {
	region := self.host.zone.region
	vms, err := region.cachedInstances()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudDisk{}
	for i := range vms {
		if self.shared && vms[i].HostPoolId != self.host.HostPoolId {
			continue
		}
		if !self.shared && vms[i].HostId != self.host.Id {
			continue
		}
		disks, err := vms[i].getDisks()
		if err != nil {
			return nil, errors.Wrapf(err, "vm %s disks", vms[i].Name)
		}
		for j := range disks {
			if self.contains(disks[j].Path) {
				disks[j].storage = self
				ret = append(ret, &disks[j])
			}
		}
	}
	return ret, nil
}

func (self *SStorage) GetIDiskById(id string) (cloudprovider.ICloudDisk, error) {
	disks, err := self.GetIDisks()
	if err != nil {
		return nil, err
	}
	for i := range disks {
		if disks[i].GetGlobalId() == id {
			return disks[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "disk %s", id)
}
