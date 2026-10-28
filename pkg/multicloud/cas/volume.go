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
	"path"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// SStorageVolume is api.yaml RsStorageVolumeList, an element of
// GET /cas/casrs/storage/volume (key "storageVolume"). size/allocation are
// MB (api.yaml says so for this schema).
type SStorageVolume struct {
	Name            string  `json:"name"`
	Format          string  `json:"format"`
	Size            float64 `json:"size"`
	Allocation      float64 `json:"allocation"`
	HostName        string  `json:"hostName"`
	StoragePoolName string  `json:"storagePoolName"`
	BackingStore    string  `json:"backingStore"`
	// Users is the comma-separated list of VM names using the volume;
	// empty means unattached.
	Users string `json:"users"`
	// Level 0 is a leaf (incremental) file; > 0 is a base/backing file.
	Level int `json:"level"`
}

func (vol *SStorageVolume) isAttached() bool {
	return len(strings.TrimSpace(vol.Users)) > 0
}

// isDataVolume is true for a leaf qcow2/raw file: ISOs (format iso) and
// base files of linked clones/templates (level > 0) are not disks.
func (vol *SStorageVolume) isDataVolume() bool {
	switch strings.ToLower(vol.Format) {
	case "qcow2", "raw":
	default:
		return false
	}
	return vol.Level == 0 && len(vol.Name) > 0
}

// GetStorageVolumes is GET /cas/casrs/storage/volume?hostId=&poolName=
// [&volumeName=], paged with offset + pageSize.
func (cli *SCasClient) GetStorageVolumes(hostId int64, poolName, volName string) ([]SStorageVolume, error) {
	params := url.Values{}
	params.Set("hostId", fmt.Sprintf("%d", hostId))
	params.Set("poolName", poolName)
	if len(volName) > 0 {
		params.Set("volumeName", volName)
	}
	items, err := cli.listPagedBy("storage/volume", params, "storageVolume", "name", "pageSize")
	if err != nil {
		return nil, err
	}
	ret := []SStorageVolume{}
	if err := jsonutils.NewArray(items...).Unmarshal(&ret); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal storageVolume")
	}
	return ret, nil
}

// getStorageVolumeRaw is GetStorageVolume without decoding, so callers can
// tell a missing field from an empty one.
func (cli *SCasClient) getStorageVolumeRaw(hostId int64, poolName, volName string) (jsonutils.JSONObject, error) {
	params := url.Values{}
	params.Set("hostId", fmt.Sprintf("%d", hostId))
	params.Set("poolName", poolName)
	params.Set("volumeName", volName)
	items, err := cli.listPagedBy("storage/volume", params, "storageVolume", "name", "pageSize")
	if err != nil {
		return nil, err
	}
	for i := range items {
		if n, _ := items[i].GetString("name"); n == volName {
			return items[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "volume %s in pool %s on host %d", volName, poolName, hostId)
}

// GetStorageVolume finds one volume by exact name in a pool.
func (cli *SCasClient) GetStorageVolume(hostId int64, poolName, volName string) (*SStorageVolume, error) {
	vols, err := cli.GetStorageVolumes(hostId, poolName, volName)
	if err != nil {
		return nil, err
	}
	for i := range vols {
		if vols[i].Name == volName {
			return &vols[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "volume %s in pool %s on host %d", volName, poolName, hostId)
}

// diskId builds the stable external id of a volume: the storage global id
// plus the file name, e.g. "host/1/defaultpool/vm1-data.qcow2" or
// "hostpool/1/share1/data1". It does not change on attach/detach.
func diskId(storageId, volName string) string {
	return fmt.Sprintf("%s/%s", storageId, volName)
}

// parseDiskId splits a diskId into the storage global id and file name.
func parseDiskId(id string) (storageId, volName string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 4 || (parts[0] != "host" && parts[0] != "hostpool") || len(parts[3]) == 0 {
		return "", "", errors.Wrapf(cloudprovider.ErrInputParameter, "disk id %q is not <host|hostpool>/<id>/<pool>/<volume>", id)
	}
	return strings.Join(parts[:3], "/"), parts[3], nil
}

func volumePath(pool *SStorage, volName string) string {
	return path.Join(pool.Path, volName)
}
