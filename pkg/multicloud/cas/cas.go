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
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/icholy/digest"
	"golang.org/x/text/encoding/simplifiedchinese"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	api "yunion.io/x/cloudmux/pkg/apis/compute"
	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	CLOUD_PROVIDER_CAS = api.CLOUD_PROVIDER_CAS

	// API_PREFIX is the CAS REST root, e.g. https://cas.example.com:8443/cas/casrs/host
	API_PREFIX = "/cas/casrs"
)

type CasClientConfig struct {
	cpcfg cloudprovider.ProviderConfig

	// accessUrl is scheme://host:port with no trailing path.
	accessUrl string
	username  string
	password  string

	// pageSize is the limit used by listPaged (DEFAULT_PAGE_SIZE if 0).
	pageSize int

	// snapshotTimeZone is the IANA name used to interpret zone-less
	// snapshot creationTime strings (api.yaml examples have no offset).
	// Empty means DEFAULT_SNAPSHOT_TIMEZONE. CAS prints CVM local wall
	// time; CN deployments commonly use Asia/Shanghai.
	snapshotTimeZone string

	// deployTimeout is the vm/deploy task wait (DEFAULT_DEPLOY_TIMEOUT
	// if 0).
	deployTimeout time.Duration

	debug bool
}

// DEFAULT_SNAPSHOT_TIMEZONE is the zone assumed for zone-less snapshot
// creationTime values. Override per client with SnapshotTimeZone.
const DEFAULT_SNAPSHOT_TIMEZONE = "Asia/Shanghai"

func NewCasClientConfig(accessUrl, username, password string) *CasClientConfig {
	return &CasClientConfig{
		accessUrl: strings.TrimSuffix(accessUrl, "/"),
		username:  username,
		password:  password,
	}
}

func (cfg *CasClientConfig) CloudproviderConfig(cpcfg cloudprovider.ProviderConfig) *CasClientConfig {
	cfg.cpcfg = cpcfg
	return cfg
}

func (cfg *CasClientConfig) Debug(debug bool) *CasClientConfig {
	cfg.debug = debug
	return cfg
}

// DeployTimeout sets how long CreateVM waits for the vm/deploy task.
func (cfg *CasClientConfig) DeployTimeout(d time.Duration) *CasClientConfig {
	cfg.deployTimeout = d
	return cfg
}

func (cfg *CasClientConfig) SnapshotTimeZone(tz string) *CasClientConfig {
	cfg.snapshotTimeZone = tz
	return cfg
}

// ReadOnlyClone returns a client that refuses non-GET/HEAD requests. It
// builds its own HTTP client (the read-only guard is per client).
func (cli *SCasClient) ReadOnlyClone() *SCasClient {
	if cli == nil {
		return nil
	}
	cfg := *cli.CasClientConfig
	cfg.cpcfg.ReadOnly = true
	return &SCasClient{CasClientConfig: &cfg, ctx: cli.ctx, timeouts: cli.timeouts, taskPollInterval: cli.taskPollInterval, taskTimeout: cli.taskTimeout}
}

// S-2: every CAS request is bounded. The vendored adaptive client dials
// for up to 120s and ignores the context, so an unreachable CAS took
// about 135s to fail. These bounds replace it; waitTask keeps its own
// overall deadline (DEFAULT_TASK_TIMEOUT) on top of them.
const (
	DEFAULT_DIAL_TIMEOUT            = 10 * time.Second
	DEFAULT_TLS_HANDSHAKE_TIMEOUT   = 10 * time.Second
	DEFAULT_RESPONSE_HEADER_TIMEOUT = 30 * time.Second // as the vendored transport
	DEFAULT_REQUEST_TIMEOUT         = 5 * time.Minute  // whole request incl. body
	// DEFAULT_PROBE_TIMEOUT bounds Probe (account create / health check).
	DEFAULT_PROBE_TIMEOUT = 30 * time.Second
)

// sClientTimeouts overrides the defaults when a field is > 0 (tests).
type sClientTimeouts struct {
	Dial, TLSHandshake, ResponseHeader, Request time.Duration
}

