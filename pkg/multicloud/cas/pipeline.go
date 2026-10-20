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

// Plain helpers shared by the qcow2 (imageimport.go) and Windows
// (windowsimport.go) import pipelines (S-4). Each pipeline keeps its own
// cleanup state machine: their keep/delete rules differ (an overlay that
// a VM may reference vs. a VM that may have run Setup).

import (
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"
)

// postTempVM sends vm/add through rawRequest (which keeps the exact HTTP
// status for isDefiniteRejection) and waits for its task. rejected is
// true only when CAS certainly did not create the VM.
func (c *SCasClient) postTempVM(name string, body []byte) (rejected bool, err error) {
	data, ce, err := c.rawRequest(httputils.POST, "vm/add", nil, body, "application/json; charset=UTF-8")
	if err == nil && ce != nil {
		err = ce
	}
	if err != nil {
		return isDefiniteRejection(err), errors.Wrapf(err, "vm/add %s", name)
	}
	resp, err := parseJSONBody(httputils.POST, "vm/add", data)
	if err != nil {
		return false, errors.Wrapf(err, "vm/add %s", name)
	}
	if _, err := c.waitAsync(httputils.POST, "vm/add", resp); err != nil {
		return false, errors.Wrapf(err, "vm/add %s", name)
	}
	return false, nil
}

// findTempVM returns the single VM named name after a successful vm/add.
func (c *SCasClient) findTempVM(region *SRegion, name string) (*SInstance, error) {
	vms, err := c.findVMsByName(name)
	if err != nil {
		return nil, errors.Wrapf(err, "find temporary VM")
	}
	if len(vms) != 1 {
		return nil, errors.Errorf("expected 1 VM named %s after vm/add, found %d", name, len(vms))
	}
	vm := &vms[0]
	vm.region = region
	return vm, nil
}

// templateLeftovers re-lists the templates named name after a failed
// addVmTemplate: each one found is named in an error (advice says what
// to do with it), so a half-made template is never silent.
func (c *SCasClient) templateLeftovers(name, advice string) ([]SVmTemplate, []error) {
	found, err := c.findTemplatesNamed(name)
	if err != nil {
		return nil, []error{errors.Wrapf(err, "could not re-list templates; check by hand whether a template %q was left", name)}
	}
	errs := []error{}
	for i := range found {
		errs = append(errs, errors.Errorf("template %d %q exists after the failure and may be incomplete: %s", found[i].Id, found[i].Name, advice))
	}
	return found, errs
}
