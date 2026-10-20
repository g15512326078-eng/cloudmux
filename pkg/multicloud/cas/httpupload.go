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

// HTTP mode of the image import: CAS pulls the file from cmx instead of
// cmx pushing it over SSH. CAS only pulls from a Glance V1 endpoint, so
// cmx serves a one-route stub for the duration of Stage:
//
//	GET|HEAD /v1/images/<uuid>  with header x-auth-token: <token>
//
// and drives, over the usual Digest REST channel:
//
//  1. GET  nova/image/ifExists?uuid&size&checksum   (api.yaml A:45267)
//  2. POST nova/download, XML <image> body           (not in api.yaml;
//     shape from the CAS OpenStack drivers, see research doc 9.2); on a
//     plain HTTP 404 (endpoint missing) fall back to the deprecated
//     PUT cinder/download, JSON body              (api.yaml A:44865)
//  3. GET  nova/image/ifExists again: the file must now be present
//  4. POST cinder/volume/createFromImage, JSON    (api.yaml A:44522):
//     copy /vms/nova/<uuid> into the target pool as <file name>.
//
// The uuid and the token are fresh crypto/rand values of each run. The
// token is never logged or put into errors, String(), or plan output.
//
// Checksum: api.yaml only says "校验和" (checksum) for ifExists and has no
// nova/download schema. The drivers pass Glance's image checksum, and
// Glance V1 image checksums are MD5 hex, so md5 hex is sent [inferred,
// live check]; SkipChecksum leaves it out.
//
// The staging file /vms/nova/<uuid> is deleted (DELETE nova/image/<uuid>,
// the drivers' call; not in api.yaml) only when that is safe, see
// novaCleanupSafe. Otherwise it is kept and reported.

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	NOVA_IMAGE_DIR = "/vms/nova/"

	// nova/image/ifExists results, from the drivers (api.yaml only
	// documents 0 = absent).
	NOVA_IMAGE_ABSENT      = 0
	NOVA_IMAGE_PRESENT     = 1
	NOVA_IMAGE_DOWNLOADING = 2

	glanceTokenHeader = "x-auth-token"
)

type SHTTPUploadOptions struct {
	LocalPath string
	// ServeAddr is the host:port the stub listens on while Stage runs.
	ServeAddr string
	// AdvertiseAddr is the host:port CAS is told to fetch from (default
	// ServeAddr). It must name a concrete host the CAS node can reach.
	AdvertiseAddr string
	// ImageType is the nova/download imageType (default qcow2).
	ImageType string
	// SkipChecksum leaves the md5 out of ifExists and nova/download.
	SkipChecksum bool
}

func (o *SHTTPUploadOptions) String() string {
	return fmt.Sprintf("http-pull %s via %s (advertised %s)", o.LocalPath, o.ServeAddr, o.advertise())
}

func (o *SHTTPUploadOptions) advertise() string {
	if len(o.AdvertiseAddr) > 0 {
		return o.AdvertiseAddr
	}
	return o.ServeAddr
}

func (o *SHTTPUploadOptions) validate() (string, int, error) {
	if len(o.LocalPath) == 0 {
		return "", 0, errors.Wrap(cloudprovider.ErrMissingParameter, "local file")
	}
	if _, _, err := net.SplitHostPort(o.ServeAddr); err != nil {
		return "", 0, errors.Wrapf(cloudprovider.ErrInputParameter, "serve address %q must be host:port", o.ServeAddr)
	}
	host, portStr, err := net.SplitHostPort(o.advertise())
	if err != nil {
		return "", 0, errors.Wrapf(cloudprovider.ErrInputParameter, "advertised address %q must be host:port", o.advertise())
	}
	port, err := strconv.Atoi(portStr)
	// port 0 (pick a free port) is only usable when the advertised address
	// is the serve address: Stage then advertises the bound port.
	if err != nil || port < 0 || port > 65535 || (port == 0 && len(o.AdvertiseAddr) > 0) {
		return "", 0, errors.Wrapf(cloudprovider.ErrInputParameter, "advertised port %q", portStr)
	}
	if ip := net.ParseIP(host); len(host) == 0 || (ip != nil && ip.IsUnspecified()) {
		return "", 0, errors.Wrapf(cloudprovider.ErrInputParameter, "advertised host %q: give an address the CAS node can reach (--serve-advertise)", host)
	}
	switch o.imageType() {
	case "qcow2", "raw":
	default:
		return "", 0, errors.Wrapf(cloudprovider.ErrNotSupported, "image type %q", o.ImageType)
	}
	return host, port, nil
}

