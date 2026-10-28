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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
)

func TestParseReportEChartNested(t *testing.T) {
	raw := `{"data":[{"name":"cpu","title":"CPU","list":[{"data":[
		{"name":"2026-03-01 09","id":1,"rate":12.5},
		{"name":"2026-03-01 10","id":2,"rate":8.0}
	]}]}]}`
	obj, err := jsonutils.ParseString(raw)
	if err != nil {
		t.Fatal(err)
	}
	pts, err := parseReportEChart(obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 || pts[0].Rate != 12.5 {
		t.Fatalf("pts=%v", pts)
	}
}

func TestGetReportVmSeriesHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/casrs/reports/vm/cpuMem", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cycle") != "0" {
			t.Errorf("cycle=%s", r.URL.Query().Get("cycle"))
		}
		if r.URL.Query().Get("domainId") != "42" {
			t.Errorf("domainId=%s", r.URL.Query().Get("domainId"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []interface{}{
				map[string]interface{}{
					"list": []interface{}{
						map[string]interface{}{"name": "2026-03-01 09", "rate": 3.0},
					},
				},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
	pts, err := cli.GetReportVmSeries("cpuMem", 42, ReportFlowCPU,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 14, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Rate != 3 {
		t.Fatalf("%v", pts)
	}
}
