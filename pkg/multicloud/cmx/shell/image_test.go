// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package shell

import (
	"testing"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// Stubs embed the interfaces; only the methods imageRow reads are set.
type stubCache struct {
	cloudprovider.ICloudStoragecache
}

func (stubCache) GetGlobalId() string { return "CAS/acc1-templates" }

type stubImage struct{ cloudprovider.ICloudImage }

func (stubImage) GetId() string                    { return "4" }
func (stubImage) GetGlobalId() string              { return "CAS/acc1/template/4" }
func (stubImage) GetName() string                  { return "tmpName" }
func (stubImage) GetDescription() string           { return "desc" }
func (stubImage) GetImageStatus() string           { return cloudprovider.IMAGE_STATUS_ACTIVE }
func (stubImage) GetOsType() cloudprovider.TOsType { return cloudprovider.OsTypeLinux }
func (stubImage) GetFullOsName() string            { return "CentOS 7" }
func (stubImage) GetMinOsDiskSizeGb() int          { return 20 }
func (stubImage) GetMinRamSizeMb() int             { return 1024 }
func (stubImage) GetSizeByte() int64               { return 20 << 30 }

func TestImageRow(t *testing.T) {
	row := imageRow(stubCache{}, stubImage{})
	want := map[string]interface{}{
		"id": "4", "global_id": "CAS/acc1/template/4", "name": "tmpName", "description": "desc",
		"status": cloudprovider.IMAGE_STATUS_ACTIVE, "os_type": "Linux", "os_full_name": "CentOS 7",
		"min_disk_gb": 20, "min_ram_mb": 1024, "size_byte": int64(20 << 30), "storagecache_id": "CAS/acc1-templates",
	}
	if len(row) != len(want) {
		t.Fatalf("row has %d keys, want %d: %v", len(row), len(want), row)
	}
	for k, v := range want {
		if row[k] != v {
			t.Errorf("%s = %#v, want %#v", k, row[k], v)
		}
	}
}