func (o *SHTTPUploadOptions) imageType() string {
	if len(o.ImageType) == 0 {
		return "qcow2"
	}
	return o.ImageType
}

// SHTTPStager is the http ImportSourceStager.
type SHTTPStager struct {
	Options SHTTPUploadOptions

	// set by Plan
	size   int64
	md5    string
	sha256 string

	// listen is net.Listen; tests may replace it.
	listen func(network, addr string) (net.Listener, error)
}

func NewHTTPStager(o SHTTPUploadOptions) *SHTTPStager {
	return &SHTTPStager{Options: o}
}

// String/GoString: the stager holds no secret (the token only lives inside
// Stage), but keep %v/%#v output short and stable.
func (s *SHTTPStager) String() string   { return s.Options.String() }
func (s *SHTTPStager) GoString() string { return s.Options.String() }

func fileDigests(p string) (int64, string, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", "", errors.Wrapf(err, "open %s", p)
	}
	defer f.Close()
	m, h := md5.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(m, h), f)
	if err != nil {
		return 0, "", "", errors.Wrapf(err, "read %s", p)
	}
	return n, hex.EncodeToString(m.Sum(nil)), hex.EncodeToString(h.Sum(nil)), nil
}

// Plan validates the options and hashes the local file. It does not
// listen and does not talk to CAS.
func (s *SHTTPStager) Plan(t SImportTarget) (*SImportStagePlan, error) {
	if _, _, err := s.Options.validate(); err != nil {
		return nil, err
	}
	size, m, h, err := fileDigests(s.Options.LocalPath)
	if err != nil {
		return nil, err
	}
	if size <= 0 {
		return nil, errors.Errorf("%s is empty", s.Options.LocalPath)
	}
	s.size, s.md5, s.sha256 = size, m, h
	sum := "md5 " + m
	if s.Options.SkipChecksum {
		sum = "no checksum sent"
	}
	detail := fmt.Sprintf("%s (%d bytes, sha256 %s, %s): serve GET /v1/images/<random uuid> on %s (advertised http://%s, x-auth-token random per run, redacted) only while this step runs; "+
		"GET nova/image/ifExists; POST nova/download destFile=%s<uuid> (on HTTP 404: PUT cinder/download); GET nova/image/ifExists; "+
		"POST cinder/volume/createFromImage name=%s location=%s",
		s.Options.LocalPath, size, h, sum, s.Options.ServeAddr, s.Options.advertise(), NOVA_IMAGE_DIR, t.FileName, t.PoolName)
	return &SImportStagePlan{Step: "http-upload", Method: "HTTP-PULL", Detail: detail, Warnings: []string{
		fmt.Sprintf("http mode: the CAS node must reach http://%s; the image is served over plain HTTP (Glance V1), protected only by a one-run token", s.Options.advertise()),
	}}, nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// glanceStub serves one file as a Glance V1 image.
type glanceStub struct {
	uuid, token string
	path        string
	size        int64
	md5         string
	diskFormat  string

	lock     sync.Mutex
	served   int // full GETs completed
	rejected int // bad token
	sentSHA  []string
}

func (g *glanceStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/images/"+g.uuid || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	got := r.Header.Get(glanceTokenHeader)
	if subtle.ConstantTimeCompare([]byte(got), []byte(g.token)) != 1 {
		g.lock.Lock()
		g.rejected++
		g.lock.Unlock()
		log.Warningf("cas http upload: rejected %s %s from %s (bad or missing %s)", r.Method, r.URL.Path, r.RemoteAddr, glanceTokenHeader)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f, err := os.Open(g.path)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != g.size {
		// the file changed since Plan: serve nothing
		w.WriteHeader(http.StatusConflict)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Content-Length", strconv.FormatInt(g.size, 10))
	hd.Set("x-image-meta-id", g.uuid)
	hd.Set("x-image-meta-size", strconv.FormatInt(g.size, 10))
	hd.Set("x-image-meta-status", "active")
	hd.Set("x-image-meta-disk_format", g.diskFormat)
	hd.Set("x-image-meta-container_format", "bare")
	if len(g.md5) > 0 {
		hd.Set("x-image-meta-checksum", g.md5)
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), f)
	log.Infof("cas http upload: served %d bytes of image %s to %s (err %v)", n, g.uuid, r.RemoteAddr, err)
	if err == nil && n == g.size {
		g.lock.Lock()
		g.served++
		g.sentSHA = append(g.sentSHA, hex.EncodeToString(h.Sum(nil)))
		g.lock.Unlock()
	}
}

// SNovaDownload is the XML body of POST nova/download (driver shape).
type SNovaDownload struct {
	XMLName   xml.Name `xml:"image"`
	Token     string   `xml:"token"`
	Host      string   `xml:"host"`
	Port      int      `xml:"port"`
	Uuid      string   `xml:"uuid"`
	DestFile  string   `xml:"destFile"`
	ImageType string   `xml:"imageType"`
	Size      int64    `xml:"size"`
	Checksum  string   `xml:"checksum,omitempty"`
}

// SCinderDownload is the JSON body of the deprecated PUT cinder/download
// (api.yaml A:44865); optional srcFile/hpName/location are not sent.
type SCinderDownload struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Token    string `json:"token"`
	Uuid     string `json:"uuid"`
	DestFile string `json:"destFile"`
}

// SCreateFromImage is the RsCinderVolume subset sent to
// cinder/volume/createFromImage.
type SCreateFromImage struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	HpName    string `json:"hpName,omitempty"`
	Location  string `json:"location"`
	Format    string `json:"format"`
	LinkClone bool   `json:"linkClone"`
	WriteZero bool   `json:"writeZero"`
}

