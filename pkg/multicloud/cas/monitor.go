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
	"net/url"
	"time"

	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// Unit conversion factors for vm/{uuid}/monitor. CAS does not document
// units for net/disk counters; these defaults match common CVM practice
// and are easy to flip after live calibration (see research §3.5).
const (
	// NET_FLOW_TO_BITS_PER_SEC multiplies readFlow/writeFlow to bits/s.
	// UNVERIFIED: default treats CAS flow as KB/s → *1024*8.
	NET_FLOW_TO_BITS_PER_SEC = 1024.0 * 8

	// DISK_STAT_TO_BYTES_PER_SEC multiplies readStat/writeStat to B/s.
	// UNVERIFIED: default treats CAS disk stats as KB/s → *1024.
	DISK_STAT_TO_BYTES_PER_SEC = 1024.0

	// DISK_REQ_TO_IOPS multiplies readReqest/writeReqest to IOPS.
	// UNVERIFIED: DDL has both per-30s RD_REQ and RD_REQ_PS; default
	// assumes the monitor field is already per-second (factor 1).
	DISK_REQ_TO_IOPS = 1.0

	// IncludeFlowLinkAsTCPConnections maps flowLink →
	// vm_netio.tcp_connections. flowLink semantics (host conntrack vs
	// guest TCP sessions) are UNVERIFIED; set false to omit the metric.
	IncludeFlowLinkAsTCPConnections = false
)

// SVmMonitor is GET /cas/casrs/vm/{uuid}/monitor (api.yaml ~22287): one
// current sample of CPU, mem, NICs, disks and partitions. Units other
// than cpuRate/memRate/partition.usage (%) are UNVERIFIED.
type SVmMonitor struct {
	// CpuRate / MemRate are pointers so a missing JSON field is not faked as 0.
	CpuRate        *float64 `json:"cpuRate"`
	MemRate        *float64 `json:"memRate"`
	Status         string   `json:"status"`
	Uptime         int64    `json:"uptime"` // minutes; -1 unknown
	CastoolsStatus string   `json:"castoolsStatus"`
	// FlowLink is "虚拟机连接数". Semantics UNVERIFIED.
	FlowLink float64 `json:"flowLink"`

	Partition []SVmMonitorPartition `json:"partition"`
	Net       []SVmMonitorNet       `json:"net"`
	Disk      []SVmMonitorDisk      `json:"disk"`
}

type SVmMonitorPartition struct {
	Device   string  `json:"device"`
	Usage    float64 `json:"usage"` // %
	Size     float64 `json:"size"`  // GB [inferred]
	UsedSize float64 `json:"usedSize"`
}

type SVmMonitorNet struct {
	Mac          string  `json:"mac"`
	Time         int64   `json:"time"` // epoch s or ms — UNVERIFIED
	ReadFlow     float64 `json:"readFlow"`
	WriteFlow    float64 `json:"writeFlow"`
	ReadPackets  float64 `json:"readPackets"`
	WritePackets float64 `json:"writePackets"`
}

type SVmMonitorDisk struct {
	Device       string  `json:"device"`
	Time         int64   `json:"time"`
	IoStat       float64 `json:"ioStat"`
	ReadStat     float64 `json:"readStat"`
	WriteStat    float64 `json:"writeStat"`
	ReadReqest   float64 `json:"readReqest"` // spelling from api.yaml
	WriteReqest  float64 `json:"writeReqest"`
	ReadLatency  float64 `json:"readLatency"`
	WriteLatency float64 `json:"writeLatency"`
}

// GetVmMonitor is GET vm/{uuid}/monitor. uuid is the cloudpods ExternalId.
func (cli *SCasClient) GetVmMonitor(uuid string) (*SVmMonitor, error) {
	if len(uuid) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrInputParameter, "empty vm uuid")
	}
	resp, err := cli.request(httputils.GET, "vm/"+url.PathEscape(uuid)+"/monitor", nil, nil)
	if err != nil {
		return nil, err
	}
	resp = unwrapRoot(resp)
	out := &SVmMonitor{}
	if err := resp.Unmarshal(out); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal vm/%s/monitor", uuid)
	}
	_ = unmarshalList(resp, "partition", &out.Partition)
	_ = unmarshalList(resp, "net", &out.Net)
	_ = unmarshalList(resp, "disk", &out.Disk)
	return out, nil
}

// epochToTime converts a CAS epoch that may be seconds or milliseconds.
// Values below 1e12 are treated as seconds (e.g. 1607412360); larger as ms
// (e.g. 1604902850000). api.yaml shows both forms on the same monitor payload.
func epochToTime(t int64) time.Time {
	if t <= 0 {
		return time.Time{}
	}
	const msThreshold = int64(1_000_000_000_000) // ~2001-09-09 in ms
	if t < msThreshold {
		return time.Unix(t, 0)
	}
	return time.UnixMilli(t)
}

// monitorTimestamp picks the newest sample time. Each raw value is first
// normalized with epochToTime (s vs ms per value), then the maximum is taken,
// so a NIC reporting seconds and a disk reporting ms compare correctly.
func monitorTimestamp(sample int64, nets []SVmMonitorNet, disks []SVmMonitorDisk) time.Time {
	if sample > 0 {
		return epochToTime(sample)
	}
	var newest time.Time
	consider := func(raw int64) {
		if t := epochToTime(raw); t.After(newest) {
			newest = t
		}
	}
	for _, n := range nets {
		consider(n.Time)
	}
	for _, d := range disks {
		consider(d.Time)
	}
	if newest.IsZero() {
		// Current-sample endpoint; cloudmon polls every few minutes.
		return time.Now()
	}
	return newest
}

