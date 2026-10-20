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
	"strings"

	"yunion.io/x/jsonutils"
)

const redacted = "******"

// isSecretKey matches credential-looking keys, case-insensitively. It was
// derived from every property/parameter name in api.yaml matching
// pwd|passw|token|secret|key|auth:
//   - ends in "pw"/"pwd": pw, pwd, ipmiPw, encryptPw, netconfPw, hostPwd,
//     vncPwd, cephPwd, slavePwd, arbitrationHostPwd;
//   - contains "password"/"passwd"/"secret"/"privatekey"/"credential":
//     password, vncPassword, loginPassword, passwd, secretKey, Password
//     on backupFileList, privateKey; (also masks harmless isSecret,
//     secretLevel, diskSecret, passwdMode);
//   - ends in "token" or "authkey": token, authKey;
//   - productKey (a licence key).
//
// Not masked: identifier keys (hostKey, clusterKey, domainKey are "unique
// identifier" per api.yaml), key, keyValue, pageKey, descPropertyKey,
// authType.
func isSecretKey(key string) bool {
	k := strings.ToLower(key)
	for _, suffix := range []string{"pw", "pwd", "token", "authkey"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	for _, sub := range []string{"password", "passwd", "secret", "privatekey", "credential"} {
		if strings.Contains(k, sub) {
			return true
		}
	}
	return k == "productkey"
}

func redactJSON(obj jsonutils.JSONObject) jsonutils.JSONObject {
	switch v := obj.(type) {
	case *jsonutils.JSONDict:
		ret := jsonutils.NewDict()
		m, _ := v.GetMap()
		for k, val := range m {
			if isSecretKey(k) {
				ret.Set(k, jsonutils.NewString(redacted))
			} else {
				ret.Set(k, redactJSON(val))
			}
		}
		return ret
	case *jsonutils.JSONArray:
		arr, _ := v.GetArray()
		ret := jsonutils.NewArray()
		for i := range arr {
			ret.Add(redactJSON(arr[i]))
		}
		return ret
	}
	return obj
}

// redactBody renders a response body for logs and errors with secrets
// masked. Bodies that are not JSON are not rendered at all, since they
// cannot be masked reliably.
func redactBody(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	obj, err := jsonutils.Parse(data)
	if err != nil {
		return fmt.Sprintf("<%d bytes non-JSON body omitted>", len(data))
	}
	return redactJSON(obj).String()
}