var intBodyRe = regexp.MustCompile(`^\s*(?:<[^>]+>\s*)*(-?\d+)\s*(?:<[^>]+>\s*)*$`)

// NovaImageExists is GET nova/image/ifExists. The documented response is a
// bare integer; a JSON number or a single XML element around it is
// accepted too.
func (cli *SCasClient) NovaImageExists(uuid string, size int64, checksum string) (int, error) {
	params := url.Values{}
	params.Set("uuid", uuid)
	params.Set("size", strconv.FormatInt(size, 10))
	if len(checksum) > 0 {
		params.Set("checksum", checksum)
	}
	data, ce, err := cli.rawRequest(httputils.GET, "nova/image/ifExists", params, nil, "")
	if err != nil {
		return 0, err
	}
	if ce != nil {
		return 0, ce.mapped()
	}
	body := strings.TrimSpace(string(data))
	if obj, err := jsonutils.ParseString(body); err == nil {
		switch v := obj.(type) {
		case *jsonutils.JSONInt:
			n, _ := v.Int()
			return int(n), nil
		case *jsonutils.JSONDict:
			// (JSONDict.Int() without a key recurses forever; always pass one)
			for _, k := range []string{"result", "status", "data"} {
				if n, err := v.Int(k); err == nil {
					return int(n), nil
				}
			}
		}
	}
	if m := intBodyRe.FindStringSubmatch(body); m != nil {
		v, _ := strconv.Atoi(m[1])
		return v, nil
	}
	return 0, errors.Errorf("nova/image/ifExists: unexpected body (%d bytes)", len(data))
}

