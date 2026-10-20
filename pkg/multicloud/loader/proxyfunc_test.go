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

package loader

import (
	"net/http"
	"net/url"
	"testing"

	"yunion.io/x/pkg/util/httputils"

	"yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestProxyFunc(t *testing.T) {
	runT := func(t *testing.T, cpcfg *cloudprovider.ProviderConfig) {
		vendor := cpcfg.Vendor
		proxied := false
		proxyFunc := func(req *http.Request) (*url.URL, error) {
			proxied = true
			return url.Parse("http://TestProxyFunc" + "." + vendor + "/")
		}
		cpcfg.ProxyFunc = proxyFunc
		_, err := cloudprovider.GetProvider(*cpcfg)
		if !proxied {
			if err != nil {
				t.Logf("vendor %s: err: %v", vendor, err)
			}
			t.Errorf("vendor %s: proxyFunc not working", vendor)
		}
		t.Run("default client no proxy", func(t *testing.T) {
			proxied = false
			client := httputils.GetDefaultClient()
			client.Get("http://default-client-no-proxy.TestProxyFunc." + vendor + "/")
			if proxied {
				t.Errorf("%s: default client proxy changed", vendor)
			}
		})
	}

	t.Parallel()
	t.Run("cloud", func(t *testing.T) {
		cpcfgs := map[string]*cloudprovider.ProviderConfig{
			compute.CLOUD_PROVIDER_CAS: &cloudprovider.ProviderConfig{},
		}
		for vendor := range cpcfgs {
			cpcfgs[vendor].Vendor = vendor
			cpcfgs[vendor].Id = vendor + "-Id"
			cpcfgs[vendor].Name = vendor + "-Name"
			cpcfgs[vendor].URL = "http://" + vendor + ".AccessUrl/TestProxyFunc"
			cpcfgs[vendor].Account = vendor + "-Account"
			cpcfgs[vendor].Secret = vendor + "-Secret"
		}
		for _, cpcfg := range cpcfgs {
			cpcfg := cpcfg
			t.Run(cpcfg.Vendor, func(t *testing.T) {
				runT(t, cpcfg)
			})
		}
	})
}