func (t sClientTimeouts) withDefaults() sClientTimeouts {
	if t.Dial <= 0 {
		t.Dial = DEFAULT_DIAL_TIMEOUT
	}
	if t.TLSHandshake <= 0 {
		t.TLSHandshake = DEFAULT_TLS_HANDSHAKE_TIMEOUT
	}
	if t.ResponseHeader <= 0 {
		t.ResponseHeader = DEFAULT_RESPONSE_HEADER_TIMEOUT
	}
	if t.Request <= 0 {
		t.Request = DEFAULT_REQUEST_TIMEOUT
	}
	return t
}

// WithContext returns a client whose requests and waits are bound to
// ctx (cancel aborts an in-flight request; a task CAS already accepted
// may still run). It shares the HTTP client and its read-only guard.
func (cli *SCasClient) WithContext(ctx context.Context) *SCasClient {
	c := cli.derive()
	c.ctx = ctx
	return c
}

// withRequestTimeout returns a client whose requests time out after d.
func (cli *SCasClient) withRequestTimeout(d time.Duration) *SCasClient {
	c := cli.derive()
	c.timeouts.Request = d
	return c
}

func (cli *SCasClient) derive() *SCasClient {
	return &SCasClient{CasClientConfig: cli.CasClientConfig, client: cli.getDefaultClient(), ctx: cli.ctx, timeouts: cli.timeouts,
		taskPollInterval: cli.taskPollInterval, taskTimeout: cli.taskTimeout}
}

func (cli *SCasClient) context() context.Context {
	if cli.ctx == nil {
		return context.Background()
	}
	return cli.ctx
}

// ctxErr reports a cancelled or expired client context (waits stop).
func (cli *SCasClient) ctxErr() error {
	if cli.ctx == nil {
		return nil
	}
	return cli.ctx.Err()
}

// snapshotLocation returns the zone for zone-less snapshot timestamps.
func (cli *SCasClient) snapshotLocation() *time.Location {
	tz := DEFAULT_SNAPSHOT_TIMEZONE
	if cli != nil && cli.CasClientConfig != nil && len(cli.snapshotTimeZone) > 0 {
		tz = cli.snapshotTimeZone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc, _ = time.LoadLocation(DEFAULT_SNAPSHOT_TIMEZONE)
	}
	if loc == nil {
		return time.UTC
	}
	return loc
}

type SCasClient struct {
	*CasClientConfig

	lock   sync.Mutex
	client *http.Client

	// ctx bounds requests and waits (nil = Background); see WithContext.
	ctx      context.Context
	timeouts sClientTimeouts

	// taskPollInterval / taskTimeout override waitTask defaults when > 0
	// (tests set them; production leaves them zero).
	taskPollInterval time.Duration
	taskTimeout      time.Duration
}

// NewCasClient builds a client and probes GET /cas/casrs/host so wrong
// Digest credentials fail at account create / GetProvider (cloudpods health).
// Offline cmx --fixture runs must not call NewCasClient; see cmx Main.
func NewCasClient(cfg *CasClientConfig) (*SCasClient, error) {
	if cfg == nil {
		return nil, errors.Wrap(cloudprovider.ErrInputParameter, "nil CasClientConfig")
	}
	cli := &SCasClient{CasClientConfig: cfg}
	if err := cli.Probe(); err != nil {
		return nil, err
	}
	return cli, nil
}

// probeTimeout is DEFAULT_PROBE_TIMEOUT; a var so tests can shorten it.
var probeTimeout = DEFAULT_PROBE_TIMEOUT

// Probe checks Digest credentials with GET /cas/casrs/host, bounded by
// probeTimeout so an unreachable CAS fails fast (S-2).
func (cli *SCasClient) Probe() error {
	_, err := cli.withRequestTimeout(probeTimeout).GetHostBriefs()
	if err != nil {
		return errors.Wrapf(err, "probe %s", cli.accessUrl)
	}
	return nil
}

