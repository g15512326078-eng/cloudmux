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
//     there over SSH, or pulled by CAS over HTTP, when explicitly
//     configured) becomes a template via a
//     qcow2 overlay, a never-booted temporary VM and addVmTemplate opType=1
//     (imageimport.go, cmx image-import-template). The import fails if
//     the new template's disk still has a backing file, or if that cannot
//     be checked. The upload step is pluggable (ImportSourceStager). The
//     SSH stager (sshupload.go) is only compiled with -tags cas_ssh, so the
//     default build does not link golang.org/x/crypto/ssh (x/crypto
//     v0.19.0 has open advisories; the newest release usable on go 1.24
//     does not fix them). Without the tag cmx --ssh-upload fails and says
//     to rebuild with -tags cas_ssh. The HTTP stager (httpupload.go,
//     always built) serves the file from a one-run Glance V1 stub
//     (GET /v1/images/<uuid>, random x-auth-token) only while it runs, has
//     CAS pull it with POST nova/download (PUT cinder/download only on a
//     404) into /vms/nova/<uuid>, then cinder/volume/createFromImage copies
//     it into the pool. Dry-run and read-only accounts never start the stub.
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
