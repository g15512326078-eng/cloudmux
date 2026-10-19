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
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SVmTemplate is VmTemplateRsDomaiAall, one element of
// GET /cas/casrs/vmTemplate/all (key "domain").
type SVmTemplate struct {
	Id                 int64   `json:"id"`
	Name               string  `json:"name"`
	Description        string  `json:"description"`
	Memory             int64   `json:"memory"` // MB
	MemoryInit         float64 `json:"memoryInit"`
	MemoryUnit         string  `json:"memoryUnit"`
	Cpu                int     `json:"cpu"`
	System             int     `json:"system"` // 0 Windows, 1 Linux, 2 BSD
	OsVersion          string  `json:"osVersion"`
	CreateDate         string  `json:"createDate"`
	StorageCapacity    int64   `json:"storageCapacity"` // MB (assumed, see report)
	TempletStoragePath string  `json:"templetStoragePath"`
}

func (cli *SCasClient) GetVmTemplates() ([]SVmTemplate, error) {
	ret := []SVmTemplate{}
	return ret, cli.list("vmTemplate/all", nil, "domain", &ret)
}

// SImage exposes a CAS VM template as a system image.
type SImage struct {
	multicloud.SImageBase
	multicloud.STagBase

	SVmTemplate

	cache *SStoragecache
}

func (self *SImage) GetId() string {
	return fmt.Sprintf("%d", self.Id)
}

// GetGlobalId is prefixed with the region global id (CAS/<cloudprovider
// id>) because cloudpods looks cached images up by external id without
// scoping by account, and template ids are small integers on every CAS.
func (self *SImage) GetGlobalId() string {
	return fmt.Sprintf("%s/template/%d", self.cache.region.GetGlobalId(), self.Id)
}

func (self *SImage) GetName() string {
	return self.Name
}

func (self *SImage) GetDescription() string {
	return self.Description
}

func (self *SImage) GetStatus() string {
	return api.CACHED_IMAGE_STATUS_ACTIVE
}

func (self *SImage) GetImageStatus() string {
	return cloudprovider.IMAGE_STATUS_ACTIVE
}

func (self *SImage) Refresh() error {
	image, err := self.cache.GetIImageById(self.GetGlobalId())
	if err != nil {
		return err
	}
	return jsonutils.Update(self, image)
}

func (self *SImage) IsEmulated() bool {
	return false
}

func (self *SImage) GetIStoragecache() cloudprovider.ICloudStoragecache {
	return self.cache
}

func (self *SImage) GetSizeByte() int64 {
	return self.StorageCapacity * 1024 * 1024
}

func (self *SImage) GetImageType() cloudprovider.TImageType {
	return cloudprovider.ImageTypeSystem
}

func (self *SImage) GetMinOsDiskSizeGb() int {
	return int(self.StorageCapacity / 1024)
}

func (self *SImage) GetMinRamSizeMb() int {
	return int(self.Memory)
}

func (self *SImage) GetImageFormat() string {
	return ""
}

func (self *SImage) GetCreatedAt() time.Time {
	t, err := time.Parse(time.RFC3339, self.CreateDate)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (self *SImage) GetFullOsName() string {
	return self.OsVersion
}

func (self *SImage) GetOsType() cloudprovider.TOsType {
	if self.System == 0 {
		return cloudprovider.OsTypeWindows
	}
	return cloudprovider.OsTypeLinux
}

func (self *SImage) GetOsDist() string {
	return ""
}

func (self *SImage) GetOsVersion() string {
	return ""
}

func (self *SImage) GetOsArch() string {
	return ""
}

func (self *SImage) GetOsLang() string {
	return ""
}

func (self *SImage) GetBios() cloudprovider.TBiosType {
	return cloudprovider.BIOS
}

func (self *SImage) Delete(ctx context.Context) error {
	return errors.Wrap(cloudprovider.ErrNotImplemented, "Delete")
}