func (cli *SCasClient) getDefaultClient() *http.Client {
	cli.lock.Lock()
	defer cli.lock.Unlock()
	if cli.client != nil {
		return cli.client
	}
	client := httputils.GetAdaptiveTimeoutClient()
	httputils.SetClientProxyFunc(client, cli.cpcfg.ProxyFunc)
	ts, _ := client.Transport.(*http.Transport)
	// S-2: a bounded, context-aware dial instead of the adaptive one
	t := cli.timeouts.withDefaults()
	ts.DialContext = (&net.Dialer{Timeout: t.Dial, KeepAlive: 30 * time.Second}).DialContext
	ts.TLSHandshakeTimeout = t.TLSHandshake
	ts.ResponseHeaderTimeout = t.ResponseHeader
	// CAS ships with a self-signed certificate by default; same policy as
	// the proxmox / oceanbase providers.
	ts.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	client.Transport = &digest.Transport{
		Username: cli.username,
		Password: cli.password,
		Transport: cloudprovider.GetCheckTransport(ts, func(req *http.Request) (func(resp *http.Response) error, error) {
			if cli.cpcfg.ReadOnly && req.Method != "GET" && req.Method != "HEAD" {
				return nil, errors.Wrapf(cloudprovider.ErrAccountReadOnly, "%s %s", req.Method, req.URL.Path)
			}
			return nil, nil
		}),
	}
	cli.client = client
	return client
}

// SCasError carries the business error headers CAS documents for HTTP 409
// (Error-Code / Error-Message) plus the body with secrets redacted.
type SCasError struct {
	Method       string
	Url          string
	StatusCode   int
	ErrorCode    string
	ErrorMessage string
	Body         string
}

func (e *SCasError) Error() string {
	return jsonutils.Marshal(e).String()
}

// ErrEmptyBody is returned for a 2xx response without a body.
const ErrEmptyBody = errors.Error("EmptyResponseBody")

// notFoundErrorCodes are 409 Error-Code values that mean the addressed
// object itself is gone. Only those codes are listed: others that also
// contain "does not exist" (for example a missing backing file of a VM
// that is still present) must not become ErrNotFound, or cloudpods would
// drop a live object.
var notFoundErrorCodes = map[string]bool{
	"402":     true, // host missing
	"1000":    true, // template missing
	"1001":    true, // VM missing
	"1013":    true, // named template missing
	"1101":    true, // cluster missing
	"11007":   true, // storage pool missing
	"91200":   true, // named storage pool missing
	"91606":   true, // named storage volume missing
	"1900009": true, // cluster id missing
	"1900015": true, // host id missing
}

// isNotFound reports whether a 409 business error means the target does
// not exist. When CAS sends an Error-Code, only notFoundErrorCodes count.
// Without a code, fall back to an Error-Message containing "不存在"
// ("does not exist"), the wording api.yaml uses in its examples.
func (e *SCasError) isNotFound() bool {
	if code := strings.TrimSpace(e.ErrorCode); len(code) > 0 {
		return notFoundErrorCodes[code]
	}
	return strings.Contains(e.ErrorMessage, "不存在")
}

// decodeErrorMessage turns the raw Error-Message header into UTF-8. CAS
// writes it in a legacy Chinese charset carried as ISO-8859-1 header
// bytes. Observed CAS clients decode via the response "charset" header
// or GB2312. Go keeps header bytes as-is, so decode here: the charset
// header first, then GB18030 (a superset of GB2312/GBK) when the bytes
// are not valid UTF-8.
func decodeErrorMessage(raw, charset string) string {
	if len(raw) == 0 {
		return raw
	}
	cs := strings.ToLower(strings.TrimSpace(charset))
	switch cs {
	case "utf-8", "utf8":
		return raw
	case "gb2312", "gbk", "gb18030", "":
		if cs == "" && utf8.ValidString(raw) {
			return raw
		}
		out, err := simplifiedchinese.GB18030.NewDecoder().String(raw)
		if err == nil {
			return out
		}
	}
	if utf8.ValidString(raw) {
		return raw
	}
	return strings.ToValidUTF8(raw, "?")
}

