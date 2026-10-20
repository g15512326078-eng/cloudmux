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

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// SVmAddStorage is StorageConfigADD in PUT /cas/casrs/vm/addDevice.
type SVmAddStorage struct {
	Device     string `json:"device"` // disk
	DeviceName string `json:"deviceName,omitempty"`
	TargetBus  string `json:"targetBus"`
	FileType   string `json:"fileType"` // file
	CacheType  string `json:"cacheType"`
	Format     string `json:"format,omitempty"`
	// PathType 0: existing file (Path); 2: new file (PoolName, FileName, Size).
	PathType int    `json:"pathType"`
	Path     string `json:"path,omitempty"`
	PoolName string `json:"poolName,omitempty"`
	FileName string `json:"fileName,omitempty"`
	Size     int64  `json:"size,omitempty"` // MB
	Mode     int    `json:"mode,omitempty"` // preallocation, 2 = thin
}

type SVmAddDeviceBody struct {
	Id      int64         `json:"id"`
	Storage SVmAddStorage `json:"storage"`
}

// SVmDelStorage is StorageConfigDEL in PUT /cas/casrs/vm/delDevice. The
// schema has only deviceName and path: there is no "delete the volume"
// flag, so delDevice unplugs the disk and leaves its file in the pool.
type SVmDelStorage struct {
	DeviceName string `json:"deviceName"`
	Path       string `json:"path,omitempty"`
}

type SVmDelDeviceBody struct {
	Id      int64         `json:"id"`
	Storage SVmDelStorage `json:"storage"`
}

const (
	ADD_DEVICE_PATH_EXISTING_FILE = 0
	ADD_DEVICE_PATH_NEW_FILE      = 2

	DEFAULT_DISK_BUS   = "virtio"
	DEFAULT_DISK_CACHE = "directsync" // cache mode observed on CAS attach
)

// diskBus reuses the bus of the VM's first disk so the guest has a driver
// for it; virtio when the VM has no disk.
func (self *SInstance) diskBus() string {
	disks, err := self.getDisks()
	if err == nil && len(disks) > 0 && len(disks[0].TargetBus) > 0 {
		return disks[0].TargetBus
	}
	return DEFAULT_DISK_BUS
}

func (self *SInstance) addDevice(storage SVmAddStorage) error {
	body := SVmAddDeviceBody{Id: self.Id, Storage: storage}
	if _, err := self.region.cli.doAsync(httputils.PUT, "vm/addDevice", nil, jsonutils.Marshal(body)); err != nil {
		return errors.Wrapf(err, "vm/addDevice")
	}
	// re-read without the per-sync cache so callers see the new disk
	return self.Refresh()
}

// resolveVolume finds the pool and volume a diskId names, as seen from
// this VM's host: a local pool must be on the VM's host, a shared pool in
// the VM's host pool.
func (self *SInstance) resolveVolume(diskId string) (*SStorage, *SStorageVolume, error) {
	storageId, volName, err := parseDiskId(diskId)
	if err != nil {
		return nil, nil, err
	}
	host, err := self.getHost()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "getHost")
	}
	storages, err := host.getStorages()
	if err != nil {
		return nil, nil, errors.Wrapf(err, "getStorages")
	}
	var storage *SStorage
	for i := range storages {
		if storages[i].GetGlobalId() == storageId {
			storage = &storages[i]
			break
		}
	}
	if storage == nil {
		return nil, nil, errors.Wrapf(cloudprovider.ErrNotFound, "storage %s is not reachable from host %s of vm %s", storageId, host.Name, self.GetName())
	}
	vol, err := self.region.cli.GetStorageVolume(host.Id, storage.Name, volName)
	if err != nil {
		return nil, nil, err
	}
	return storage, vol, nil
}

// AttachDisk plugs an existing volume (diskId from CreateIDisk / a pool
// listing) into the VM with PUT /vm/addDevice (pathType 0 = existing
// file) and waits for the task. Attaching a disk the VM already has is a
// no-op; a volume used by another VM is refused.
func (self *SInstance) AttachDisk(ctx context.Context, diskId string) error {
	storage, vol, err := self.resolveVolume(diskId)
	if err != nil {
		return err
	}
	if err := self.Refresh(); err != nil {
		return err
	}
	volPath := volumePath(storage, vol.Name)
	if vmHasDiskPath(self, volPath) {
		return nil
	}
	if vol.isAttached() {
		return errors.Wrapf(cloudprovider.ErrInvalidStatus, "volume %s is already used by %s", vol.Name, vol.Users)
	}
	if !vol.isDataVolume() {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "volume %s (format %s, level %d) is not a data disk", vol.Name, vol.Format, vol.Level)
	}
	return self.addDevice(SVmAddStorage{
		Device:    "disk",
		TargetBus: self.diskBus(),
		FileType:  "file",
		CacheType: DEFAULT_DISK_CACHE,
		Format:    vol.Format,
		PathType:  ADD_DEVICE_PATH_EXISTING_FILE,
		Path:      volPath,
	})
}

// DetachDisk unplugs a data disk with PUT /vm/delDevice and waits for the
// task. The volume file is kept (delDevice has no delete option). A disk
// the VM does not have is a no-op; the system disk is refused.
func (self *SInstance) DetachDisk(ctx context.Context, diskId string) error {
	if err := self.Refresh(); err != nil {
		return err
	}
	disks, err := self.getDisks()
	if err != nil {
		return err
	}
	for i := range disks {
		if disks[i].GetGlobalId() != diskId {
			continue
		}
		if disks[i].index == 0 {
			return errors.Wrapf(cloudprovider.ErrNotSupported, "detaching the system disk %s", disks[i].DeviceName)
		}
		body := SVmDelDeviceBody{Id: self.Id, Storage: SVmDelStorage{DeviceName: disks[i].DeviceName, Path: disks[i].Path}}
		if _, err := self.region.cli.WithContext(ctx).doAsync(httputils.PUT, "vm/delDevice", nil, jsonutils.Marshal(body)); err != nil {
			return errors.Wrapf(err, "vm/delDevice %s", disks[i].DeviceName)
		}
		return self.Refresh()
	}
	return nil
}

// createDataDisk adds a new thin qcow2 file of sizeMB to the VM in one
// PUT /vm/addDevice call (pathType 2 = new file in poolName).
func (self *SInstance) createDataDisk(poolName, fileName string, sizeMB int64) error {
	if sizeMB <= 0 {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "data disk size %d MB", sizeMB)
	}
	return self.addDevice(SVmAddStorage{
		Device:    "disk",
		TargetBus: self.diskBus(),
		FileType:  "file",
		CacheType: DEFAULT_DISK_CACHE,
		Format:    "qcow2",
		PathType:  ADD_DEVICE_PATH_NEW_FILE,
		PoolName:  poolName,
		FileName:  fileName,
		Size:      sizeMB,
		Mode:      VOLUME_MODE_THIN,
	})
}

func dataDiskFileName(vmName string, idx int) string {
	return path.Base(fmt.Sprintf("%s-data%d", vmName, idx))
}
