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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestGetMetricsMapsMonitorUnits(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/casrs/vm/uuid-1/monitor", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		reply(w, dict{
			"cpuRate": 12.5, "memRate": 40.0, "flowLink": 3,
			"net":       []dict{{"mac": "aa", "time": 1700000000000, "readFlow": 10, "writeFlow": 2, "readPackets": 1, "writePackets": 1}},
			"disk":      []dict{{"device": "vda", "time": 1700000000000, "readStat": 5, "writeStat": 1, "readReqest": 7, "writeReqest": 3}},
			"partition": []dict{{"device": "/", "usage": 55, "size": 100, "usedSize": 55}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}

	out, err := cli.GetMetrics(&cloudprovider.MetricListOptions{
		ResourceType: cloudprovider.METRIC_RESOURCE_TYPE_SERVER,
		ResourceId:   "uuid-1",
		StartTime:    time.Now().Add(-time.Hour),
		EndTime:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	byType := map[cloudprovider.TMetricType]float64{}
	for _, m := range out {
		if len(m.Values) != 1 {
			t.Fatalf("%s values=%d", m.MetricType, len(m.Values))
		}
		byType[m.MetricType] = m.Values[0].Value
		if m.Id != "uuid-1" {
			t.Errorf("id %s", m.Id)
		}
	}
	assertClose := func(mt cloudprovider.TMetricType, want float64) {
		t.Helper()
		got, ok := byType[mt]
		if !ok {
			t.Fatalf("missing %s in %#v", mt, byType)
		}
		if got < want-1e-6 || got > want+1e-6 {
			t.Errorf("%s = %v want %v", mt, got, want)
		}
	}
	assertClose(cloudprovider.VM_METRIC_TYPE_CPU_USAGE, 12.5)
	assertClose(cloudprovider.VM_METRIC_TYPE_MEM_USAGE, 40)
	assertClose(cloudprovider.VM_METRIC_TYPE_NET_BPS_RX, 10*NET_FLOW_TO_BITS_PER_SEC)
	assertClose(cloudprovider.VM_METRIC_TYPE_NET_BPS_TX, 2*NET_FLOW_TO_BITS_PER_SEC)
	assertClose(cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_BPS, 5*DISK_STAT_TO_BYTES_PER_SEC)
	assertClose(cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_BPS, 1*DISK_STAT_TO_BYTES_PER_SEC)
	assertClose(cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_IOPS, 7)
	assertClose(cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_IOPS, 3)
	assertClose(cloudprovider.VM_METRIC_TYPE_DISK_USAGE, 55)
	if IncludeFlowLinkAsTCPConnections {
		assertClose(cloudprovider.VM_METRIC_TYPE_NET_TCP_CONNECTION, 3)
	}
}

func TestGetMetricsSkipsEmptySections(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cas/casrs/vm/uuid-2/monitor", func(w http.ResponseWriter, r *http.Request) {
		reply(w, dict{"cpuRate": 1, "memRate": 2})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
	out, err := cli.GetMetrics(&cloudprovider.MetricListOptions{
		ResourceType: cloudprovider.METRIC_RESOURCE_TYPE_SERVER,
		ResourceId:   "uuid-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range out {
		if strings.Contains(string(m.MetricType), "diskio") || strings.Contains(string(m.MetricType), "bps_") {
			t.Errorf("unexpected %s when net/disk absent", m.MetricType)
		}
	}
	if len(out) < 2 {
		t.Fatalf("got %d", len(out))
	}
}

func TestGetMetricsRequiresResourceId(t *testing.T) {
	cli := &SCasClient{CasClientConfig: NewCasClientConfig("http://127.0.0.1:1", "u", "p")}
	_, err := cli.GetMetrics(&cloudprovider.MetricListOptions{ResourceType: cloudprovider.METRIC_RESOURCE_TYPE_SERVER})
	if err == nil {
		t.Fatal("expected error")
	}
}
