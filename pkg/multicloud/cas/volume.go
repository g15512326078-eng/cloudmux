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
	"yunion.io/x/pkg/util/httputils"

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

// SStorageVolumeCreate is the body of POST /cas/casrs/storage/volume
// ("增加存储卷(以任务方式)"). capacity is MB (UNVERIFIED: api.yaml gives no
// unit; every other CAS volume size observed in the field uses MB).
type SStorageVolumeCreate struct {
	HostId   int64  `json:"hostId"`
	SpName   string `json:"spName"`
	VolName  string `json:"volName"`
	Capacity int64  `json:"capacity"`
	Format   string `json:"format"`
	// Mode: 0 lazy zero, 1 zero, other thin. We send thin (2).
	Mode int `json:"mode"`
	// BaseFile is api.yaml "基础镜像文件，可选": the backing file of a new
	// qcow2 overlay. Only the image import sets it.
	BaseFile string `json:"baseFile,omitempty"`
}

const VOLUME_MODE_THIN = 2

// CreateStorageVolume is POST /cas/casrs/storage/volume. api.yaml documents
// a bare 200 for it; a task body (msgId) is waited on, an empty body is
// taken as done.
func (cli *SCasClient) CreateStorageVolume(body *SStorageVolumeCreate) error {
	resp, err := cli.request(httputils.POST, "storage/volume", nil, jsonutils.Marshal(body))
	if err != nil {
		if errors.Cause(err) == ErrEmptyBody {
			return nil
		}
		return err
	}
	return cli.waitAck(resp, "POST storage/volume")
}

// waitStorageVolume polls GET storage/volume until the named volume
// appears. POST storage/volume may answer a bare 200 before the file is
// listed; a single immediate read would then fail and leave an orphan.
func (cli *SCasClient) waitStorageVolume(hostId int64, poolName, volName string) (*SStorageVolume, error) {
	interval, timeout := cli.taskPollInterval, cli.taskTimeout
	if interval <= 0 {
		interval = DEFAULT_TASK_POLL_INTERVAL
	}
	if timeout <= 0 {
		timeout = DEFAULT_TASK_TIMEOUT
	}
	var found *SStorageVolume
	var last error
	err := cloudprovider.Wait(interval, timeout, func() (bool, error) {
		vol, err := cli.GetStorageVolume(hostId, poolName, volName)
		if err == nil {
			found = vol
			last = nil
			return true, nil
		}
		last = err
		if errors.Cause(err) == cloudprovider.ErrNotFound {
			return false, nil
		}
		return false, err
	})
	if err != nil {
		if last != nil {
			return nil, errors.Wrapf(err, "waitStorageVolume %s: last=%v", volName, last)
		}
		return nil, errors.Wrapf(err, "waitStorageVolume %s", volName)
	}
	return found, nil
}

// DeleteStorageVolume is DELETE /cas/casrs/storage/volume/delete/{hostId}/
// {poolName}/{volName}. isWipeVolume is always sent as false: the volume
// file is removed, but its blocks are not zeroed.
func (cli *SCasClient) DeleteStorageVolume(hostId int64, poolName, volName string) error {
	params := url.Values{}
	params.Set("isWipeVolume", "false")
	res := fmt.Sprintf("storage/volume/delete/%d/%s/%s", hostId, url.PathEscape(poolName), url.PathEscape(volName))
	resp, err := cli.request(httputils.DELETE, res, params, nil)
	if err != nil {
		if errors.Cause(err) == ErrEmptyBody {
			return nil // 204, as documented
		}
		return err
	}
	return cli.waitAck(resp, "DELETE "+res)
}

// waitAck waits on an RsTaskMsg body if the response is one; any other
// non-empty body is accepted as a synchronous success.
func (cli *SCasClient) waitAck(resp jsonutils.JSONObject, what string) error {
	ack, err := parseTaskMsg(resp)
	if err != nil || ack.MsgId == 0 || ack.MsgId == MSG_ID_NO_TASK {
		return nil
	}
	if ack.MsgId < 0 {
		return errors.Errorf("%s returned msgId %d", what, ack.MsgId)
	}
	_, err = cli.waitTask(ack.MsgId)
	return err
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
