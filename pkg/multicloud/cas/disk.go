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
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SDisk is either a "disk" device of VMRsDomainDetail.storage[] (ins set)
// or an unattached volume file found in a storage pool (ins nil).
type SDisk struct {
	multicloud.SDisk
	multicloud.STagBase

	SInstanceStorage

	ins     *SInstance
	index   int // position among the VM's disks; -1 when unattached
	storage *SStorage
}

// GetId is diskId(storage, file name) — stable across attach/detach. A
// VM disk whose pool cannot be resolved (definitive ErrNotFound) falls
// back to the legacy {vm uuid}/{device name}. Transient lookup errors
// must not flip the id: callers that need a stable id (GetIDisks) resolve
// storage first and surface those errors instead of calling GetId blind.
func (self *SDisk) GetId() string {
	if self.storage != nil {
		return diskId(self.storage.GetGlobalId(), path.Base(self.Path))
	}
	if storage, err := self.getStorage(); err == nil {
		return diskId(storage.GetGlobalId(), path.Base(self.Path))
	} else if errors.Cause(err) != cloudprovider.ErrNotFound {
		// Transient lookup failure: never flip to the legacy {uuid}/{dev}
		// id (that churns synced records). Use the volume path as a
		// stand-in; GetIDisks surfaces the error before GetId is relied on.
		return self.Path
	}
	if self.ins != nil {
		return fmt.Sprintf("%s/%s", self.ins.Uuid, self.DeviceName)
	}
	return self.Path
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
	if self.ins == nil {
		storage, err := self.getStorage()
		if err != nil {
			return err
		}
		vol, err := storage.host.zone.region.cli.GetStorageVolume(storage.host.Id, storage.Name, path.Base(self.Path))
		if err != nil {
			return err
		}
		self.Size = int64(vol.Size)
		self.Format = vol.Format
		return nil
	}
	return nil
}

func (self *SDisk) IsEmulated() bool {
	return false
}

