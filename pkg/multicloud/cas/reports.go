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
	"fmt"
	"net/url"
	"strconv"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"
)

// Report cycle values (api.yaml reports/* cycle param).
const (
	ReportCycleHour  = 0
	ReportCycleDay   = 1
	ReportCycleWeek  = 2
	ReportCycleMonth = 3
	ReportCycleYear  = 4
)

// Flow types for reports/vm/cpuMem and netSp/disk.
const (
	ReportFlowCPU       = 0
	ReportFlowMEM       = 1
	ReportFlowNetRead   = 0 // netSp: read rate
	ReportFlowNetWrite  = 1
	ReportFlowDiskRead  = 0
	ReportFlowDiskWrite = 1
)

// SReportEChartValue is one bucket in a reports/* series.
// UNVERIFIED: whether Name is a bucket label (yyyy-MM-dd HH) or an epoch,
// and whether Rate is avg or max of the 1H aggregate (research §3.5).
type SReportEChartValue struct {
	Name string  `json:"name"`
	Id   int64   `json:"id"`
	Rate float64 `json:"rate"`
}

// SVmResourceDetail is oneBtn/vmResourceDetail row (api.yaml ~10691).
type SVmResourceDetail struct {
	Id          int64   `json:"id"`
	Uuid        string  `json:"uuid"`
	Title       string  `json:"title"`
	MonthTopCpu float64 `json:"monthTopCpu"`
	MonthAvgCpu float64 `json:"monthAvgCpu"`
	MonthTopMem float64 `json:"monthTopMem"`
	MonthAvgMem float64 `json:"monthAvgMem"`
}

// SZombieVm is oneBtn/zombieVm row. closeTimeout unit is UNVERIFIED
// (research assumes days).
type SZombieVm struct {
	Title   string `json:"title"`
	Status  string `json:"status"`
	OffTime string `json:"offTime"` // human string e.g. "1月7天1时" — UNVERIFIED
	Host    string `json:"host"`
	Cpu     int    `json:"cpu"`
	Mem     int64  `json:"mem"`
	VmDisk  int64  `json:"vmDisk"`
	Uuid    string `json:"uuid"`
	Id      int64  `json:"id"`
}

// GetReportVmSeries is GET reports/vm/{kind} with cycle=0 (hour).
// kind is cpuMem|netSp|disk|io. domainId is the numeric VM id.
// start/end formatting is UNVERIFIED: api.yaml types int64 but examples
// look like dates; we send unix seconds (flip to date string if live CAS
// rejects it).
func (cli *SCasClient) GetReportVmSeries(kind string, domainId int64, flowType int, start, end time.Time) ([]SReportEChartValue, error) {
	if domainId <= 0 {
		return nil, errors.Wrap(errors.ErrEmpty, "domainId")
	}
	params := url.Values{}
	params.Set("domainId", fmt.Sprintf("%d", domainId))
	params.Set("cycle", strconv.Itoa(ReportCycleHour))
	params.Set("flowType", strconv.Itoa(flowType))
	// UNVERIFIED time format: prefer yyyy-MM-dd (matches description examples).
	params.Set("startTime", start.Format("2006-01-02"))
	params.Set("endTime", end.Format("2006-01-02"))
	res := fmt.Sprintf("reports/vm/%s", kind)
	obj, err := cli.request(httputils.GET, res, params, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "GET %s", res)
	}
	return parseReportEChart(obj)
}

// parseReportEChart walks the nested RsEChartDataRsCommList shape and
// flattens every EChartValue. Nested wrappers vary between
// data[].list[].data[] and data[].list[] — both are accepted.
func parseReportEChart(obj jsonutils.JSONObject) ([]SReportEChartValue, error) {
	out := []SReportEChartValue{}
	if obj == nil {
		return out, nil
	}
	var walk func(jsonutils.JSONObject)
	walk = func(o jsonutils.JSONObject) {
		if o == nil {
			return
		}
		switch v := o.(type) {
		case *jsonutils.JSONDict:
			m, _ := v.GetMap()
			// leaf EChartValue: has "rate"
			if _, ok := m["rate"]; ok {
				var ev SReportEChartValue
				if err := v.Unmarshal(&ev); err == nil {
					out = append(out, ev)
					return
				}
			}
			for _, key := range []string{"data", "list"} {
				if child, err := v.Get(key); err == nil {
					walk(child)
				}
			}
		case *jsonutils.JSONArray:
			arr, _ := v.GetArray()
			for i := range arr {
				walk(arr[i])
			}
		}
	}
	walk(obj)
	if len(out) == 0 {
		log.Debugf("cas reports: no EChartValue parsed from %s", obj.String())
	}
	return out, nil
}

// GetVmResourceDetails pages oneBtn/vmResourceDetail (cheap prefilter).
func (cli *SCasClient) GetVmResourceDetails() ([]SVmResourceDetail, error) {
	params := url.Values{}
	params.Set("offset", "0")
	params.Set("limit", "200")
	rows := []SVmResourceDetail{}
	for {
		var page struct {
			Offset int64               `json:"offset"`
			Total  int64               `json:"total"`
			Data   []SVmResourceDetail `json:"data"`
		}
		if err := cli.get("oneBtn/vmResourceDetail", params, &page); err != nil {
			return nil, errors.Wrap(err, "oneBtn/vmResourceDetail")
		}
		rows = append(rows, page.Data...)
		if int64(len(rows)) >= page.Total || len(page.Data) == 0 {
			break
		}
		params.Set("offset", strconv.FormatInt(int64(len(rows)), 10))
	}
	return rows, nil
}

// GetZombieVms is GET oneBtn/zombieVm. Scope ids of 0 mean "all" when the
// CVM accepts them; requiredness in api.yaml is UNVERIFIED live (many
// "required" query params are optional in practice).
func (cli *SCasClient) GetZombieVms(closeTimeoutDays int) ([]SZombieVm, error) {
	params := url.Values{}
	params.Set("closeTimeout", strconv.Itoa(closeTimeoutDays))
	params.Set("hostPoolId", "0")
	params.Set("clusterId", "0")
	params.Set("hostId", "0")
	params.Set("offset", "0")
	params.Set("limit", "200")
	params.Set("sortDir", "0")
	params.Set("sortField", "title")
	var page struct {
		Data []SZombieVm `json:"data"`
	}
	if err := cli.get("oneBtn/zombieVm", params, &page); err != nil {
		return nil, errors.Wrap(err, "oneBtn/zombieVm")
	}
	return page.Data, nil
}
