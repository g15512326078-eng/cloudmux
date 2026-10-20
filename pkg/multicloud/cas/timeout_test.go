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
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// stalledListener accepts TCP connections and never answers (no TLS
// handshake, no HTTP response): a hung CAS.
func stalledListener(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var lock sync.Mutex
	conns := []net.Conn{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			lock.Lock()
			conns = append(conns, c)
			lock.Unlock()
			go io.Copy(io.Discard, c)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		lock.Lock()
		defer lock.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return l.Addr().String()
}

func timeoutClient(u string, to sClientTimeouts) *SCasClient {
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(u, testUser, testPass).CloudproviderConfig(cloudprovider.ProviderConfig{Id: "acc1", Name: "cas-test"})}
	cli.timeouts = to
	return cli
}

func assertFast(t *testing.T, start time.Time, limit time.Duration, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("no error")
	}
	if d := time.Since(start); d > limit {
		t.Fatalf("took %s (> %s): %v", d, limit, err)
	}
}

func TestDefaultTimeoutsBounded(t *testing.T) {
	d := sClientTimeouts{}.withDefaults()
	if d.Dial != DEFAULT_DIAL_TIMEOUT || d.Dial > 10*time.Second || d.TLSHandshake > 10*time.Second || d.ResponseHeader > 30*time.Second || d.Request > 5*time.Minute || DEFAULT_PROBE_TIMEOUT > 30*time.Second {
		t.Fatalf("defaults %+v probe %s", d, DEFAULT_PROBE_TIMEOUT)
	}
	if o := (sClientTimeouts{Dial: time.Second}).withDefaults(); o.Dial != time.Second || o.Request != DEFAULT_REQUEST_TIMEOUT {
		t.Fatalf("override %+v", o)
	}
}

// A CAS that accepts the connection but never answers: the response
// header timeout fires, not a two-minute hang.
func TestRequestStalledServerFailsFast(t *testing.T) {
	cli := timeoutClient("http://"+stalledListener(t), sClientTimeouts{ResponseHeader: 300 * time.Millisecond})
	start := time.Now()
	_, err := cli.GetHostBriefs()
	assertFast(t, start, 5*time.Second, err)
	if !isTransportError(err) {
		t.Fatalf("not a transport error: %v", err)
	}
}

func TestRequestStalledTLSHandshakeFailsFast(t *testing.T) {
	cli := timeoutClient("https://"+stalledListener(t), sClientTimeouts{TLSHandshake: 300 * time.Millisecond})
	start := time.Now()
	_, err := cli.GetHostBriefs()
	assertFast(t, start, 5*time.Second, err)
}