func (cli *SCasClient) url(res string, params url.Values) string {
	u := fmt.Sprintf("%s%s/%s", cli.accessUrl, API_PREFIX, strings.TrimPrefix(res, "/"))
	if len(params) > 0 {
		u = fmt.Sprintf("%s?%s", u, params.Encode())
	}
	return u
}

func (cli *SCasClient) request(method httputils.THttpMethod, res string, params url.Values, body jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	var raw []byte
	if body != nil {
		raw = []byte(body.String())
	}
	data, ce, err := cli.rawRequest(method, res, params, raw, "application/json; charset=UTF-8")
	if err != nil {
		return nil, err
	}
	if ce != nil {
		return nil, ce.mapped()
	}
	return parseJSONBody(method, cli.url(res, params), data)
}

func parseJSONBody(method httputils.THttpMethod, u string, data []byte) (jsonutils.JSONObject, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		// Never turn an empty 200 into {}: a zero-value VM detail would make
		// cloudpods detach every disk and NIC.
		return nil, errors.Wrapf(ErrEmptyBody, "%s %s", method, u)
	}
	if data[0] == '<' {
		// We ask for JSON; an XML answer means this CAS build ignores Accept.
		return nil, errors.Wrapf(cloudprovider.ErrNotSupported, "%s %s returned XML, only JSON is supported", method, u)
	}
	obj, err := jsonutils.Parse(data)
	if err != nil {
		return nil, errors.Wrapf(err, "parse json %s %s", method, u)
	}
	return obj, nil
}

// mapped turns an HTTP error status into the cloudprovider error causes
// callers test for (404/401/403, and 409 with a not-found code).
func (ce *SCasError) mapped() error {
	switch ce.StatusCode {
	case http.StatusNotFound:
		return errors.Wrap(cloudprovider.ErrNotFound, ce.Error())
	case http.StatusUnauthorized:
		return errors.Wrap(cloudprovider.ErrInvalidAccessKey, ce.Error())
	case http.StatusForbidden:
		return errors.Wrap(cloudprovider.ErrNoPermission, ce.Error())
	case http.StatusConflict:
		if ce.isNotFound() {
			return errors.Wrap(cloudprovider.ErrNotFound, ce.Error())
		}
	}
	return ce
}

// rawRequest sends body (with contentType when non-empty) and returns the
// response body of a 2xx, or the unmapped *SCasError for status >= 300, or
// a transport error. Callers that need the exact status (for example to
// tell a missing endpoint, HTTP 404, from a business error) use it
// directly; request() maps the error.
func (cli *SCasClient) rawRequest(method httputils.THttpMethod, res string, params url.Values, body []byte, contentType string) ([]byte, *SCasError, error) {
	u := cli.url(res, params)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	ctx, cancel := context.WithTimeout(cli.context(), cli.timeouts.withDefaults().Request)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, string(method), u, reader)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "NewRequest %s %s", method, u)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := cli.getDefaultClient().Do(req)
	if err != nil {
		// unwrap *url.Error so causes such as ErrAccountReadOnly survive
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err
		}
		return nil, nil, errors.Wrapf(err, "%s %s", method, u)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "read body %s %s", method, u)
	}
	if cli.debug {
		log.Debugf("%s %s -> %d %s", method, u, resp.StatusCode, redactBody(data))
	}
	if resp.StatusCode >= 300 {
		return nil, &SCasError{
			Method:       string(method),
			Url:          u,
			StatusCode:   resp.StatusCode,
			ErrorCode:    resp.Header.Get("Error-Code"),
			ErrorMessage: decodeErrorMessage(resp.Header.Get("Error-Message"), resp.Header.Get("charset")),
			Body:         redactBody(data),
		}, nil
	}
	return data, nil, nil
}

