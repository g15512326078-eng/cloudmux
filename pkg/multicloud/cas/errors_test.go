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
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

func gb18030(t *testing.T, s string) string {
	out, err := simplifiedchinese.GB18030.NewEncoder().String(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDecodeErrorMessage(t *testing.T) {
	gbk := gb18030(t, "虚拟机不存在。")
	cases := []struct {
		raw, charset, want string
	}{
		{"", "", ""},
		{"busy", "", "busy"},
		{"虚拟机不存在。", "", "虚拟机不存在。"},
		{"虚拟机不存在。", "UTF-8", "虚拟机不存在。"},
		{gbk, "", "虚拟机不存在。"},
		{gbk, "GB2312", "虚拟机不存在。"},
		{gbk, "gbk", "虚拟机不存在。"},
	}
	for _, c := range cases {
		if got := decodeErrorMessage(c.raw, c.charset); got != c.want {
			t.Errorf("decode(%q, %q) = %q, want %q", c.raw, c.charset, got, c.want)
		}
	}
}

func TestIsNotFoundByCode(t *testing.T) {
	cases := []struct {
		code, msg string
		want      bool
	}{
		{"1001", "", true},
		{"91606", "存储卷“x”不存在。", true},
		{"1900015", "", true},
		// a dependency of a live object is missing: never "VM gone"
		{"1011", "虚拟机的存储文件不存在。", false},
		{"212", "文件不存在。", false},
		{"843", "busy", false},
		// no code: message heuristic
		{"", "虚拟机不存在。", true},
		{"", "busy", false},
	}
	for _, c := range cases {
		e := &SCasError{ErrorCode: c.code, ErrorMessage: c.msg}
		if got := e.isNotFound(); got != c.want {
			t.Errorf("isNotFound(code=%q msg=%q) = %v, want %v", c.code, c.msg, got, c.want)
		}
	}
}

// A GB18030 Error-Message on the wire, as CAS sends it, is decoded and the
// Error-Code decides NotFound.
func TestRequestDecodesGbErrorMessage(t *testing.T) {
	gbk := gb18030(t, "虚拟机不存在。")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cas/casrs/gone":
			w.Header()["Error-Code"] = []string{"1001"}
			w.Header()["Error-Message"] = []string{gbk}
			w.Header()["Charset"] = []string{"GB2312"}
		case "/cas/casrs/file-missing":
			w.Header()["Error-Code"] = []string{"1011"}
			w.Header()["Error-Message"] = []string{gb18030(t, "虚拟机的存储文件不存在。")}
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
	err := cli.get("gone", nil, nil)
	if errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Fatalf("1001: err = %v, want ErrNotFound", err)
	}
	err = cli.get("file-missing", nil, nil)
	ce, ok := err.(*SCasError)
	if !ok || ce.ErrorCode != "1011" || ce.ErrorMessage != "虚拟机的存储文件不存在。" {
		t.Fatalf("1011: err = %#v, want decoded SCasError (not NotFound)", err)
	}
}
