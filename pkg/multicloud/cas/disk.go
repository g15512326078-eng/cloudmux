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
	"path"

	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SDisk is a "disk" device of VMRsDomainDetail.storage[]. CAS volumes have
// no id of their own here, so the id is {vm uuid}/{device name}.
type SDisk struct {
	multicloud.SDisk
	multicloud.STagBase

	SInstanceStorage

	ins     *SInstance
	index   int
	storage *SStorage
}

func (self *SDisk) GetId() string {
	return fmt.Sprintf("%s/%s", self.ins.Uuid, self.DeviceName)
}

func (self *SDisk) GetGlobalId() string {
	return self.GetId()
}

func (self *SDisk) GetName() string {
	return path.Base(self.Path)
}

func (self *SDisk) GetStatus() string {
	return api.DISK_READY
}

func (self *SDisk) Refresh() error {
	return nil
}

func (self *SDisk) IsEmulated() bool {
	return false
}

func (self *SDisk) GetIStorage() (cloudprovider.ICloudStorage, error) {
	if self.storage != nil {
		return self.storage, nil
	}
	host, err := self.ins.getHost()
	if err != nil {
		return nil, errors.Wrapf(err, "getHost")
	}
	storages, err := host.getStorages()
	if err != nil {
		return nil, errors.Wrapf(err, "getStorages")
	}
	storage := matchStorage(storages, self.Path)
	if storage == nil {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "no storage pool on host %s holds %s", host.Name, self.Path)
	}
	self.storage = storage
	return storage, nil
}

// matchStorage picks the pool with the longest path prefix of volPath.
func matchStorage(storages []SStorage, volPath string) *SStorage {
	var ret *SStorage
	for i := range storages {
		if storages[i].contains(volPath) && (ret == nil || len(storages[i].Path) > len(ret.Path)) {
			ret = &storages[i]
		}
	}
	return ret
}

func (self *SDisk) GetDiskFormat() string {
	return self.Format
}

func (self *SDisk) GetDiskSizeMB() int {
	return int(self.Size)
}

func (self *SDisk) GetDeviceName() string {
	return self.DeviceName
}

func (self *SDisk) GetIsAutoDelete() bool {
	return false
}

func (self *SDisk) GetTemplateId() string {
	return ""
}

// GetDiskType: the first "disk" device in the detail is taken as the system
// disk.
func (self *SDisk) GetDiskType() string {
	if self.index == 0 {
		return api.DISK_TYPE_SYS
	}
	return api.DISK_TYPE_DATA
}

func (self *SDisk) GetFsFormat() string {
	return ""
}

func (self *SDisk) GetIsNonPersistent() bool {
	return false
}

func (self *SDisk) GetDriver() string {
	return self.TargetBus
}

func (self *SDisk) GetCacheMode() string {
	return self.CacheType
}

func (self *SDisk) GetMountpoint() string {
	return ""
}

func (self *SDisk) GetAccessPath() string {
	return self.Path
}

func (self *SDisk) Delete(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "Delete")
}

func (self *SDisk) CreateISnapshot(ctx context.Context, name string, desc string) (cloudprovider.ICloudSnapshot, error) {
	return nil, errors.Wrap(cloudprovider.ErrNotImplemented, "CreateISnapshot")
}

func (self *SDisk) GetISnapshots() ([]cloudprovider.ICloudSnapshot, error) {
	return []cloudprovider.ICloudSnapshot{}, nil
}

func (self *SDisk) Resize(ctx context.Context, newSizeMB int64) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "Resize")
}

func (self *SDisk) Reset(ctx context.Context, snapshotId string) (string, error) {
	return "", errors.Wrap(cloudprovider.ErrNotImplemented, "Reset")
}

func (self *SDisk) Rebuild(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "Rebuild")
}
