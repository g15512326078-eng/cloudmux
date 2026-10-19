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
	"sync"

	"yunion.io/x/pkg/errors"
)

// sSyncCache memoises read-only API results for the lifetime of one SRegion.
// cli.GetRegion/GetIRegions hands out a fresh SRegion (and so an empty
// cache) on every call, so one sync pass that walks a region object sees
// one consistent snapshot and fetches each list once instead of once per
// host/VM/storage. Errors are not cached. Refresh() methods bypass it.
type sSyncCache struct {
	lock    sync.Mutex
	entries map[string]*sCacheEntry
}

type sCacheEntry struct {
	lock  sync.Mutex
	done  bool
	value interface{}
}

// get returns the cached value for key, calling fetch at most once
// successfully. Each key has its own lock so a fetch may use other keys.
func (c *sSyncCache) get(key string, fetch func() (interface{}, error)) (interface{}, error) {
	c.lock.Lock()
	if c.entries == nil {
		c.entries = map[string]*sCacheEntry{}
	}
	e, ok := c.entries[key]
	if !ok {
		e = &sCacheEntry{}
		c.entries[key] = e
	}
	c.lock.Unlock()

	e.lock.Lock()
	defer e.lock.Unlock()
	if e.done {
		return e.value, nil
	}
	v, err := fetch()
	if err != nil {
		return nil, err
	}
	e.value, e.done = v, true
	return v, nil
}

// The accessors below return fresh slices (shallow copies) so callers may
// set back-pointers (zone/host/region) on the elements without racing.

func (self *SRegion) cachedHostPools() ([]SHostPool, error) {
	v, err := self.cache.get("hostpools", func() (interface{}, error) {
		return self.cli.GetHostPools()
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostPools")
	}
	return append([]SHostPool{}, v.([]SHostPool)...), nil
}

func (self *SRegion) cachedHosts() ([]SHost, error) {
	v, err := self.cache.get("hosts", func() (interface{}, error) {
		return self.cli.GetHosts()
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetHosts")
	}
	return append([]SHost{}, v.([]SHost)...), nil
}

type sHostStorages struct {
	pools  []SStoragePool
	shared []SStoragePool
}

func (self *SRegion) cachedHostStorages(hostId int64) ([]SStoragePool, []SStoragePool, error) {
	v, err := self.cache.get(fmt.Sprintf("storages/%d", hostId), func() (interface{}, error) {
		pools, err := self.cli.GetStoragePools(hostId)
		if err != nil {
			return nil, errors.Wrapf(err, "GetStoragePools")
		}
		shared, err := self.cli.GetSharedStoragePools(hostId)
		if err != nil {
			return nil, errors.Wrapf(err, "GetSharedStoragePools")
		}
		return sHostStorages{pools: pools, shared: shared}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	s := v.(sHostStorages)
	return append([]SStoragePool{}, s.pools...), append([]SStoragePool{}, s.shared...), nil
}

func (self *SRegion) cachedHostVSwitches(hostId int64) ([]SVSwitch, error) {
	v, err := self.cache.get(fmt.Sprintf("vswitches/%d", hostId), func() (interface{}, error) {
		return self.cli.GetHostVSwitches(hostId)
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostVSwitches %d", hostId)
	}
	return append([]SVSwitch{}, v.([]SVSwitch)...), nil
}

// cachedInstances is the full, paged vm/vmList. Host and storage views
// filter it locally instead of issuing per-host list calls.
func (self *SRegion) cachedInstances() ([]SInstance, error) {
	v, err := self.cache.get("vms", func() (interface{}, error) {
		return self.cli.GetInstances(nil)
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetInstances")
	}
	vms := append([]SInstance{}, v.([]SInstance)...)
	for i := range vms {
		vms[i].region = self
	}
	return vms, nil
}

// cachedHostDetails is GET vm/detail/all?hostId=, one call per host,
// keyed by VM uuid. The returned details are shared and must not be mutated.
func (self *SRegion) cachedHostDetails(hostId int64) (map[string]*SInstanceDetail, error) {
	v, err := self.cache.get(fmt.Sprintf("details/%d", hostId), func() (interface{}, error) {
		details, err := self.cli.GetHostInstanceDetails(hostId)
		if err != nil {
			return nil, err
		}
		ret := map[string]*SInstanceDetail{}
		for i := range details {
			ret[details[i].Uuid] = &details[i]
		}
		return ret, nil
	})
	if err != nil {
		return nil, errors.Wrapf(err, "GetHostInstanceDetails %d", hostId)
	}
	return v.(map[string]*SInstanceDetail), nil
}
