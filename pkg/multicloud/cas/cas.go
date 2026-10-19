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
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/icholy/digest"

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

	debug bool
}

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

type SCasClient struct {
	*CasClientConfig

	lock   sync.Mutex
	client *http.Client
}

// NewCasClient builds a client and probes GET /cas/casrs/host (the path
// upstream cas-clitool.sh uses), which checks connectivity and Digest
// credentials in one go.
func NewCasClient(cfg *CasClientConfig) (*SCasClient, error) {
	cli := &SCasClient{CasClientConfig: cfg}
	_, err := cli.GetHostBriefs()
	if err != nil {
		return nil, errors.Wrapf(err, "probe %s", cfg.accessUrl)
	}
	return cli, nil
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

// notFoundErrorCodes are 409 Error-Code values meaning "object does not
// exist". api.yaml documents none (and the review lists none), so this is
// empty until confirmed on a real CAS; see isNotFound.
var notFoundErrorCodes = map[string]bool{}

// isNotFound reports whether a 409 business error means the target does
// not exist: either a known Error-Code, or an Error-Message containing
// "不存在" ("does not exist"), the wording api.yaml uses in its error
// message examples (e.g. "子网“suet_post”不存在。").
func (e *SCasError) isNotFound() bool {
	if notFoundErrorCodes[e.ErrorCode] {
		return true
	}
	return strings.Contains(e.ErrorMessage, "不存在")
}

func (cli *SCasClient) url(res string, params url.Values) string {
	u := fmt.Sprintf("%s%s/%s", cli.accessUrl, API_PREFIX, strings.TrimPrefix(res, "/"))
	if len(params) > 0 {
		u = fmt.Sprintf("%s?%s", u, params.Encode())
	}
	return u
}

func (cli *SCasClient) request(method httputils.THttpMethod, res string, params url.Values, body jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	u := cli.url(res, params)
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(body.String())
	}
	req, err := http.NewRequestWithContext(context.Background(), string(method), u, reader)
	if err != nil {
		return nil, errors.Wrapf(err, "NewRequest %s %s", method, u)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	resp, err := cli.getDefaultClient().Do(req)
	if err != nil {
		// unwrap *url.Error so causes such as ErrAccountReadOnly survive
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err
		}
		return nil, errors.Wrapf(err, "%s %s", method, u)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "read body %s %s", method, u)
	}
	if cli.debug {
		log.Debugf("%s %s -> %d %s", method, u, resp.StatusCode, redactBody(data))
	}
	if resp.StatusCode >= 300 {
		ce := &SCasError{
			Method:       string(method),
			Url:          u,
			StatusCode:   resp.StatusCode,
			ErrorCode:    resp.Header.Get("Error-Code"),
			ErrorMessage: resp.Header.Get("Error-Message"),
			Body:         redactBody(data),
		}
		switch resp.StatusCode {
		case http.StatusNotFound:
			return nil, errors.Wrap(cloudprovider.ErrNotFound, ce.Error())
		case http.StatusUnauthorized:
			return nil, errors.Wrap(cloudprovider.ErrInvalidAccessKey, ce.Error())
		case http.StatusForbidden:
			return nil, errors.Wrap(cloudprovider.ErrNoPermission, ce.Error())
		case http.StatusConflict:
			if ce.isNotFound() {
				return nil, errors.Wrap(cloudprovider.ErrNotFound, ce.Error())
			}
		}
		return nil, ce
	}
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
		query.Set("limit", fmt.Sprintf("%d", limit))
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
	return []string{
		cloudprovider.CLOUD_CAPABILITY_COMPUTE + cloudprovider.READ_ONLY_SUFFIX,
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
)
