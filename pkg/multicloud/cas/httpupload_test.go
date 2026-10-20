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
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// httpCAS adds the nova/cinder endpoints to impFake. nova/download really
// fetches the image from the cmx stub over HTTP, like the CAS helper.
type httpCAS struct {
	*impFake
	nova map[string][]byte // /vms/nova/<uuid> -> content

	nova404, cinder404 bool   // endpoint missing
	novaReject         int    // nova/download -> this status (no task)
	novaNever          bool   // nova/download task never completes
	cfiFail            bool   // createFromImage task fails
	wrongToken         bool   // CAS sends a wrong x-auth-token
	echoToken          bool   // 409 whose Error-Message echoes the token
	fetchStatus        []int  // stub status codes seen by "CAS"
	fetchBytes         []int  // body bytes received per fetch
	lastToken          string // token CAS was given
	lastUuid           string
}

func newHTTPCAS(t *testing.T) *httpCAS {
	h := &httpCAS{impFake: newImpFake(t), nova: map[string][]byte{}}
	h.volumes = nil // the source only appears after createFromImage
	h.extra = h.serve
	return h
}

func (h *httpCAS) fetch(host string, port int, uuid, token string) bool {
	if h.wrongToken {
		token = "not-" + token
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://%s/v1/images/%s", net.JoinHostPort(host, fmt.Sprint(port)), uuid), nil)
	req.Header.Set("x-auth-token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Errorf("fetch: %v", err)
		return false
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	h.fetchStatus = append(h.fetchStatus, resp.StatusCode)
	h.fetchBytes = append(h.fetchBytes, len(data))
	if resp.StatusCode != 200 {
		return false
	}
	h.nova[uuid] = data
	return true
}

func (h *httpCAS) serve(w http.ResponseWriter, r *http.Request, path string, q url.Values, body []byte) bool {
	switch {
	case r.Method == "GET" && path == "hostpool/all":
		reply(w, dict{"hostPool": []dict{{"id": 1, "name": "hp1"}}})
	case r.Method == "GET" && path == "nova/image/ifExists":
		data, ok := h.nova[q.Get("uuid")]
		switch {
		case !ok:
			w.Write([]byte("0"))
		case fmt.Sprint(len(data)) != q.Get("size"):
			w.Write([]byte("0"))
		default:
			sum := md5.Sum(data)
			if c := q.Get("checksum"); len(c) > 0 && c != hex.EncodeToString(sum[:]) {
				w.Write([]byte("0"))
			} else {
				w.Write([]byte("1"))
			}
		}
	case r.Method == "POST" && path == "nova/download":
		if h.nova404 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
			h.t.Errorf("nova/download content-type %s", ct)
		}
		b := &SNovaDownload{}
		if err := xml.Unmarshal(body, b); err != nil {
			h.t.Errorf("nova/download xml: %v", err)
		}
		h.lastToken, h.lastUuid = b.Token, b.Uuid
		if h.novaReject > 0 || h.echoToken {
			st := h.novaReject
			if h.echoToken {
				w.Header().Set("Error-Message", "bad token "+b.Token)
				st = http.StatusConflict
			}
			w.Header().Set("Error-Code", "999")
			w.WriteHeader(st)
			return true
		}
		if h.novaNever {
			h.task(w, 8010)
			return true
		}
		if !h.fetch(b.Host, b.Port, b.Uuid, b.Token) {
			h.failTask[8010] = true
		}
		h.task(w, 8010)
	case r.Method == "PUT" && path == "cinder/download":
		if h.cinder404 {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		b := &SCinderDownload{}
		obj, _ := jsonutils.Parse(body)
		obj.Unmarshal(b)
		h.lastToken, h.lastUuid = b.Token, b.Uuid
		if !h.fetch(b.Host, b.Port, b.Uuid, b.Token) {
			h.failTask[8011] = true
		}
		h.task(w, 8011)
	case r.Method == "POST" && path == "cinder/volume/createFromImage":
		obj, _ := jsonutils.Parse(body)
		img, _ := obj.GetString("image")
		name, _ := obj.GetString("name")
		if h.cfiFail || h.nova[img] == nil {
			h.failTask[8012] = true
		} else {
			h.volumes = append(h.volumes, dict{"name": name, "format": "qcow2", "size": 2252.5, "storagePoolName": "defaultpool"})
		}
		h.task(w, 8012)
	case r.Method == "DELETE" && strings.HasPrefix(path, "nova/image/"):
		uuid := strings.TrimPrefix(path, "nova/image/")
		if _, ok := h.nova[uuid]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		delete(h.nova, uuid)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, "message/"):
		if id := strings.TrimPrefix(path, "message/"); id == "8010" && h.novaNever {
			reply(w, dict{"msgId": 8010, "completed": false})
			return true
		}
		return false
	default:
		return false
	}
	return true
}

// countingStager wraps SHTTPStager.listen to count listeners and record
// the bound address.
func httpStager(t *testing.T, content []byte, listens *int32, addr *string) *SHTTPStager {
	st := NewHTTPStager(SHTTPUploadOptions{LocalPath: writeLocal(t, content), ServeAddr: "127.0.0.1:0"})
	st.listen = func(network, a string) (net.Listener, error) {
		atomic.AddInt32(listens, 1)
		ln, err := net.Listen(network, a)
		if err == nil && addr != nil {
			*addr = ln.Addr().String()
		}
		return ln, err
	}
	return st
}

func (h *httpCAS) hasWrite(prefix string) bool {
	for _, w := range h.writes() {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

func assertNoToken(t *testing.T, token string, things ...interface{}) {
	t.Helper()
	if len(token) == 0 {
		t.Fatal("no token captured")
	}
	for _, x := range things {
		for _, s := range []string{fmt.Sprintf("%v", x), fmt.Sprintf("%+v", x), fmt.Sprintf("%#v", x), jsonutils.Marshal(x).String()} {
			if strings.Contains(s, token) {
				t.Fatalf("token leaked: %s", s)
			}
		}
	}
}

func TestImportQcow2HTTPUpload(t *testing.T) {
	h := newHTTPCAS(t)
	content := []byte("qcow2 cloud image bytes")
	var listens int32
	var bound string
	st := httpStager(t, content, &listens, &bound)
	o := impOpts()
	o.Stager = st
	res, err := h.client(t).ImportQcow2AsTemplate(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(h.nova[h.lastUuid]) != string(content) || len(h.fetchStatus) != 1 || h.fetchStatus[0] != 200 {
		t.Fatalf("CAS fetch: %v %q", h.fetchStatus, h.nova[h.lastUuid])
	}
	if len(h.lastToken) != 64 || len(h.lastUuid) != 36 {
		t.Fatalf("token/uuid shape %d %q", len(h.lastToken), h.lastUuid)
	}
	b := &SNovaDownload{}
	xml.Unmarshal([]byte(h.bodies["POST nova/download"]), b)
	sum := md5.Sum(content)
	if b.DestFile != "/vms/nova/"+h.lastUuid || b.ImageType != "qcow2" || b.Size != int64(len(content)) || b.Checksum != hex.EncodeToString(sum[:]) || b.Host != "127.0.0.1" || b.Port == 0 {
		t.Fatalf("nova/download body %+v", b)
	}
	cfi, _ := jsonutils.ParseString(h.bodies["POST cinder/volume/createFromImage"])
	for k, v := range map[string]string{"name": impSource, "image": h.lastUuid, "location": "defaultpool", "hpName": "hp1", "format": "qcow2"} {
		if got, _ := cfi.GetString(k); got != v {
			t.Errorf("createFromImage %s = %q, want %q", k, got, v)
		}
	}
	if lc, err := cfi.Bool("linkClone"); err != nil || lc {
		t.Errorf("linkClone must be sent as false: %v %v", lc, err)
	}
	want := []string{"POST /cas/casrs/nova/download", "POST /cas/casrs/cinder/volume/createFromImage", "GET /cas/casrs/storage/refresh", "POST /cas/casrs/storage/volume", "POST /cas/casrs/vm/add"}
	w := h.writes()
	for i := range want {
		if !strings.HasPrefix(w[i], want[i]) {
			t.Fatalf("write %d = %s, want %s (%v)", i, w[i], want[i], w)
		}
	}
	if h.hasWrite("DELETE /cas/casrs/nova/image/") {
		t.Error("staging file deleted after createFromImage")
	}
	if listens != 1 {
		t.Fatalf("listens = %d", listens)
	}
	if c, err := net.DialTimeout("tcp", bound, time.Second); err == nil {
		c.Close()
		t.Fatalf("stub still listening on %s after the import", bound)
	}
	if res.Steps[0].Step != "http-upload" || !res.Steps[0].Done {
		t.Fatalf("steps %+v", res.Steps)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "/vms/nova/"+h.lastUuid+" was kept") {
		t.Errorf("warnings %v", res.Warnings)
	}
	assertNoToken(t, h.lastToken, res, st)
	h.assertSourceUntouched(t)
}

// Wrong token: the stub answers 401 with no body bytes, the download
// task fails, and the (absent) staging file is cleaned up safely.
func TestImportQcow2HTTPWrongTokenRejected(t *testing.T) {
	h := newHTTPCAS(t)
	h.wrongToken = true
	var listens int32
	st := httpStager(t, []byte("secret image"), &listens, nil)
	o := impOpts()
	o.Stager = st
	res, err := h.client(t).ImportQcow2AsTemplate(o)
	if err == nil {
		t.Fatal("want error")
	}
	if len(h.fetchStatus) != 1 || (h.fetchStatus[0] != 401 && h.fetchStatus[0] != 403) || h.fetchBytes[0] != 0 {
		t.Fatalf("stub answer %v, %v bytes", h.fetchStatus, h.fetchBytes)
	}
	if h.hasWrite("POST /cas/casrs/cinder/volume/createFromImage") || h.hasWrite("POST /cas/casrs/storage/volume") {
		t.Fatalf("continued after failed download: %v", h.writes())
	}
	if !h.hasWrite("DELETE /cas/casrs/nova/image/" + h.lastUuid) {
		t.Errorf("safe cleanup of the staging file not attempted: %v", h.writes())
	}
	assertNoToken(t, h.lastToken, res, err.Error(), st)
}

func TestImportQcow2HTTPFallbackCinderDownload(t *testing.T) {
	h := newHTTPCAS(t)
	h.nova404 = true
	var listens int32
	o := impOpts()
	o.Stager = httpStager(t, []byte("img"), &listens, nil)
	if _, err := h.client(t).ImportQcow2AsTemplate(o); err != nil {
		t.Fatal(err)
	}
	cb, _ := jsonutils.ParseString(h.bodies["PUT cinder/download"])
	if d, _ := cb.GetString("destFile"); d != "/vms/nova/"+h.lastUuid {
		t.Fatalf("cinder/download body %s", cb)
	}
	for _, k := range []string{"hpName", "location", "srcFile"} {
		if cb.Contains(k) {
			t.Errorf("cinder/download must not send %s", k)
		}
	}
	if string(h.nova[h.lastUuid]) != "img" {
		t.Fatal("not fetched via cinder/download")
	}
}

func TestImportQcow2HTTPBothDownloads404(t *testing.T) {
	h := newHTTPCAS(t)
	h.nova404, h.cinder404 = true, true
	var listens int32
	o := impOpts()
	o.Stager = httpStager(t, []byte("img"), &listens, nil)
	_, err := h.client(t).ImportQcow2AsTemplate(o)
	if err == nil || !strings.Contains(err.Error(), "neither POST nova/download nor PUT cinder/download exists") {
		t.Fatalf("err = %v", err)
	}
	if h.hasWrite("DELETE ") || h.hasWrite("POST /cas/casrs/cinder/volume/createFromImage") {
		t.Fatalf("writes %v", h.writes())
	}
}

// The staging file is kept (and reported) whenever deleting it is not
// known to be safe.
func TestImportQcow2HTTPStagingKeptWhenUnsafe(t *testing.T) {
	cases := map[string]struct {
		mod      func(h *httpCAS)
		keepWarn bool
	}{
		"download timeout":      {func(h *httpCAS) { h.novaNever = true }, true},
		"createFromImage fails": {func(h *httpCAS) { h.cfiFail = true }, true},
		// 409/408/429/499 are ambiguous (R4): CAS may have started it
		"download 409": {func(h *httpCAS) { h.novaReject = 409 }, true},
		"download 429": {func(h *httpCAS) { h.novaReject = 429 }, true},
		// a definite 400 created nothing; nothing to keep or delete
		"download rejected 400": {func(h *httpCAS) { h.novaReject = 400 }, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHTTPCAS(t)
			c.mod(h)
			var listens int32
			var bound string
			o := impOpts()
			o.Stager = httpStager(t, []byte("img"), &listens, &bound)
			cli := h.client(t)
			cli.taskTimeout = 300 * time.Millisecond
			res, err := cli.ImportQcow2AsTemplate(o)
			if err == nil {
				t.Fatal("want error")
			}
			if h.hasWrite("DELETE /cas/casrs/nova/image/") {
				t.Fatalf("staging file deleted: %v", h.writes())
			}
			kept := strings.Contains(strings.Join(res.Warnings, "\n"), "was kept")
			if kept != c.keepWarn {
				t.Errorf("kept warning = %v, want %v (%v)", kept, c.keepWarn, res.Warnings)
			}
			if conn, err := net.DialTimeout("tcp", bound, time.Second); err == nil {
				conn.Close()
				t.Fatal("stub still listening after failure")
			}
		})
	}
}

// A CAS error that echoes the token must not leak it.
func TestImportQcow2HTTPTokenRedactedInErrors(t *testing.T) {
	h := newHTTPCAS(t)
	h.echoToken = true
	var listens int32
	st := httpStager(t, []byte("img"), &listens, nil)
	o := impOpts()
	o.Stager = st
	res, err := h.client(t).ImportQcow2AsTemplate(o)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "bad token "+redacted) {
		t.Errorf("err = %v", err)
	}
	assertNoToken(t, h.lastToken, err.Error(), res, st)
}

func TestImportQcow2HTTPDryRunAndReadOnlyNoServer(t *testing.T) {
	for _, mode := range []string{"dry-run", "read-only"} {
		t.Run(mode, func(t *testing.T) {
			h := newHTTPCAS(t)
			var listens int32
			o := impOpts()
			o.Stager = httpStager(t, []byte("img"), &listens, nil)
			cli := h.client(t)
			if mode == "dry-run" {
				o.DryRun = true
			} else {
				cli.cpcfg.ReadOnly = true
			}
			res, err := cli.ImportQcow2AsTemplate(o)
			if mode == "dry-run" {
				if err != nil || res.Steps[0].Step != "http-upload" || res.Steps[0].Done || !strings.Contains(res.Steps[0].Detail, "redacted") {
					t.Fatalf("%v %+v", err, res)
				}
			} else if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
				t.Fatalf("err = %v", err)
			}
			if listens != 0 {
				t.Fatalf("stub started %d times", listens)
			}
			if w := h.writes(); len(w) != 0 {
				t.Fatalf("writes %v", w)
			}
			if len(h.fetchStatus) != 0 {
				t.Fatal("fetched")
			}
		})
	}
}

func TestHTTPStagerValidate(t *testing.T) {
	local := writeLocal(t, []byte("x"))
	for name, o := range map[string]SHTTPUploadOptions{
		"no serve addr":      {LocalPath: local},
		"unspecified host":   {LocalPath: local, ServeAddr: "0.0.0.0:9292"},
		"advertise port 0":   {LocalPath: local, ServeAddr: "0.0.0.0:0", AdvertiseAddr: "198.51.100.7:0"},
		"bad image type":     {LocalPath: local, ServeAddr: "127.0.0.1:1", ImageType: "vmdk"},
		"missing local file": {LocalPath: "/nonexistent/x.qcow2", ServeAddr: "127.0.0.1:1"},
	} {
		if _, err := NewHTTPStager(o).Plan(SImportTarget{}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	ok := NewHTTPStager(SHTTPUploadOptions{LocalPath: local, ServeAddr: "0.0.0.0:9292", AdvertiseAddr: "198.51.100.7:9292"})
	if plan, err := ok.Plan(SImportTarget{FileName: "a.qcow2", PoolName: "p"}); err != nil || !strings.Contains(plan.Detail, "http://198.51.100.7:9292") {
		t.Fatalf("%v %+v", err, plan)
	}
}

// The stub itself: wrong/missing token -> 401 and no bytes; other paths
// 404; HEAD gives metadata only.
func TestGlanceStub(t *testing.T) {
	local := writeLocal(t, []byte("abcdef"))
	g := &glanceStub{uuid: "u-1", token: "tok", path: local, size: 6, md5: "m", diskFormat: "qcow2"}
	srv := &http.Server{Handler: g}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()
	do := func(method, p, tok string) (int, string, http.Header) {
		req, _ := http.NewRequest(method, base+p, nil)
		if len(tok) > 0 {
			req.Header.Set("x-auth-token", tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}
	if c, b, _ := do("GET", "/v1/images/u-1", ""); c != 401 || len(b) != 0 {
		t.Errorf("no token: %d %q", c, b)
	}
	if c, b, _ := do("GET", "/v1/images/u-1", "tok2"); c != 401 || len(b) != 0 {
		t.Errorf("bad token: %d %q", c, b)
	}
	if c, _, _ := do("GET", "/v1/images/other", "tok"); c != 404 {
		t.Errorf("other uuid: %d", c)
	}
	if c, _, _ := do("PUT", "/v1/images/u-1", "tok"); c != 404 {
		t.Errorf("PUT: %d", c)
	}
	if c, b, hd := do("HEAD", "/v1/images/u-1", "tok"); c != 200 || len(b) != 0 || hd.Get("x-image-meta-size") != "6" {
		t.Errorf("HEAD: %d %q %v", c, b, hd)
	}
	if c, b, hd := do("GET", "/v1/images/u-1", "tok"); c != 200 || b != "abcdef" || hd.Get("x-image-meta-checksum") != "m" {
		t.Errorf("GET: %d %q", c, b)
	}
	if g.served != 1 || g.rejected != 2 {
		t.Errorf("served %d rejected %d", g.served, g.rejected)
	}
}

func TestNovaImageExistsBodies(t *testing.T) {
	for body, want := range map[string]int{"0": 0, " 1\n": 1, "<int>2</int>": 2, `{"result":1}`: 1} {
		f := newImpFake(t)
		b := body
		f.extra = func(w http.ResponseWriter, r *http.Request, path string, q url.Values, _ []byte) bool {
			w.Write([]byte(b))
			return true
		}
		got, err := f.client(t).NovaImageExists("u", 1, "")
		if err != nil || got != want {
			t.Errorf("%q: %d %v", body, got, err)
		}
	}
}

func TestRedactToken(t *testing.T) {
	err := redactToken(errors.Error("x tok123 y tok123"), "tok123")
	if strings.Contains(err.Error(), "tok123") || !strings.Contains(err.Error(), redacted) {
		t.Fatal(err)
	}
	if redactToken(nil, "t") != nil {
		t.Fatal("nil")
	}
}