func (self *SDisk) getStorage() (*SStorage, error) {
	if self.storage != nil {
		return self.storage, nil
	}
	if self.ins == nil {
		return nil, errors.Wrapf(cloudprovider.ErrNotFound, "volume %s has no storage", self.Path)
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

func (self *SDisk) GetIStorage() (cloudprovider.ICloudStorage, error) {
	return self.getStorage()
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

// GetDiskType: the first "disk" device of a VM is its system disk; every
// other disk, and every unattached volume, is a data disk.
func (self *SDisk) GetDiskType() string {
	if self.ins != nil && self.index == 0 {
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

// Delete removes an unattached volume file with
// DELETE storage/volume/delete/{hostId}/{pool}/{vol}?isWipeVolume=false.
// It refuses a disk attached to a VM (detach it first) and re-reads the
// volume first so a volume attached since the last sync is not removed.
// The file is deleted but not wiped.
func (self *SDisk) Delete(ctx context.Context) error {
	if self.ins != nil {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "disk %s is attached to vm %s; detach it before deleting", self.GetGlobalId(), self.ins.GetName())
	}
	storage, err := self.getStorage()
	if err != nil {
		return err
	}
	cli := storage.host.zone.region.cli.WithContext(ctx)
	volName := path.Base(self.Path)
	vol, err := cli.GetStorageVolume(storage.host.Id, storage.Name, volName)
	if err != nil {
		if errors.Cause(err) == cloudprovider.ErrNotFound {
			return nil
		}
		return errors.Wrapf(err, "GetStorageVolume")
	}
	if vol.isAttached() {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "volume %s is used by %s", volName, vol.Users)
	}
	return cli.DeleteStorageVolume(storage.host.Id, storage.Name, volName)
}

// CAS snapshots are whole-VM (POST /vm/snapshot); there is no per-disk
// snapshot API, so disk snapshots are not offered. See snapshot.go for the
// instance-snapshot implementation.
func (self *SDisk) CreateISnapshot(ctx context.Context, name string, desc string) (cloudprovider.ICloudSnapshot, error) {
	return nil, errors.Wrap(cloudprovider.ErrNotSupported, "CAS has VM snapshots only; use instance snapshots")
}

func (self *SDisk) GetISnapshots() ([]cloudprovider.ICloudSnapshot, error) {
	return []cloudprovider.ICloudSnapshot{}, nil
}

// SVmModifyStorage is VMModifyStorageConfig in the body of PUT /vm/modify.
type SVmModifyStorage struct {
	Device     string `json:"device"`
	DeviceName string `json:"deviceName"`
	Format     string `json:"format"`
	Size       int64  `json:"size"` // MB, like vm/detail storage[].size
	CacheType  string `json:"cacheType"`
}

type SVmModifyStorageBody struct {
	Id      int64            `json:"id"`
	Storage SVmModifyStorage `json:"storage"`
}

// Resize grows an attached disk with PUT /cas/casrs/vm/modify
// (RsDomainModifyBasic.storage: device, format, size, cacheType are
// required), waits for the task, then re-reads the VM detail and fails if
// the disk did not reach the new size. It never shrinks: a smaller size
// is rejected, the same size is a no-op. An unattached volume cannot be
// resized (no non-cinder API for that).
func (self *SDisk) Resize(ctx context.Context, newSizeMB int64) error {
	if self.ins == nil {
		return errors.Wrap(cloudprovider.ErrNotSupported, "resize of an unattached CAS volume; attach it to a VM first")
	}
	if newSizeMB < self.Size {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "shrinking disk %s from %d MB to %d MB is not allowed", self.DeviceName, self.Size, newSizeMB)
	}
	if newSizeMB == self.Size {
		return nil
	}
	cli := self.ins.region.cli.WithContext(ctx)
	body := SVmModifyStorageBody{
		Id: self.ins.Id,
		Storage: SVmModifyStorage{
			Device:     "disk",
			DeviceName: self.DeviceName,
			Format:     self.Format,
			Size:       newSizeMB,
			CacheType:  self.CacheType,
		},
	}
	if _, err := cli.doAsync(httputils.PUT, "vm/modify", nil, jsonutils.Marshal(body)); err != nil {
		return errors.Wrapf(err, "vm/modify resize %s", self.DeviceName)
	}
	detail, err := cli.GetInstanceDetail(self.ins.Id)
	if err != nil {
		return errors.Wrapf(err, "GetInstanceDetail after resize")
	}
	for i := range detail.disks {
		if detail.disks[i].DeviceName == self.DeviceName {
			if detail.disks[i].Size < newSizeMB {
				return errors.Errorf("disk %s is %d MB after resize, want %d MB", self.DeviceName, detail.disks[i].Size, newSizeMB)
			}
			self.Size = detail.disks[i].Size
			self.ins.detail = detail
			return nil
		}
	}
	return errors.Errorf("disk %s missing from vm %s after resize", self.DeviceName, self.ins.GetName())
}

func (self *SDisk) Reset(ctx context.Context, snapshotId string) (string, error) {
	return "", errors.Wrap(cloudprovider.ErrNotSupported, "CAS has VM snapshots only; use ResetToInstanceSnapshot")
}

func (self *SDisk) Rebuild(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "Rebuild")
}

// volumeDisk wraps an unattached pool volume as an SDisk.
func volumeDisk(storage *SStorage, vol *SStorageVolume) SDisk {
	return SDisk{
		SInstanceStorage: SInstanceStorage{
			Device: "disk",
			Path:   volumePath(storage, vol.Name),
			Format: vol.Format,
			Size:   int64(vol.Size),
		},
		index:   -1,
		storage: storage,
	}
}

// vmHasDiskPath reports whether p is one of vm's disks.
func vmHasDiskPath(vm *SInstance, p string) bool {
	disks, err := vm.getDisks()
	if err != nil {
		return false
	}
	for i := range disks {
		if strings.TrimSuffix(disks[i].Path, "/") == strings.TrimSuffix(p, "/") {
			return true
		}
	}
	return false
}
