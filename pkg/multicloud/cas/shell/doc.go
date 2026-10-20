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

// Package shell is reserved for CAS-specific cmx subcommands.
//
// On the CAS-only cmx tree, metric-list and instance-idle-list are
// registered in pkg/multicloud/cmx/shell so they share cmx's
// ICloudProvider Invoke path. This package is blank-imported from cmx.
package shell
