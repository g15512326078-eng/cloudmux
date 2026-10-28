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

	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"
)

// SVncInfo is GET /cas/casrs/vmvnc/vnc/{id} (and the common fields of
// /vmvnc/novnc/{id}).
type SVncInfo struct {
	Ip       string `json:"ip"`
	Port     int    `json:"port"`
	Password string `json:"password"`
}

func (cli *SCasClient) GetVnc(id int64) (*SVncInfo, error) {
	resp, err := cli.request(httputils.GET, fmt.Sprintf("vmvnc/vnc/%d", id), nil, nil)
	if err != nil {
		return nil, err
	}
	info := &SVncInfo{}
	if err := unwrapRoot(resp).Unmarshal(info); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal vnc")
	}
	if len(info.Ip) == 0 || info.Port == 0 {
		return nil, errors.Wrapf(ErrEmptyBody, "vmvnc/vnc/%d missing ip/port", id)
	}
	return info, nil
}