func onePoint(id string, mt cloudprovider.TMetricType, unit string, ts time.Time, v float64) cloudprovider.MetricValues {
	return cloudprovider.MetricValues{
		Id:         id,
		MetricType: mt,
		Unit:       unit,
		Values:     []cloudprovider.MetricValue{{Timestamp: ts, Value: v}},
	}
}

// GetMetrics implements cloudprovider.ICloudProvider for CAS VMs.
// cloudmon's CAS driver (SCollectByResourceIdDriver) calls this every ~6
// min with ResourceType=server, ResourceId=<VM UUID> and no MetricType, so
// we return every available VM metric from one GET vm/{uuid}/monitor.
// Metrics CAS does not return are skipped (never faked).
func (cli *SCasClient) GetMetrics(opts *cloudprovider.MetricListOptions) ([]cloudprovider.MetricValues, error) {
	if opts == nil {
		return nil, errors.Wrap(cloudprovider.ErrInputParameter, "nil MetricListOptions")
	}
	switch opts.ResourceType {
	case cloudprovider.METRIC_RESOURCE_TYPE_SERVER, "":
		return cli.getServerMetrics(opts)
	default:
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "CAS GetMetrics resource %s", opts.ResourceType)
	}
}

func (cli *SCasClient) getServerMetrics(opts *cloudprovider.MetricListOptions) ([]cloudprovider.MetricValues, error) {
	if len(opts.ResourceId) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "ResourceId (vm uuid)")
	}
	mon, err := cli.GetVmMonitor(opts.ResourceId)
	if err != nil {
		return nil, err
	}
	ts := monitorTimestamp(0, mon.Net, mon.Disk)
	id := opts.ResourceId
	ret := []cloudprovider.MetricValues{}

	want := func(mt cloudprovider.TMetricType) bool {
		return len(opts.MetricType) == 0 || opts.MetricType == mt
	}

	if want(cloudprovider.VM_METRIC_TYPE_CPU_USAGE) && mon.CpuRate != nil {
		ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_CPU_USAGE, "%", ts, *mon.CpuRate))
	}
	if want(cloudprovider.VM_METRIC_TYPE_MEM_USAGE) && mon.MemRate != nil {
		ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_MEM_USAGE, "%", ts, *mon.MemRate))
	}

	var rxFlow, txFlow float64
	for _, n := range mon.Net {
		rxFlow += n.ReadFlow
		txFlow += n.WriteFlow
	}
	if len(mon.Net) > 0 {
		if want(cloudprovider.VM_METRIC_TYPE_NET_BPS_RX) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_NET_BPS_RX, "bps", ts, rxFlow*NET_FLOW_TO_BITS_PER_SEC))
		}
		if want(cloudprovider.VM_METRIC_TYPE_NET_BPS_TX) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_NET_BPS_TX, "bps", ts, txFlow*NET_FLOW_TO_BITS_PER_SEC))
		}
	}

	var rdBps, wrBps, rdIops, wrIops float64
	for _, d := range mon.Disk {
		rdBps += d.ReadStat
		wrBps += d.WriteStat
		rdIops += d.ReadReqest
		wrIops += d.WriteReqest
	}
	if len(mon.Disk) > 0 {
		if want(cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_BPS) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_BPS, "Bps", ts, rdBps*DISK_STAT_TO_BYTES_PER_SEC))
		}
		if want(cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_BPS) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_BPS, "Bps", ts, wrBps*DISK_STAT_TO_BYTES_PER_SEC))
		}
		if want(cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_IOPS) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_DISK_IO_READ_IOPS, "count/s", ts, rdIops*DISK_REQ_TO_IOPS))
		}
		if want(cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_IOPS) {
			ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_DISK_IO_WRITE_IOPS, "count/s", ts, wrIops*DISK_REQ_TO_IOPS))
		}
	}

	if len(mon.Partition) > 0 && want(cloudprovider.VM_METRIC_TYPE_DISK_USAGE) {
		// Weighted by size when available; else simple average.
		var sumSize, sumUsed float64
		var sumUsage, n float64
		for _, p := range mon.Partition {
			if p.Size > 0 {
				sumSize += p.Size
				sumUsed += p.Size * p.Usage / 100
			} else {
				sumUsage += p.Usage
				n++
			}
		}
		usage := 0.0
		if sumSize > 0 {
			usage = sumUsed / sumSize * 100
		} else if n > 0 {
			usage = sumUsage / n
		}
		ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_DISK_USAGE, "%", ts, usage))
	}

	if IncludeFlowLinkAsTCPConnections && want(cloudprovider.VM_METRIC_TYPE_NET_TCP_CONNECTION) {
		// UNVERIFIED semantics; included because the metric type exists.
		ret = append(ret, onePoint(id, cloudprovider.VM_METRIC_TYPE_NET_TCP_CONNECTION, "count", ts, mon.FlowLink))
	}

	if len(ret) == 0 {
		log.Debugf("cas GetMetrics %s: monitor returned no mappable fields", id)
	}
	return ret, nil
}
