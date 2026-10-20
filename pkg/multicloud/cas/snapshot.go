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
	"net/url"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"
	"yunion.io/x/pkg/util/timeutils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SVmSnapshot is api.yaml VMRsSnapshotList, an element of
// GET /cas/casrs/vm/snapshot/{vmId} (key "snapshot"). Snapshots form a
// tree: children are under snapshots.snapshot[]. CAS snapshots cover the
// whole VM and are addressed by (vmId, name).
type SVmSnapshot struct {
	multicloud.SVirtualResourceBase
	multicloud.STagBase

	ins *SInstance

	Name          string `json:"name"`
	SnapshotTitle string `json:"snapshotTitle"`
	Desc          string `json:"desc"`
	IsCurr        bool   `json:"isCurr"`
	SnapMem       bool   `json:"snapMem"`
	SnapshotMode  int    `json:"snapshotMode"`
	CreationTime  string `json:"creationTime"` // "2020-12-02 11:34:59", no zone

	Snapshots *struct {
		Snapshot jsonutils.JSONObject `json:"snapshot"`
	} `json:"snapshots"`
}

// GetId is "{vm uuid}/{snapshot name}" (names are unique per VM).
func (self *SVmSnapshot) GetId() string {
	return fmt.Sprintf("%s/%s", self.ins.Uuid, self.Name)
}

func (self *SVmSnapshot) GetGlobalId() string {
	return self.GetId()
}

func (self *SVmSnapshot) GetName() string {
	return self.Name
}

func (self *SVmSnapshot) GetDescription() string {
	return self.Desc
}

func (self *SVmSnapshot) GetStatus() string {
	return api.INSTANCE_SNAPSHOT_READY
}

