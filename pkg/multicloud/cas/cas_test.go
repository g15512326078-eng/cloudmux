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
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	testUser  = "admin"
	testPass  = "s3cret"
	testRealm = "cas-test"
	testNonce = "dcd98b7102dd2f0e8b11d0f600bfb0c093"
)

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func parseDigestHeader(h string) map[string]string {
	ret := map[string]string{}
	h = strings.TrimPrefix(h, "Digest ")
	for _, part := range strings.Split(h, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			ret[kv[0]] = strings.Trim(kv[1], `"`)
		}
	}
	return ret
}

// checkDigest validates an RFC 2617 MD5 / qop=auth response.
func checkDigest(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := parseDigestHeader(h)
	if p["username"] != testUser || p["realm"] != testRealm || p["nonce"] != testNonce {
		return false
	}
	ha1 := md5hex(fmt.Sprintf("%s:%s:%s", testUser, testRealm, testPass))
	ha2 := md5hex(fmt.Sprintf("%s:%s", r.Method, p["uri"]))
	expect := md5hex(strings.Join([]string{ha1, p["nonce"], p["nc"], p["cnonce"], p["qop"], ha2}, ":"))
	return p["response"] == expect && p["uri"] == r.URL.RequestURI()
}

type fakeCas struct {
	t      *testing.T
	routes map[string]string // path (no query) -> body
	seen   []string          // request URIs that passed auth
}

func (f *fakeCas) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !checkDigest(r) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", algorithm=MD5`, testRealm, testNonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if accept := r.Header.Get("Accept"); accept != "application/json" {
		f.t.Errorf("Accept = %q", accept)
	}
	f.seen = append(f.seen, r.URL.RequestURI())
	body, ok := f.routes[r.URL.Path]
	if !ok {
		w.Header().Set("Error-Code", "404")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(body))
}

func fixture(t *testing.T, name string) string {
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(data)
}

func newFakeCas(t *testing.T) (*fakeCas, *httptest.Server) {
	f := &fakeCas{t: t, routes: map[string]string{
		"/cas/casrs/hostpool/all":      fixture(t, "hostpool_all"),
		"/cas/casrs/host":              fixture(t, "host"),
		"/cas/casrs/host/id/1":         fixture(t, "host_detail"),
		"/cas/casrs/storage/pool":      fixture(t, "storage_pool"),
		"/cas/casrs/host/id/1/storage": fixture(t, "host_shared_storage"),
		"/cas/casrs/host/id/1/vswitch": fixture(t, "host_vswitch"),
		"/cas/casrs/vm/vmList":         fixture(t, "vm_list"),
		"/cas/casrs/vm/detail/9":       fixture(t, "vm_detail"),
		// The api.yaml examples for vmList and vm/detail describe different
		// VMs, so the bulk detail is empty here and the mapping test goes
		// through the per-VM vm/detail/{id} fallback. The bulk path is
		// tested with the consistent fake in sync_test.go.
		"/cas/casrs/vm/detail/all":  `{"domain":[]}`,
		"/cas/casrs/vmTemplate/all": fixture(t, "vmtemplate_all"),
		"/cas/casrs/version":        `{"casVersion":"test-version"}`,
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func newTestClient(t *testing.T, url, password string) (*SCasClient, error) {
	cfg := NewCasClientConfig(url+"/", testUser, password).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})
	return NewCasClient(cfg)
}

func TestDigestAuth(t *testing.T) {
	f, srv := newFakeCas(t)
	cli, err := newTestClient(t, srv.URL, testPass)
	if err != nil {
		t.Fatalf("NewCasClient: %v", err)
	}
	if len(f.seen) != 1 || f.seen[0] != "/cas/casrs/host" {
		t.Errorf("probe requests = %v, want [/cas/casrs/host]", f.seen)
	}
	version, err := cli.GetVersion()
	if err != nil || version != "test-version" {
		t.Errorf("GetVersion = %q, %v", version, err)
	}

	_, err = newTestClient(t, srv.URL, "wrong")
	if errors.Cause(err) != cloudprovider.ErrInvalidAccessKey {
		t.Errorf("wrong password: err = %v, want ErrInvalidAccessKey", err)
	}
}

func TestRequestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cas/casrs/conflict":
			w.Header().Set("Error-Code", "1234")
			w.Header().Set("Error-Message", "busy")
			w.WriteHeader(http.StatusConflict)
		case "/cas/casrs/xml":
			w.Write([]byte(`<list><host><id>1</id></host></list>`))
		case "/cas/casrs/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/cas/casrs/conflict-notfound":
			w.Header().Set("Error-Code", "5001")
			w.Header().Set("Error-Message", "虚拟机不存在。")
			w.WriteHeader(http.StatusConflict)
		case "/cas/casrs/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/cas/casrs/empty":
			w.WriteHeader(http.StatusOK)
		case "/cas/casrs/vm/detail/1":
			w.Write([]byte(`{}`))
		case "/cas/casrs/host/id/1":
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}

	err := cli.get("conflict", nil, nil)
	ce, ok := err.(*SCasError)
	if !ok || ce.StatusCode != 409 || ce.ErrorCode != "1234" || ce.ErrorMessage != "busy" {
		t.Errorf("409: err = %#v", err)
	}
	err = cli.get("xml", nil, nil)
	if errors.Cause(err) != cloudprovider.ErrNotSupported {
		t.Errorf("xml: err = %v, want ErrNotSupported", err)
	}
	err = cli.get("missing", nil, nil)
	if errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("404: err = %v, want ErrNotFound", err)
	}
	err = cli.get("conflict-notfound", nil, nil)
	if errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("409 not-exist: err = %v, want ErrNotFound", err)
	}
	err = cli.get("forbidden", nil, nil)
	if errors.Cause(err) != cloudprovider.ErrNoPermission {
		t.Errorf("403: err = %v, want ErrNoPermission", err)
	}

	// an empty 200 must be an error, never a zero-value object
	err = cli.get("empty", nil, nil)
	if errors.Cause(err) != ErrEmptyBody {
		t.Errorf("empty 200: err = %v, want ErrEmptyBody", err)
	}
	if detail, err := cli.GetInstanceDetail(1); errors.Cause(err) != ErrEmptyBody {
		t.Errorf("{} vm detail: %+v, %v, want ErrEmptyBody", detail, err)
	}
	if host, err := cli.GetHost("1"); errors.Cause(err) != ErrEmptyBody {
		t.Errorf("{} host detail: %+v, %v, want ErrEmptyBody", host, err)
	}
}

