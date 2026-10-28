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

package idle_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

// Ensures the idle command's ReadOnlyClone cannot reach write endpoints.
func TestReadOnlyCloneBlocksWrites(t *testing.T) {
	wrote := false
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/casrs/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"host":[]}`))
	})
	mux.HandleFunc("/cas/casrs/vm/deploy", func(w http.ResponseWriter, r *http.Request) {
		wrote = true
		w.WriteHeader(200)
		w.Write([]byte(`{"msgId":1}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cli := &cas.SCasClient{CasClientConfig: cas.NewCasClientConfig(srv.URL, "u", "p")}
	ro := cli.ReadOnlyClone()
	_, err := ro.DeployVM(&cas.SDomainDeploy{Id: 1, DomainName: "x", Title: "x", DeployType: 1, DeployMode: 0, TargetHostId: 1, CpuSocket: 1, CpuCore: 1, Memory: 512, MemoryUnit: "MB"})
	if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Fatalf("err=%v want ErrAccountReadOnly", err)
	}
	if wrote {
		t.Fatal("write reached server")
	}
	// GET still works
	_, err = ro.GetHostBriefs()
	if err != nil {
		t.Fatalf("GET blocked: %v", err)
	}
}
