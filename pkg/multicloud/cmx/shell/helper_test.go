// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shell

import (
	"strings"
	"testing"
)

// A CAS provider without an access URL must fail before any factory lookup
// or request is made.
func TestNewCloudProviderCASRequiresURL(t *testing.T) {
	_, err := NewCloudProvider(&GlobalOptions{Provider: "CAS", AccessKey: "a", Secret: "s"})
	if err == nil || !strings.Contains(err.Error(), "CAS requires --cas-url (or CLOUDMUX_CAS_URL)") {
		t.Fatalf("want missing --cas-url error, got %v", err)
	}
}