func TestReadOnlyBlocksWrites(t *testing.T) {
	_, srv := newFakeCas(t)
	cfg := NewCasClientConfig(srv.URL, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{ReadOnly: true})
	cli, err := NewCasClient(cfg)
	if err != nil {
		t.Fatalf("NewCasClient: %v", err)
	}
	_, err = cli.request("PUT", "vm/start/9", nil, jsonutils.NewDict())
	if errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
		t.Errorf("PUT on read-only account: err = %v", err)
	}
}

func TestUnmarshalListShapes(t *testing.T) {
	cases := map[string]int{
		`{"hostPool":[{"id":1,"name":"a"},{"id":2,"name":"b"}]}`: 2,
		`{"hostPool":{"id":1,"name":"a"}}`:                       1, // single element as bare object
		`{"list":{"hostPool":[{"id":1,"name":"a"}]}}`:            1, // root element kept
		`[{"id":1,"name":"a"}]`:                                  1, // bare array
		`{}`:                                                     0,
		`{"hostPool":""}`:                                        0,
		`{"hostPool":null}`:                                      0,
	}
	for body, want := range cases {
		obj, err := jsonutils.ParseString(body)
		if err != nil {
			t.Fatalf("parse %s: %v", body, err)
		}
		pools := []SHostPool{}
		err = unmarshalList(obj, "hostPool", &pools)
		if err != nil {
			t.Errorf("%s: %v", body, err)
			continue
		}
		if len(pools) != want {
			t.Errorf("%s: got %d pools, want %d", body, len(pools), want)
		}
		if want > 0 && (pools[0].Id != 1 || pools[0].Name != "a") {
			t.Errorf("%s: pools[0] = %+v", body, pools[0])
		}
	}
}