func (cli *SCasClient) get(res string, params url.Values, retVal interface{}) error {
	resp, err := cli.request(httputils.GET, res, params, nil)
	if err != nil {
		return err
	}
	if retVal == nil {
		return nil
	}
	return unwrapRoot(resp).Unmarshal(retVal)
}

// list GETs res and decodes the array found under key into retVal (a
// pointer to a slice).
func (cli *SCasClient) list(res string, params url.Values, key string, retVal interface{}) error {
	resp, err := cli.request(httputils.GET, res, params, nil)
	if err != nil {
		return err
	}
	return unmarshalList(resp, key, retVal)
}

// unwrapRoot strips a {"list": {...}} style root element, in case the
// server keeps the JAXB XML root name (swagger documents "xml: name: list")
// when serializing JSON.
func unwrapRoot(obj jsonutils.JSONObject) jsonutils.JSONObject {
	dict, ok := obj.(*jsonutils.JSONDict)
	if !ok || dict.Length() != 1 {
		return obj
	}
	if v, err := dict.Get("list"); err == nil {
		if _, isDict := v.(*jsonutils.JSONDict); isDict {
			return v
		}
	}
	return obj
}

// listItems returns the elements of obj[key]. It tolerates the shapes
// JAXB-to-JSON bridges commonly emit for a repeated XML element: a JSON
// array, a bare object when there is exactly one element, and a missing
// key / null / "" when there are none.
func listItems(obj jsonutils.JSONObject, key string) ([]jsonutils.JSONObject, error) {
	obj = unwrapRoot(obj)
	if arr, ok := obj.(*jsonutils.JSONArray); ok {
		return arr.GetArray()
	}
	if !obj.Contains(key) {
		return nil, nil
	}
	v, err := obj.Get(key)
	if err != nil {
		return nil, errors.Wrapf(err, "get %s", key)
	}
	switch vv := v.(type) {
	case *jsonutils.JSONArray:
		return vv.GetArray()
	case *jsonutils.JSONDict:
		return []jsonutils.JSONObject{vv}, nil
	}
	if v.IsZero() || v == jsonutils.JSONNull {
		return nil, nil
	}
	return nil, errors.Errorf("unexpected %s value type %T", key, v)
}

// unmarshalList decodes obj[key] (see listItems) into retVal, a pointer to
// a slice.
func unmarshalList(obj jsonutils.JSONObject, key string, retVal interface{}) error {
	items, err := listItems(obj, key)
	if err != nil {
		return err
	}
	return jsonutils.NewArray(items...).Unmarshal(retVal)
}

const (
	DEFAULT_PAGE_SIZE = 100
	// MAX_PAGES bounds listPaged so a misbehaving server cannot loop it
	// forever (100 * 10000 = 1M records).
	MAX_PAGES = 10000
)

// listPaged walks an offset/limit list endpoint (offset is taken as a
// record offset: page N is offset=N*limit) and returns every element,
// de-duplicated by idKey. Stop conditions:
//   - a page shorter than limit: that was the last page, done;
//   - the first page is longer than limit: the server ignores limit and
//     returned everything, done;
//   - a page that is full but adds no new id: the server ignores offset,
//     so completeness cannot be proven -> error (never a short list);
//   - MAX_PAGES reached -> error.
//
// Returning an error instead of a partial list matters: cloudpods marks
// VMs missing from a successful list as lost.
func (cli *SCasClient) listPaged(res string, params url.Values, key, idKey string) ([]jsonutils.JSONObject, error) {
	return cli.listPagedBy(res, params, key, idKey, "limit")
}

