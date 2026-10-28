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

package shell

import "yunion.io/x/cloudmux/pkg/cloudprovider"

// imageRow is what `image-list` prints for one image (read-only).
func imageRow(cache cloudprovider.ICloudStoragecache, img cloudprovider.ICloudImage) map[string]interface{} {
	return map[string]interface{}{
		"id":              img.GetId(),
		"global_id":       img.GetGlobalId(),
		"name":            img.GetName(),
		"description":     img.GetDescription(),
		"status":          img.GetImageStatus(),
		"os_type":         string(img.GetOsType()),
		"os_full_name":    img.GetFullOsName(),
		"min_disk_gb":     img.GetMinOsDiskSizeGb(),
		"min_ram_mb":      img.GetMinRamSizeMb(),
		"size_byte":       img.GetSizeByte(),
		"storagecache_id": cache.GetGlobalId(),
	}
}

func init() {
	cmd := NewCommand("image")

	type ImageListOptions struct {
		ListBaseOptions
	}

	// list only reads (GET); template create/rename/delete stay on the
	// provider interfaces.
	RegionR[ImageListOptions](cmd).List("list", "List images (CAS VM templates) in every storage cache of the region", func(cli cloudprovider.ICloudRegion, _ *ImageListOptions) (any, error) {
		caches, err := cli.GetIStoragecaches()
		if err != nil {
			return nil, err
		}
		objs := []interface{}{}
		for i := range caches {
			images, err := caches[i].GetICloudImages()
			if err != nil {
				return nil, err
			}
			for j := range images {
				objs = append(objs, imageRow(caches[i], images[j]))
			}
		}
		return objs, nil
	})
}