func TestSyncMapping(t *testing.T) {
	_, srv := newFakeCas(t)
	cli, err := newTestClient(t, srv.URL, testPass)
	if err != nil {
		t.Fatalf("NewCasClient: %v", err)
	}
	regions, err := cli.GetIRegions()
	if err != nil || len(regions) != 1 {
		t.Fatalf("GetIRegions = %v, %v", regions, err)
	}
	region := regions[0]
	if region.GetGlobalId() != "CAS/acc1" || region.GetProvider() != api.CLOUD_PROVIDER_CAS {
		t.Errorf("region = %s %s", region.GetGlobalId(), region.GetProvider())
	}

	// hostpool -> zone
	zones, err := region.GetIZones()
	if err != nil || len(zones) != 1 {
		t.Fatalf("GetIZones = %v, %v", zones, err)
	}
	if zones[0].GetGlobalId() != "1" || zones[0].GetName() != "hostpool1" {
		t.Errorf("zone = %s %s", zones[0].GetGlobalId(), zones[0].GetName())
	}

	// host
	hosts, err := zones[0].GetIHosts()
	if err != nil || len(hosts) != 1 {
		t.Fatalf("GetIHosts = %v, %v", hosts, err)
	}
	host := hosts[0]
	checks := map[string][2]interface{}{
		"id":       {host.GetGlobalId(), "1"},
		"name":     {host.GetName(), "cvknode186"},
		"type":     {host.GetHostType(), api.HOST_TYPE_CAS},
		"ip":       {host.GetAccessIp(), "10.125.35.186"},
		"cpu":      {host.GetCpuCount(), 40},
		"sockets":  {host.GetNodeCount(), int8(2)},
		"mhz":      {host.GetCpuMhz(), 2772},
		"mem":      {host.GetMemSizeMB(), 128554},
		"disk":     {host.GetStorageSizeMB(), int64(994485)},
		"status":   {host.GetHostStatus(), api.HOST_ONLINE},
		"sn":       {host.GetSN(), "210200A00DN177000200"},
		"maintain": {host.GetIsMaintenance(), false},
	}
	for k, v := range checks {
		if v[0] != v[1] {
			t.Errorf("host %s = %v, want %v", k, v[0], v[1])
		}
	}

	// storage: fixture "isopool" (dir) is not in the host's shared list
	// ("share2"), so it is a local pool keyed by host.
	storages, err := host.GetIStorages()
	if err != nil || len(storages) != 1 {
		t.Fatalf("host GetIStorages = %v, %v", storages, err)
	}
	st := storages[0]
	if st.GetGlobalId() != "host/1/isopool" || st.GetStorageType() != api.STORAGE_LOCAL ||
		st.GetCapacityMB() != 74456 || st.GetCapacityUsedMB() != 74456-69985 || st.GetStatus() != api.STORAGE_ONLINE {
		t.Errorf("storage = %s %s %d %d %s", st.GetGlobalId(), st.GetStorageType(), st.GetCapacityMB(), st.GetCapacityUsedMB(), st.GetStatus())
	}
	zoneStorages, err := zones[0].GetIStorages()
	if err != nil || len(zoneStorages) != 1 {
		t.Errorf("zone GetIStorages = %v, %v", zoneStorages, err)
	}

	// vswitch -> wire, host nics
	vpcs, err := region.GetIVpcs()
	if err != nil || len(vpcs) != 1 {
		t.Fatalf("GetIVpcs = %v, %v", vpcs, err)
	}
	wires, err := vpcs[0].GetIWires()
	if err != nil || len(wires) != 1 {
		t.Fatalf("GetIWires = %v, %v", wires, err)
	}
	if vpcs[0].GetGlobalId() != "vswitch/test" || vpcs[0].GetCidrBlock() != "0.0.0.0/0" {
		t.Errorf("vpc = %s %s", vpcs[0].GetGlobalId(), vpcs[0].GetCidrBlock())
	}
	if wires[0].GetGlobalId() != "vswitch/test/hostpool/1" || wires[0].GetIZone().GetGlobalId() != "1" ||
		wires[0].GetIVpc().GetGlobalId() != "vswitch/test" {
		t.Errorf("wire = %s", wires[0].GetGlobalId())
	}
	networks, err := wires[0].GetINetworks()
	if err != nil || len(networks) != 1 {
		t.Fatalf("GetINetworks = %v, %v", networks, err)
	}
	n := networks[0]
	if n.GetGlobalId() != "vswitch/test/hostpool/1/default" || !n.IsEmulated() || n.GetIpStart() != "0.0.0.1" ||
		n.GetIpEnd() != "255.255.255.254" || n.GetIpMask() != 0 || n.GetServerType() != api.NETWORK_TYPE_GUEST ||
		n.GetIWire().GetGlobalId() != wires[0].GetGlobalId() {
		t.Errorf("network = %s %v %s-%s/%d %s", n.GetGlobalId(), n.IsEmulated(), n.GetIpStart(), n.GetIpEnd(), n.GetIpMask(), n.GetServerType())
	}
	if _, err := wires[0].GetINetworkById("vswitch/test/hostpool/1/default"); err != nil {
		t.Errorf("GetINetworkById: %v", err)
	}
	nics, err := host.GetIHostNics()
	if err != nil || len(nics) != 1 || nics[0].GetIWire().GetGlobalId() != wires[0].GetGlobalId() {
		t.Errorf("GetIHostNics = %v, %v", nics, err)
	}

	// vm
	vms, err := host.GetIVMs()
	if err != nil || len(vms) != 1 {
		t.Fatalf("GetIVMs = %v, %v", vms, err)
	}
	vm := vms[0]
	vmChecks := map[string][2]interface{}{
		"id":         {vm.GetGlobalId(), "3a07b70f-085a-4558-ab22-fd1db6bcb4f3"},
		"name":       {vm.GetName(), "win7-ipv6"},
		"status":     {vm.GetStatus(), api.VM_RUNNING},
		"hypervisor": {vm.GetHypervisor(), api.HYPERVISOR_CAS},
		"host":       {vm.GetIHostId(), "1"},
		"cpu":        {vm.GetVcpuCount(), 2},
		"mem":        {vm.GetVmemSizeMB(), 4096},
		"os":         {vm.GetOsType(), cloudprovider.OsTypeWindows},
		"osname":     {vm.GetFullOsName(), "Windows 7 Ultimate, 64-bit"},
	}
	for k, v := range vmChecks {
		if v[0] != v[1] {
			t.Errorf("vm %s = %v, want %v", k, v[0], v[1])
		}
	}
	if vm.GetCreatedAt().IsZero() {
		t.Errorf("vm createdAt not parsed")
	}
	// lookup by uuid is covered by TestGetInstanceByUuid (sync_test.go)

	disks, err := vm.GetIDisks()
	if err != nil || len(disks) != 1 {
		t.Fatalf("GetIDisks = %v, %v", disks, err)
	}
	disk := disks[0]
	if disk.GetGlobalId() != "3a07b70f-085a-4558-ab22-fd1db6bcb4f3/vda" || disk.GetDiskSizeMB() != 81920 ||
		disk.GetDiskType() != api.DISK_TYPE_SYS || disk.GetDriver() != "ide" || disk.GetDiskFormat() != "qcow2" ||
		disk.GetCacheMode() != "directsync" {
		t.Errorf("disk = %s %d %s %s %s", disk.GetGlobalId(), disk.GetDiskSizeMB(), disk.GetDiskType(), disk.GetDriver(), disk.GetDiskFormat())
	}
	// api.yaml examples are inconsistent: the disk lives in /vms/images,
	// the only pool example is /vms/isos.
	if _, err := disk.GetIStorage(); errors.Cause(err) != cloudprovider.ErrNotFound {
		t.Errorf("disk GetIStorage = %v, want ErrNotFound", err)
	}

	vnics, err := vm.GetINics()
	if err != nil || len(vnics) != 1 {
		t.Fatalf("GetINics = %v, %v", vnics, err)
	}
	if vnics[0].GetMAC() != "0c:da:41:1d:19:d0" || vnics[0].GetIP() != "10.10.10.1" || vnics[0].GetDriver() != "virtio" ||
		// the api.yaml VM example sits on "vswitch0", the host example only
		// has "test"; the consistent case is in TestNetworkTopology.
		vnics[0].GetINetworkId() != "vswitch/vswitch0/hostpool/1/default" {
		t.Errorf("nic = %s %s %s %s", vnics[0].GetMAC(), vnics[0].GetIP(), vnics[0].GetDriver(), vnics[0].GetINetworkId())
	}

	// writes are stubbed
	if err := vm.(*SInstance).StartVM(nil); errors.Cause(err) != cloudprovider.ErrNotImplemented {
		t.Errorf("StartVM = %v", err)
	}

	// templates -> images
	caches, err := region.GetIStoragecaches()
	if err != nil || len(caches) != 1 {
		t.Fatalf("GetIStoragecaches = %v, %v", caches, err)
	}
	images, err := caches[0].GetICloudImages()
	if err != nil || len(images) != 1 {
		t.Fatalf("GetICloudImages = %v, %v", images, err)
	}
	img := images[0]
	if img.GetGlobalId() != "CAS/acc1/template/1" || img.GetName() != "sdfsdf" || img.GetSizeByte() != 1024*1024*1024 ||
		img.GetOsType() != cloudprovider.OsTypeWindows || img.GetImageStatus() != cloudprovider.IMAGE_STATUS_ACTIVE {
		t.Errorf("image = %s %s %d %s", img.GetGlobalId(), img.GetName(), img.GetSizeByte(), img.GetOsType())
	}
	if st.GetIStoragecache().GetGlobalId() != caches[0].GetGlobalId() {
		t.Errorf("storage cache mismatch")
	}
}

