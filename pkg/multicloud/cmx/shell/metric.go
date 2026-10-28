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

package shell

import (
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	casprovider "yunion.io/x/cloudmux/pkg/multicloud/cas/provider"
)

func init() {
	cmd := NewCommand("metric")
	type MetricListOptions struct {
		cloudprovider.MetricListOptions
	}
	ProviderR[MetricListOptions](cmd).Run("list", "List CAS VM metrics via GetMetrics (vm/{uuid}/monitor)", func(cli cloudprovider.ICloudProvider, args *MetricListOptions) (any, error) {
		if _, ok := cli.(*casprovider.SCasProvider); !ok {
			return nil, errors.Wrap(cloudprovider.ErrNotSupported, "metric-list requires CAS provider")
		}
		if args.StartTime.IsZero() {
			args.StartTime = time.Now().Add(-time.Hour)
		}
		if args.EndTime.IsZero() {
			args.EndTime = time.Now()
		}
		if len(args.ResourceType) == 0 {
			args.ResourceType = cloudprovider.METRIC_RESOURCE_TYPE_SERVER
		}
		return cli.GetMetrics(&args.MetricListOptions)
	})
}
