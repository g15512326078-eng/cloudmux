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

// Package cas implements a cloudmux provider for H3C CAS (CVM/CVK)
// virtualization, talking to the CAS REST API under
// {AccessUrl}/cas/casrs/ with HTTP Digest authentication.
//
// Current scope:
//
//   - Read sync: host pools/clusters as zones, hosts, storage pools,
//     vswitch-based networks, VMs with disks/NICs, VM templates as images.
//
//   - VM lifecycle: create from template, start/stop/restart, delete into
//     the CAS recycle bin (never wipes volumes), VNC info.
//
//   - Disks and snapshots: create/attach/detach data disks (detach keeps
//     the volume), VM snapshots create/revert/delete.
//
//   - Idle analysis: read-only monitor/report metrics and the idle
//     scoring in the idle subpackage (cmx instance-idle-list).
//
//   - Templates: create from a VM (clone, the VM is kept), rename
//     (refused unless the current storages can be sent back unchanged),
//     delete (refused while fast-deployed or referenced by any VM).
//
//   - Image import: a qcow2 cloud image already in a file pool (or copied
//     there over SSH when explicitly configured) becomes a template via a
//     qcow2 overlay, a never-booted temporary VM and addVmTemplate opType=1
//     (imageimport.go, cmx image-import-template).
//
// Storagecache.UploadImage (cloudpods image upload), ISO installs and
// template storage pool management are not implemented.
//
// A read-only cloud account (ProviderConfig.ReadOnly) is enforced in the
// HTTP transport: every request other than GET/HEAD fails with
// ErrAccountReadOnly before it is sent. Template writes also check this
// up front, before any lookup.
//
// This is not the H3C public/CloudOS provider (pkg/multicloud/h3c) and has
// nothing to do with the Keystone CAS SSO driver.
package cas // import "yunion.io/x/cloudmux/pkg/multicloud/cas"
