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
	"strings"
	"testing"
	"time"

	"yunion.io/x/pkg/util/httputils"
)

func newTaskClient(t *testing.T, h http.HandlerFunc) *SCasClient {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cli := &SCasClient{CasClientConfig: NewCasClientConfig(srv.URL, testUser, testPass)}
	cli.taskPollInterval = 5 * time.Millisecond
	cli.taskTimeout = time.Second
	return cli
}

// msgId -1 means the write finished inline: no message polling.
func TestDoAsyncMsgIdMinusOneIsSync(t *testing.T) {
	polled := false
	cli := newTaskClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cas/casrs/message/") {
			polled = true
		}
		reply(w, dict{"msgId": -1, "completed": false, "targetId": 7})
	})
	msg, err := cli.doAsync(httputils.PUT, "vm/modify", nil, nil)
	if err != nil || msg.TargetId != 7 || polled {
		t.Fatalf("msg=%+v err=%v polled=%v", msg, err, polled)
	}
}

func TestDoAsyncRejectsOtherNegativeMsgId(t *testing.T) {
	cli := newTaskClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, dict{"msgId": -2})
	})
	if _, err := cli.doAsync(httputils.PUT, "vm/modify", nil, nil); err == nil {
		t.Fatal("msgId -2 accepted")
	}
}

// 403 while polling is transient for CAS; keep polling until done.
func TestWaitTaskToleratesTransient403(t *testing.T) {
	n := 0
	cli := newTaskClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if n <= 2 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		reply(w, dict{"msgId": 9, "completed": true, "result": 0, "targetId": 3})
	})
	msg, err := cli.waitTask(9)
	if err != nil || msg.TargetId != 3 {
		t.Fatalf("msg=%+v err=%v", msg, err)
	}
}

func TestWaitTaskFailureIncludesKeyValue(t *testing.T) {
	cli := newTaskClient(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, dict{"msgId": 9, "completed": true, "result": 1, "failMsg": "no space",
			"keyValue": []dict{{"key": "poolName", "value": "defaultpool"}}})
	})
	_, err := cli.waitTask(9)
	if err == nil || !strings.Contains(err.Error(), "no space") || !strings.Contains(err.Error(), "poolName=defaultpool") {
		t.Fatalf("err = %v", err)
	}
}