// novaDownload sends POST nova/download and, only when CAS answers HTTP
// 404 (endpoint missing on that build), PUT cinder/download. It returns
// which endpoint was used, whether a download task certainly ran and
// ended (succeeded or failed, so a file it made is no longer written),
// and the error. Rejected requests, 5xx, transport errors and timeouts
// give ended=false.
func (cli *SCasClient) novaDownload(body *SNovaDownload) (string, bool, error) {
	raw, err := xml.Marshal(body)
	if err != nil {
		return "", false, err
	}
	data, ce, err := cli.rawRequest(httputils.POST, "nova/download", nil, raw, "application/xml; charset=UTF-8")
	if err != nil {
		return "nova/download", false, err
	}
	if ce == nil {
		resp, err := parseJSONBody(httputils.POST, "nova/download", data)
		if err != nil {
			return "nova/download", false, err
		}
		_, err = cli.waitAsync(httputils.POST, "nova/download", resp)
		return "nova/download", taskEnded(err), err
	}
	if ce.StatusCode != http.StatusNotFound {
		return "nova/download", false, ce.mapped()
	}
	log.Warningf("cas: POST nova/download returned 404, falling back to the deprecated PUT cinder/download")
	cb := &SCinderDownload{Host: body.Host, Port: body.Port, Token: body.Token, Uuid: body.Uuid, DestFile: body.DestFile}
	resp, err := cli.request(httputils.PUT, "cinder/download", nil, jsonutils.Marshal(cb))
	if err != nil {
		if errors.Cause(err) == cloudprovider.ErrNotFound {
			return "cinder/download", false, errors.Wrapf(cloudprovider.ErrNotSupported, "neither POST nova/download nor PUT cinder/download exists on this CAS (both 404); use pool-file or ssh mode")
		}
		return "cinder/download", false, err
	}
	_, err = cli.waitAsync(httputils.PUT, "cinder/download", resp)
	return "cinder/download", taskEnded(err), err
}

// taskEnded: waitAsync returned success or a finished task's failure.
func taskEnded(err error) bool {
	return err == nil || errors.Cause(err) == ErrTaskFailed
}

// DeleteNovaImage is DELETE nova/image/<uuid> (the drivers' call, not in
// api.yaml), which removes /vms/nova/<uuid>.
func (cli *SCasClient) DeleteNovaImage(uuid string) error {
	_, ce, err := cli.rawRequest(httputils.DELETE, "nova/image/"+url.PathEscape(uuid), nil, nil, "")
	if err != nil {
		return err
	}
	if ce != nil {
		return ce.mapped()
	}
	return nil
}

func redactToken(err error, token string) error {
	if err == nil || len(token) == 0 || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.Error(strings.ReplaceAll(err.Error(), token, redacted))
}

func (cli *SCasClient) hostPoolName(id int64) (string, error) {
	pools, err := cli.GetHostPools()
	if err != nil {
		return "", err
	}
	for i := range pools {
		if pools[i].Id == id {
			return pools[i].Name, nil
		}
	}
	return "", errors.Wrapf(cloudprovider.ErrNotFound, "host pool %d", id)
}

// Stage runs the stub for exactly the duration of this call (every return
// path closes it) and pulls the file into the pool.
func (s *SHTTPStager) Stage(t SImportTarget) ([]string, error) {
	if t.cli == nil {
		return nil, errors.Errorf("http stager: no CAS client")
	}
	if len(s.sha256) == 0 {
		return nil, errors.Errorf("http stager: Stage called before Plan")
	}
	host, port, err := s.Options.validate()
	if err != nil {
		return nil, err
	}
	if err := t.cli.checkWritable("http upload"); err != nil {
		return nil, err
	}
	hpName, err := t.cli.hostPoolName(t.HostPoolId)
	if err != nil {
		return nil, errors.Wrapf(err, "host pool of host %d", t.HostId)
	}
	uuid, err := newUUID()
	if err != nil {
		return nil, err
	}
	token, err := randHex(32)
	if err != nil {
		return nil, err
	}
	checksum := s.md5
	if s.Options.SkipChecksum {
		checksum = ""
	}
	stub := &glanceStub{uuid: uuid, token: token, path: s.Options.LocalPath, size: s.size, md5: checksum, diskFormat: s.Options.imageType()}

	listen := s.listen
	if listen == nil {
		listen = net.Listen
	}
	ln, err := listen("tcp", s.Options.ServeAddr)
	if err != nil {
		return nil, errors.Wrapf(err, "listen on %s", s.Options.ServeAddr)
	}
	if port == 0 {
		port = ln.Addr().(*net.TCPAddr).Port
	}
	srv := &http.Server{Handler: stub, ReadHeaderTimeout: 30 * time.Second}
	served := make(chan struct{})
	go func() {
		defer close(served)
		srv.Serve(ln)
	}()
	defer func() {
		srv.Close()
		<-served
		log.Infof("cas http upload: stub on %s stopped", s.Options.ServeAddr)
	}()

	warns, err := s.pull(t, stub, host, port, hpName, checksum)
	return warns, redactToken(err, token)
}