// listPagedBy is listPaged with the page-size query name given: most CAS
// lists take offset+limit, GET /storage/volume takes offset+pageSize.
func (cli *SCasClient) listPagedBy(res string, params url.Values, key, idKey, limitParam string) ([]jsonutils.JSONObject, error) {
	limit := cli.pageSize
	if limit <= 0 {
		limit = DEFAULT_PAGE_SIZE
	}
	seen := map[string]bool{}
	ret := []jsonutils.JSONObject{}
	for page := 0; page < MAX_PAGES; page++ {
		query := url.Values{}
		for k, v := range params {
			query[k] = v
		}
		query.Set("offset", fmt.Sprintf("%d", page*limit))
		query.Set(limitParam, fmt.Sprintf("%d", limit))
		resp, err := cli.request(httputils.GET, res, query, nil)
		if err != nil {
			return nil, err
		}
		items, err := listItems(resp, key)
		if err != nil {
			return nil, err
		}
		added := 0
		for i := range items {
			id, _ := items[i].GetString(idKey)
			if len(id) > 0 {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			ret = append(ret, items[i])
			added++
		}
		switch {
		case len(items) < limit:
			return ret, nil
		case len(items) > limit:
			if page == 0 {
				return ret, nil
			}
			return nil, errors.Errorf("%s: page %d returned %d items for limit %d", res, page, len(items), limit)
		case added == 0:
			return nil, errors.Errorf("%s: page %d (offset %d) repeated earlier items, server seems to ignore offset; refusing a possibly truncated list", res, page, page*limit)
		}
	}
	return nil, errors.Errorf("%s: more than %d pages of %d", res, MAX_PAGES, limit)
}

func (cli *SCasClient) GetAccountId() string {
	return cli.accessUrl
}

func (cli *SCasClient) GetSubAccounts() ([]cloudprovider.SSubAccount, error) {
	subAccount := cloudprovider.SSubAccount{
		Id:           cli.GetAccountId(),
		Account:      cli.username,
		Name:         cli.cpcfg.Name,
		HealthStatus: api.CLOUD_PROVIDER_HEALTH_NORMAL,
	}
	return []cloudprovider.SSubAccount{subAccount}, nil
}

func (cli *SCasClient) GetCapabilities() []string {
	// P1: compute is writable (start/stop/delete/deploy). Network stays
	// read-only: CreateINetwork / security groups are not implemented.
	return []string{
		cloudprovider.CLOUD_CAPABILITY_COMPUTE,
		cloudprovider.CLOUD_CAPABILITY_NETWORK + cloudprovider.READ_ONLY_SUFFIX,
	}
}

func (cli *SCasClient) GetCloudRegionExternalIdPrefix() string {
	return fmt.Sprintf("%s/%s", CLOUD_PROVIDER_CAS, cli.cpcfg.Id)
}

func (cli *SCasClient) GetRegion() *SRegion {
	return &SRegion{cli: cli}
}

func (cli *SCasClient) GetIRegions() ([]cloudprovider.ICloudRegion, error) {
	return []cloudprovider.ICloudRegion{cli.GetRegion()}, nil
}

type SVersionInfo struct {
	CasVersion string `json:"casVersion"`
}

// GetVersion returns casVersion from GET /cas/casrs/version.
func (cli *SCasClient) GetVersion() (string, error) {
	ret := SVersionInfo{}
	err := cli.get("version", nil, &ret)
	if err != nil {
		return "", err
	}
	return ret.CasVersion, nil
}

var (
	_ cloudprovider.ICloudRegion       = (*SRegion)(nil)
	_ cloudprovider.ICloudZone         = (*SZone)(nil)
	_ cloudprovider.ICloudHost         = (*SHost)(nil)
	_ cloudprovider.ICloudStorage      = (*SStorage)(nil)
	_ cloudprovider.ICloudStoragecache = (*SStoragecache)(nil)
	_ cloudprovider.ICloudImage        = (*SImage)(nil)
	_ cloudprovider.ICloudVM           = (*SInstance)(nil)
	_ cloudprovider.ICloudDisk         = (*SDisk)(nil)
	_ cloudprovider.ICloudNic          = (*SInstanceNic)(nil)
	_ cloudprovider.ICloudVpc          = (*SVpc)(nil)
	_ cloudprovider.ICloudWire         = (*SWire)(nil)
	_ cloudprovider.ICloudNetwork      = (*SNetwork)(nil)

	_ cloudprovider.ICloudInstanceSnapshot = (*SVmSnapshot)(nil)
)
