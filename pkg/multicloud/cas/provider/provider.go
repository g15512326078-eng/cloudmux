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

package provider

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/regutils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

type SCasProviderFactory struct {
	cloudprovider.SPrivateCloudBaseProviderFactory
}

func (self *SCasProviderFactory) GetId() string {
	return cas.CLOUD_PROVIDER_CAS
}

func (self *SCasProviderFactory) GetName() string {
	return cas.CLOUD_PROVIDER_CAS
}

func (self *SCasProviderFactory) ValidateChangeBandwidth(instanceId string, bandwidth int64) error {
	return fmt.Errorf("Changing %s bandwidth is not supported", cas.CLOUD_PROVIDER_CAS)
}

// DEFAULT_SCHEME is used when only host/port are given. api.yaml declares
// "schemes: [http]" (api.yaml:99-100) and nothing else, so http is the
// documented default; deployments serving https must pass a full URL.
const DEFAULT_SCHEME = "http"

// defaultPorts are the CVM Tomcat connectors observed in the field:
// http://host:8080 and https://host:8443 (TLS 1.2, self-signed).
var defaultPorts = map[string]int{"http": 8080, "https": 8443}

// ValidateCreateCloudaccountData accepts either a full URL in AuthUrl
// (scheme://host[:port], scheme http or https) or Host [+ Port] with
// DEFAULT_SCHEME. A missing port defaults to 8080 for http and 8443 for
// https.
func (self *SCasProviderFactory) ValidateCreateCloudaccountData(ctx context.Context, input cloudprovider.SCloudaccountCredential) (cloudprovider.SCloudaccount, error) {
	output := cloudprovider.SCloudaccount{}
	if len(input.Username) == 0 {
		return output, errors.Wrap(cloudprovider.ErrMissingParameter, "username")
	}
	if len(input.Password) == 0 {
		return output, errors.Wrap(cloudprovider.ErrMissingParameter, "password")
	}
	accessUrl, err := buildAccessUrl(input)
	if err != nil {
		return output, err
	}
	output.AccessUrl = accessUrl
	output.Account = input.Username
	output.Secret = input.Password
	return output, nil
}