func (s *SHTTPStager) pull(t SImportTarget, stub *glanceStub, host string, port int, hpName, checksum string) ([]string, error) {
	cli := t.cli
	destFile := NOVA_IMAGE_DIR + stub.uuid
	keepMsg := func(why string) string {
		return fmt.Sprintf("staging file %s was kept (%s); remove it on the CAS node by hand when no task uses it", destFile, why)
	}

	st, err := cli.NovaImageExists(stub.uuid, s.size, checksum)
	if err != nil {
		return nil, errors.Wrapf(err, "nova/image/ifExists before download")
	}
	if st != NOVA_IMAGE_ABSENT {
		// a fresh random uuid cannot exist; do not touch whatever it is
		return nil, errors.Errorf("nova/image/ifExists says %s exists already (status %d) before any download; refusing to continue", destFile, st)
	}

	// Deleting /vms/nova/<uuid> is safe only when (1) this run created it:
	// the uuid is random and ifExists said absent just above; (2) the
	// download task is known to have ended, so nothing writes it any more;
	// (3) createFromImage was never sent, so no pool copy can read it.
	// Otherwise it is kept and reported.
	cleanup := func(cause error, ended bool) ([]string, error) {
		if !ended {
			if isDefiniteRejection(cause) {
				return nil, cause // no task, no file
			}
			return []string{keepMsg("the download may still be running or its outcome is unknown")}, cause
		}
		if err := cli.DeleteNovaImage(stub.uuid); err != nil && errors.Cause(err) != cloudprovider.ErrNotFound {
			return []string{keepMsg(fmt.Sprintf("DELETE nova/image/%s failed: %v", stub.uuid, err))}, cause
		}
		return nil, cause
	}

	body := &SNovaDownload{Token: stub.token, Host: host, Port: port, Uuid: stub.uuid, DestFile: destFile,
		ImageType: s.Options.imageType(), Size: s.size, Checksum: checksum}
	endpoint, accepted, err := cli.novaDownload(body)
	if err != nil {
		return cleanup(errors.Wrapf(err, "%s", endpoint), accepted)
	}
	stub.lock.Lock()
	served, rejected, sent := stub.served, stub.rejected, append([]string{}, stub.sentSHA...)
	stub.lock.Unlock()
	if served == 0 {
		return cleanup(errors.Errorf("%s reported success but CAS never fetched the image from http://%s:%d (%d rejected requests)", endpoint, host, port, rejected), true)
	}
	for _, h := range sent {
		if h != s.sha256 {
			return cleanup(errors.Errorf("local file %s changed while it was served (sha256 %s, planned %s)", s.Options.LocalPath, h, s.sha256), true)
		}
	}
	st, err = cli.NovaImageExists(stub.uuid, s.size, checksum)
	if err != nil {
		return cleanup(errors.Wrapf(err, "nova/image/ifExists after download"), true)
	}
	if st != NOVA_IMAGE_PRESENT {
		return cleanup(errors.Errorf("after %s, nova/image/ifExists for %s returned %d, want %d (present with size %d)", endpoint, destFile, st, NOVA_IMAGE_PRESENT, s.size), true)
	}

	cfi := &SCreateFromImage{Name: t.FileName, Image: stub.uuid, HpName: hpName, Location: t.PoolName, Format: s.Options.imageType()}
	if _, err := cli.doAsync(httputils.POST, "cinder/volume/createFromImage", nil, jsonutils.Marshal(cfi)); err != nil {
		return []string{keepMsg("createFromImage was sent; the pool copy may still read it")}, errors.Wrapf(err, "cinder/volume/createFromImage %s into %s", t.FileName, t.PoolName)
	}
	return []string{keepMsg("it is the source of createFromImage; once " + t.Path() + " is confirmed it is no longer needed")}, nil
}