func TestRequestOverallTimeout(t *testing.T) {
	// headers arrive, the body never ends
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"host":[`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	cli := timeoutClient(srv.URL, sClientTimeouts{Request: 300 * time.Millisecond})
	start := time.Now()
	_, err := cli.GetHostBriefs()
	assertFast(t, start, 5*time.Second, err)
}

// An unroutable address (TEST-NET-1, RFC 5737): the dial timeout fires
// (or the network says unreachable at once). With the defaults the probe
// must give up within the default dial timeout, not ~135s.
func TestProbeUnroutableFailsFast(t *testing.T) {
	cli := timeoutClient("http://192.0.2.1:9", sClientTimeouts{Dial: 300 * time.Millisecond})
	start := time.Now()
	err := cli.Probe()
	assertFast(t, start, 5*time.Second, err)
	if testing.Short() {
		return
	}
	start = time.Now()
	_, err = NewCasClient(NewCasClientConfig("http://192.0.2.1:9", testUser, testPass))
	assertFast(t, start, DEFAULT_DIAL_TIMEOUT+5*time.Second, err)
}

func TestWithContextCancelAbortsRequest(t *testing.T) {
	cli := timeoutClient("http://"+stalledListener(t), sClientTimeouts{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := cli.WithContext(ctx).GetHostBriefs()
	assertFast(t, start, 5*time.Second, err)
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err %v", err)
	}
	// the base client is not bound to ctx
	if cli.ctx != nil {
		t.Fatal("WithContext changed the base client")
	}
}

func TestWaitTaskStopsOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply(w, dict{"msgId": 5, "completed": false})
	}))
	defer srv.Close()
	cli := timeoutClient(srv.URL, sClientTimeouts{})
	cli.taskPollInterval, cli.taskTimeout = 10*time.Millisecond, time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := cli.WithContext(ctx).waitTask(5)
	assertFast(t, start, 5*time.Second, err)
	if !strings.Contains(err.Error(), "may still run on CAS") {
		t.Fatalf("err %v", err)
	}
}

// A poll that times out does not fail a task that runs on CAS; a streak
// of them does.
func TestWaitTaskToleratesTransportErrors(t *testing.T) {
	for _, c := range []struct {
		stalls int32
		ok     bool
	}{{2, true}, {MAX_TASK_TRANSPORT_ERRS + 2, false}} {
		t.Run(fmt.Sprint(c.stalls), func(t *testing.T) {
			var n int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&n, 1) <= c.stalls {
					<-r.Context().Done()
					return
				}
				reply(w, dict{"msgId": 5, "completed": true, "result": 0})
			}))
			defer srv.Close()
			cli := timeoutClient(srv.URL, sClientTimeouts{ResponseHeader: 100 * time.Millisecond})
			cli.taskPollInterval, cli.taskTimeout = 10*time.Millisecond, 20*time.Second
			_, err := cli.waitTask(5)
			if (err == nil) != c.ok {
				t.Fatalf("err %v", err)
			}
			if !c.ok && !strings.Contains(err.Error(), "consecutive transport errors") {
				t.Fatalf("err %v", err)
			}
		})
	}
}

// ReadOnlyClone and WithContext keep the timeouts.
func TestClonesKeepTimeouts(t *testing.T) {
	cli := timeoutClient("http://"+stalledListener(t), sClientTimeouts{ResponseHeader: 200 * time.Millisecond})
	for name, c := range map[string]*SCasClient{"readonly": cli.ReadOnlyClone(), "ctx": cli.WithContext(context.Background())} {
		start := time.Now()
		_, err := c.GetHostBriefs()
		assertFast(t, start, 5*time.Second, err)
		_ = name
	}
}

// MF-1: a cancel that lands while a poll is in flight still reports the
// task as possibly running, not a bare "context canceled". The server
// holds every poll until the client gives up, so the cancel always hits
// an in-flight request.
func TestWaitTaskCancelDuringPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	cli := timeoutClient(srv.URL, sClientTimeouts{})
	cli.taskPollInterval, cli.taskTimeout = 10*time.Millisecond, time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := cli.WithContext(ctx).waitTask(5)
	assertFast(t, start, 5*time.Second, err)
	if !strings.Contains(err.Error(), "may still run on CAS") || errors.Cause(err) != context.Canceled {
		t.Fatalf("err %v", err)
	}
}

// The same for the other cancellable waits: the error says it was a
// cancel.
func TestWaitLoopsCancelDuringPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	for name, fn := range map[string]func(*SCasClient) error{
		"waitVMNotLive": func(c *SCasClient) error { return c.waitVMNotLive("u1") },
		"waitStorageVolume": func(c *SCasClient) error {
			_, err := c.waitStorageVolume(1, "pool", "vol")
			return err
		},
	} {
		cli := timeoutClient(srv.URL, sClientTimeouts{})
		cli.taskPollInterval, cli.taskTimeout = 10*time.Millisecond, time.Minute
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(100*time.Millisecond, cancel)
		start := time.Now()
		err := fn(cli.WithContext(ctx))
		assertFast(t, start, 5*time.Second, err)
		if !strings.Contains(err.Error(), "cancelled while waiting") || errors.Cause(err) != context.Canceled {
			t.Errorf("%s: %v", name, err)
		}
	}
}
