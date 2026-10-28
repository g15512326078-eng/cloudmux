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
	"testing"

	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func TestFactoryRegistered(t *testing.T) {
	factory, err := cloudprovider.GetProviderFactory(api.CLOUD_PROVIDER_CAS)
	if err != nil {
		t.Fatalf("GetProviderFactory(CAS): %v", err)
	}
	if factory.IsPublicCloud() || factory.IsOnPremise() {
		t.Errorf("CAS should be a private cloud provider")
	}
}

func TestValidateCreateCloudaccountData(t *testing.T) {
	factory := &SCasProviderFactory{}
	base := cloudprovider.SCloudaccountCredential{Username: "admin", Password: "pw"}
	cases := []struct {
		name    string
		host    string
		port    int
		authUrl string
		want    string
		err     error
	}{
		{name: "default scheme is http", host: "10.0.0.1", port: 8080, want: "http://10.0.0.1:8080"},
		{name: "https via auth_url", authUrl: "https://cas.example.com:8443", want: "https://cas.example.com:8443"},
		{name: "http via auth_url", authUrl: "http://10.0.0.2:8080/", want: "http://10.0.0.2:8080"},
		{name: "auth_url overrides host/port", host: "10.0.0.1", port: 1, authUrl: "https://10.0.0.3:443", want: "https://10.0.0.3:443"},
		{name: "missing port defaults to 8080", host: "10.0.0.1", want: "http://10.0.0.1:8080"},
		{name: "https auth_url without port defaults to 8443", authUrl: "https://10.0.0.1", want: "https://10.0.0.1:8443"},
		{name: "http auth_url without port defaults to 8080", authUrl: "http://10.0.0.1", want: "http://10.0.0.1:8080"},
		{name: "port out of range", host: "10.0.0.1", port: 70000, err: cloudprovider.ErrInputParameter},
		{name: "bad scheme", authUrl: "ftp://10.0.0.1:21", err: cloudprovider.ErrInputParameter},
		{name: "path not allowed", authUrl: "http://10.0.0.1:8080/cas/casrs", err: cloudprovider.ErrInputParameter},
		{name: "missing host", port: 8080, err: cloudprovider.ErrMissingParameter},
	}
	for _, c := range cases {
		input := base
		input.Host, input.Port, input.AuthUrl = c.host, c.port, c.authUrl
		out, err := factory.ValidateCreateCloudaccountData(context.Background(), input)
		if c.err != nil {
			if errors.Cause(err) != c.err {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.err)
			}
			continue
		}
		if err != nil || out.AccessUrl != c.want || out.Account != "admin" || out.Secret != "pw" {
			t.Errorf("%s: out = %+v, err = %v, want %s", c.name, out, err, c.want)
		}
	}
}