func TestMatchStorage(t *testing.T) {
	storages := []SStorage{
		{SStoragePool: SStoragePool{Name: "vms", Path: "/vms"}},
		{SStoragePool: SStoragePool{Name: "defaultpool", Path: "/vms/images"}},
		{SStoragePool: SStoragePool{Name: "imagesX", Path: "/vms/imagesX"}},
	}
	if s := matchStorage(storages, "/vms/images/aa_inc_1"); s == nil || s.Name != "defaultpool" {
		t.Errorf("match = %v", s)
	}
	if s := matchStorage(storages, "/data/x"); s != nil {
		t.Errorf("unexpected match %v", s)
	}
}

func TestSharedStorageId(t *testing.T) {
	host := &SHost{Id: 3, HostPoolId: 7, Name: "cvk3"}
	pools := []SStoragePool{{Name: "share2", Type: "fs"}, {Name: "defaultpool", Type: "dir"}}
	shared := []SStoragePool{{Name: "share2"}}
	got := []string{}
	for _, s := range newStorages(host, pools, shared) {
		got = append(got, s.GetGlobalId()+"|"+s.GetStorageType())
	}
	want := "hostpool/7/share2|fs,host/3/defaultpool|local"
	if strings.Join(got, ",") != want {
		t.Errorf("got %v, want %s", got, want)
	}
}

func TestNoSecretsInLogsOrErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cas/casrs/host/id/1":
			w.Write([]byte(`{"id":1,"name":"cvk1","user":"root","pwd":"S3CR3T-HOST","ipmiPw":"S3CR3T-IPMI"}`))
		case "/cas/casrs/vm/detail/4":
			w.Write([]byte(`{"id":4,"uuid":"u4","graphics":[{"type":"vnc","port":5908,"password":"S3CR3T-VNC"}],"storage":[],"network":[]}`))
		case "/cas/casrs/conflict":
			w.Header().Set("Error-Code", "1")
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"host":{"pwd":"S3CR3T-ERR"}}`))
		case "/cas/casrs/xml":
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`<host><pwd>S3CR3T-XML</pwd></host>`))
		}
	}))
	defer srv.Close()

	logger := log.Logger()
	buf := &bytes.Buffer{}
	oldOut, oldLevel := logger.Out, logger.GetLevel()
	logger.SetOutput(buf)
	logger.SetLevel(logrus.DebugLevel)
	defer func() {
		logger.SetOutput(oldOut)
		logger.SetLevel(oldLevel)
	}()

	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass).Debug(true)}
	if _, err := cli.GetHost("1"); err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if _, err := cli.GetInstanceDetail(4); err != nil {
		t.Fatalf("GetInstanceDetail: %v", err)
	}
	errText := ""
	for _, res := range []string{"conflict", "xml"} {
		if err := cli.get(res, nil, nil); err == nil {
			t.Fatalf("%s: expected error", res)
		} else {
			errText += err.Error()
		}
	}
	out := buf.String() + errText
	if !strings.Contains(buf.String(), "host/id/1") || !strings.Contains(out, redacted) {
		t.Fatalf("debug log missing expected lines: %s", out)
	}
	for _, secret := range []string{"S3CR3T-HOST", "S3CR3T-IPMI", "S3CR3T-VNC", "S3CR3T-ERR", "S3CR3T-XML"} {
		if strings.Contains(out, secret) {
			t.Errorf("secret %s leaked: %s", secret, out)
		}
	}
}

func TestImageGlobalIdIsAccountScoped(t *testing.T) {
	ids := map[string]bool{}
	for _, acc := range []string{"acc1", "acc2"} {
		cli := &SCasClient{CasClientConfig: NewCasClientConfig("http://x:1", testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: acc})}
		img := &SImage{SVmTemplate: SVmTemplate{Id: 1}, cache: cli.GetRegion().getStoragecache()}
		want := fmt.Sprintf("CAS/%s/template/1", acc)
		if img.GetGlobalId() != want {
			t.Errorf("image id = %s, want %s", img.GetGlobalId(), want)
		}
		ids[img.GetGlobalId()] = true
	}
	if len(ids) != 2 {
		t.Errorf("template 1 on two accounts collides: %v", ids)
	}
}
