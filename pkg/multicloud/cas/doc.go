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
//     the CAS recycle bin (never wipes volumes), VNC info. Create refuses
//     a name a live VM already has, and an ambiguous deploy (timeout,
//     transport error, failed task) names any VM it may have created
//     instead of retrying or adopting it.
//
//   - Disks and snapshots: create/attach/detach data disks (detach keeps
//     the volume), VM snapshots create/revert/delete.
//
//   - Templates: create from a shut-off VM (clone, the VM is kept; the
//     template must be proven flat, else it is kept and reported), rename
//     (refused unless the current storages can be sent back unchanged),
//     delete (refused while fast-deployed or referenced by any VM).
//
// Storagecache.UploadImage (cloudpods image upload) is not implemented.
//
// Every CAS request is bounded (10s dial, 10s TLS handshake, 30s response
// header, 5 min per request; the probe 30s) and follows the ctx of the
// cloudpods call; task waits keep their own overall deadline.
//
// A read-only cloud account (ProviderConfig.ReadOnly) is enforced in the
// HTTP transport: every request other than GET/HEAD fails with
// ErrAccountReadOnly before it is sent. Template writes also check this
// up front, before any lookup.
//
// Provenance: request and response shapes follow the CAS REST API
// reference (api.yaml). The points below are not in the official CAS API
// documentation, or go beyond it, and have not yet been verified against
// a real CAS; they are UNVERIFIED in the code:
//
//   - the legacy charset of the Error-Message header (decodeErrorMessage);
//   - task polling: msgId -1 for writes that finish inline, a brief 403
//     while the Digest nonce rolls over, and message keyValue context.
//
// This is not the H3C public/CloudOS provider (pkg/multicloud/h3c) and has
// nothing to do with the Keystone CAS SSO driver.
package cas // import "yunion.io/x/cloudmux/pkg/multicloud/cas"
