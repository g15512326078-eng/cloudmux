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
	"os"
	"path/filepath"
	"testing"
)

// writeLocal writes a local "image" for the upload stagers (ssh, http).
func writeLocal(t *testing.T, content []byte) string {
	p := filepath.Join(t.TempDir(), "cloud.qcow2")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
