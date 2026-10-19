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
	input := cloudprovider.SCloudaccountCredential{Username: "admin", Password: "pw", Host: "10.0.0.1"}
	_, err := factory.ValidateCreateCloudaccountData(context.Background(), input)
	if errors.Cause(err) != cloudprovider.ErrMissingParameter {
		t.Errorf("missing port: err = %v", err)
	}
	input.Port = 8443
	out, err := factory.ValidateCreateCloudaccountData(context.Background(), input)
	if err != nil || out.AccessUrl != "https://10.0.0.1:8443" || out.Account != "admin" || out.Secret != "pw" {
		t.Errorf("out = %+v, err = %v", out, err)
	}
}