// GetCreatedAt parses creationTime. It carries no zone; it is read as UTC
// (UNVERIFIED: CAS most likely prints the CVM's local time).
func (self *SVmSnapshot) GetCreatedAt() time.Time {
	t, err := timeutils.ParseTimeStr(self.CreationTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (self *SVmSnapshot) Refresh() error {
	snap, err := self.ins.getSnapshot(self.Name)
	if err != nil {
		return err
	}
	return jsonutils.Update(self, snap)
}

// Delete is DELETE /cas/casrs/vm/snapshot/{vmId}/{snapshotName}, waited on.
// Only this snapshot is removed; a missing snapshot is a no-op.
func (self *SVmSnapshot) Delete() error {
	res := fmt.Sprintf("vm/snapshot/%d/%s", self.ins.Id, url.PathEscape(self.Name))
	_, err := self.ins.region.cli.doAsync(httputils.DELETE, res, nil, nil)
	if err != nil && errors.Cause(err) == cloudprovider.ErrNotFound {
		return nil
	}
	return err
}

// flattenSnapshots walks the snapshot tree depth first.
func flattenSnapshots(items []jsonutils.JSONObject, ins *SInstance, out *[]SVmSnapshot) error {
	for i := range items {
		snap := SVmSnapshot{}
		if err := items[i].Unmarshal(&snap); err != nil {
			return errors.Wrapf(err, "Unmarshal snapshot")
		}
		snap.ins = ins
		var children []jsonutils.JSONObject
		if snap.Snapshots != nil && snap.Snapshots.Snapshot != nil {
			obj := jsonutils.NewDict()
			obj.Add(snap.Snapshots.Snapshot, "snapshot")
			var err error
			children, err = listItems(obj, "snapshot")
			if err != nil {
				return err
			}
		}
		snap.Snapshots = nil
		*out = append(*out, snap)
		if err := flattenSnapshots(children, ins, out); err != nil {
			return err
		}
	}
	return nil
}

// getSnapshots is GET /cas/casrs/vm/snapshot/{vmId}, flattened.
func (self *SInstance) getSnapshots() ([]SVmSnapshot, error) {
	resp, err := self.region.cli.request(httputils.GET, fmt.Sprintf("vm/snapshot/%d", self.Id), nil, nil)
	if err != nil {
		if errors.Cause(err) == ErrEmptyBody {
			return []SVmSnapshot{}, nil
		}
		return nil, err
	}
	items, err := listItems(resp, "snapshot")
	if err != nil {
		return nil, err
	}
	ret := []SVmSnapshot{}
	return ret, flattenSnapshots(items, self, &ret)
}

func (self *SInstance) getSnapshot(name string) (*SVmSnapshot, error) {
	snaps, err := self.getSnapshots()
	if err != nil {
		return nil, err
	}
	for i := range snaps {
		if snaps[i].Name == name {
			return &snaps[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "snapshot %s of vm %s", name, self.GetName())
}

// snapshotName accepts a snapshot global id ("{vm uuid}/{name}") or a bare
// name; an id of another VM is rejected.
func (self *SInstance) snapshotName(idStr string) (string, error) {
	if i := strings.Index(idStr, "/"); i >= 0 {
		if idStr[:i] != self.Uuid {
			return "", errors.Wrapf(cloudprovider.ErrNotFound, "snapshot %s does not belong to vm %s", idStr, self.Uuid)
		}
		idStr = idStr[i+1:]
	}
	if len(idStr) == 0 {
		return "", errors.Wrap(cloudprovider.ErrInputParameter, "empty snapshot id")
	}
	return idStr, nil
}

func (self *SInstance) GetInstanceSnapshots() ([]cloudprovider.ICloudInstanceSnapshot, error) {
	snaps, err := self.getSnapshots()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudInstanceSnapshot{}
	for i := range snaps {
		ret = append(ret, &snaps[i])
	}
	return ret, nil
}

func (self *SInstance) GetInstanceSnapshot(idStr string) (cloudprovider.ICloudInstanceSnapshot, error) {
	name, err := self.snapshotName(idStr)
	if err != nil {
		return nil, err
	}
	return self.getSnapshot(name)
}

// SVmSnapshotCreate is the body of POST /cas/casrs/vm/snapshot.
type SVmSnapshotCreate struct {
	VmId    int64  `json:"vmId"`
	Name    string `json:"name"`
	Desc    string `json:"desc,omitempty"`
	SnapMem bool   `json:"snapMem"`
}

// CreateInstanceSnapshot is POST /cas/casrs/vm/snapshot (disk-only:
// snapMem=false; snapshotMode left to the CAS default, internal), waited
// on, then read back by name. An existing snapshot of that name is an
// error, not reused.
func (self *SInstance) CreateInstanceSnapshot(ctx context.Context, name string, desc string) (cloudprovider.ICloudInstanceSnapshot, error) {
	if len(name) == 0 || strings.Contains(name, "/") {
		return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "snapshot name %q", name)
	}
	if _, err := self.getSnapshot(name); err == nil {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "snapshot %s of vm %s exists", name, self.GetName())
	} else if errors.Cause(err) != cloudprovider.ErrNotFound {
		return nil, err
	}
	body := SVmSnapshotCreate{VmId: self.Id, Name: name, Desc: desc, SnapMem: false}
	if _, err := self.region.cli.doAsync(httputils.POST, "vm/snapshot", nil, jsonutils.Marshal(body)); err != nil {
		return nil, errors.Wrapf(err, "vm/snapshot")
	}
	return self.getSnapshot(name)
}

type SVmSnapshotResume struct {
	VmId int64  `json:"vmId"`
	Name string `json:"name"`
}

// ResetToInstanceSnapshot is PUT /cas/casrs/vm/snapshot/resume, waited on.
// This discards the VM's current disk state since that snapshot; it runs
// only when cloudpods asks for it explicitly. The snapshot must exist.
func (self *SInstance) ResetToInstanceSnapshot(ctx context.Context, idStr string) error {
	name, err := self.snapshotName(idStr)
	if err != nil {
		return err
	}
	if _, err := self.getSnapshot(name); err != nil {
		return err
	}
	body := SVmSnapshotResume{VmId: self.Id, Name: name}
	if _, err := self.region.cli.doAsync(httputils.PUT, "vm/snapshot/resume", nil, jsonutils.Marshal(body)); err != nil {
		return errors.Wrapf(err, "vm/snapshot/resume %s", name)
	}
	self.detail = nil
	return nil
}