func buildAccessUrl(input cloudprovider.SCloudaccountCredential) (string, error) {
	scheme, host, port := DEFAULT_SCHEME, input.Host, input.Port
	if len(input.AuthUrl) > 0 {
		u, err := url.Parse(input.AuthUrl)
		if err != nil {
			return "", errors.Wrapf(cloudprovider.ErrInputParameter, "auth_url %q: %v", input.AuthUrl, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", errors.Wrapf(cloudprovider.ErrInputParameter, "auth_url scheme must be http or https, got %q", u.Scheme)
		}
		if len(strings.Trim(u.Path, "/")) > 0 {
			return "", errors.Wrapf(cloudprovider.ErrInputParameter, "auth_url must not have a path (/cas/casrs is added by the client)")
		}
		scheme, host = u.Scheme, u.Hostname()
		port = 0
		if len(u.Port()) > 0 {
			port, err = strconv.Atoi(u.Port())
			if err != nil {
				return "", errors.Wrapf(cloudprovider.ErrInputParameter, "auth_url port %q", u.Port())
			}
		}
	}
	if len(host) == 0 {
		return "", errors.Wrap(cloudprovider.ErrMissingParameter, "host")
	}
	if !regutils.MatchIPAddr(host) && !regutils.MatchDomainName(host) {
		return "", errors.Wrap(cloudprovider.ErrInputParameter, "host should be ip or domain name")
	}
	if port == 0 {
		port = defaultPorts[scheme]
	}
	if port < 0 || port > 65535 {
		return "", errors.Wrapf(cloudprovider.ErrInputParameter, "port %d out of range", port)
	}
	return fmt.Sprintf("%s://%s:%d", scheme, host, port), nil
}

func (self *SCasProviderFactory) ValidateUpdateCloudaccountCredential(ctx context.Context, input cloudprovider.SCloudaccountCredential, cloudaccount string) (cloudprovider.SCloudaccount, error) {
	output := cloudprovider.SCloudaccount{}
	if len(input.Username) == 0 {
		return output, errors.Wrap(cloudprovider.ErrMissingParameter, "username")
	}
	if len(input.Password) == 0 {
		return output, errors.Wrap(cloudprovider.ErrMissingParameter, "password")
	}
	output = cloudprovider.SCloudaccount{
		Account: input.Username,
		Secret:  input.Password,
	}
	return output, nil
}

func (self *SCasProviderFactory) GetProvider(cfg cloudprovider.ProviderConfig) (cloudprovider.ICloudProvider, error) {
	client, err := cas.NewCasClient(
		cas.NewCasClientConfig(cfg.URL, cfg.Account, cfg.Secret).CloudproviderConfig(cfg),
	)
	if err != nil {
		return nil, err
	}
	return &SCasProvider{
		SBaseProvider: cloudprovider.NewBaseProvider(self),
		client:        client,
	}, nil
}

func (self *SCasProviderFactory) GetClientRC(info cloudprovider.SProviderInfo) (map[string]string, error) {
	return map[string]string{
		"CAS_ACCESS_URL": info.Url,
		"CAS_USERNAME":   info.Account,
		"CAS_PASSWORD":   info.Secret,
	}, nil
}

func init() {
	factory := SCasProviderFactory{}
	cloudprovider.RegisterFactory(&factory)
}

type SCasProvider struct {
	cloudprovider.SBaseProvider
	client *cas.SCasClient
}

func (self *SCasProvider) GetSysInfo() (jsonutils.JSONObject, error) {
	return jsonutils.NewDict(), nil
}

// GetVersion returns casVersion from GET /cas/casrs/version, or "" when the
// endpoint is unavailable on this CAS build.
func (self *SCasProvider) GetVersion() string {
	version, err := self.client.GetVersion()
	if err != nil {
		log.Warningf("cas GetVersion: %v", err)
		return ""
	}
	return version
}

func (self *SCasProvider) GetSubAccounts() ([]cloudprovider.SSubAccount, error) {
	return self.client.GetSubAccounts()
}

func (self *SCasProvider) GetAccountId() string {
	return self.client.GetAccountId()
}

func (self *SCasProvider) GetCloudRegionExternalIdPrefix() string {
	return self.client.GetCloudRegionExternalIdPrefix()
}

func (self *SCasProvider) GetIRegions() ([]cloudprovider.ICloudRegion, error) {
	return self.client.GetIRegions()
}

func (self *SCasProvider) GetIRegionById(id string) (cloudprovider.ICloudRegion, error) {
	regions, err := self.GetIRegions()
	if err != nil {
		return nil, err
	}
	for i := range regions {
		if regions[i].GetGlobalId() == id {
			return regions[i], nil
		}
	}
	return nil, cloudprovider.ErrNotFound
}

func (self *SCasProvider) GetClient() *cas.SCasClient {
	return self.client
}

func (self *SCasProvider) GetMetrics(opts *cloudprovider.MetricListOptions) ([]cloudprovider.MetricValues, error) {
	return self.client.GetMetrics(opts)
}

func (self *SCasProvider) GetBalance() (*cloudprovider.SBalanceInfo, error) {
	return &cloudprovider.SBalanceInfo{
		Amount:   0.0,
		Currency: "CNY",
		Status:   api.CLOUD_PROVIDER_HEALTH_NORMAL,
	}, cloudprovider.ErrNotSupported
}

func (self *SCasProvider) GetIProjects() ([]cloudprovider.ICloudProject, error) {
	return []cloudprovider.ICloudProject{}, nil
}

func (self *SCasProvider) GetStorageClasses(regionId string) []string {
	return nil
}

func (self *SCasProvider) GetBucketCannedAcls(regionId string) []string {
	return nil
}

func (self *SCasProvider) GetObjectCannedAcls(regionId string) []string {
	return nil
}

func (self *SCasProvider) GetCapabilities() []string {
	return self.client.GetCapabilities()
}

var (
	_ cloudprovider.ICloudProvider        = (*SCasProvider)(nil)
	_ cloudprovider.ICloudProviderFactory = (*SCasProviderFactory)(nil)
)
