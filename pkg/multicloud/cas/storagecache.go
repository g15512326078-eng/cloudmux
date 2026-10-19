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

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud"
)

// SStoragecache is a single account-wide cache holding the CAS VM
// templates, which are not tied to one storage pool in the API.
type SStoragecache struct {
	multicloud.SResourceBase
	multicloud.STagBase

	region *SRegion
}

func (self *SRegion) getStoragecache() *SStoragecache {
	return &SStoragecache{region: self}
}

func (self *SStoragecache) GetId() string {
	return fmt.Sprintf("%s-templates", self.region.GetId())
}

func (self *SStoragecache) GetGlobalId() string {
	return fmt.Sprintf("%s-templates", self.region.GetGlobalId())
}

func (self *SStoragecache) GetName() string {
	return fmt.Sprintf("%s-templates", self.region.GetName())
}

func (self *SStoragecache) GetStatus() string {
	return "available"
}

func (self *SStoragecache) GetPath() string {
	return ""
}

func (self *SStoragecache) GetICloudImages() ([]cloudprovider.ICloudImage, error) {
	templates, err := self.region.cli.GetVmTemplates()
	if err != nil {
		return nil, err
	}
	ret := []cloudprovider.ICloudImage{}
	for i := range templates {
		ret = append(ret, &SImage{SVmTemplate: templates[i], cache: self})
	}
	return ret, nil
}

func (self *SStoragecache) GetICustomizedCloudImages() ([]cloudprovider.ICloudImage, error) {
	return []cloudprovider.ICloudImage{}, nil
}

func (self *SStoragecache) GetIImageById(id string) (cloudprovider.ICloudImage, error) {
	images, err := self.GetICloudImages()
	if err != nil {
		return nil, err
	}
	for i := range images {
		if images[i].GetGlobalId() == id {
			return images[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "template %s", id)
}

func (self *SStoragecache) UploadImage(ctx context.Context, opts *cloudprovider.SImageCreateOption, callback func(float32)) (string, error) {
	return "", errors.Wrap(cloudprovider.ErrNotImplemented, "UploadImage")
}
