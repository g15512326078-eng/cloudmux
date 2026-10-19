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

// Package cas implements a read-only (P0) cloudmux provider for H3C CAS
// (CVM/CVK) virtualization, talking to the CAS REST API under
// {AccessUrl}/cas/casrs/ with HTTP Digest authentication.
//
// This is not the H3C public/CloudOS provider (pkg/multicloud/h3c) and has
// nothing to do with the Keystone CAS SSO driver.
package cas

import (
	"fmt"
	"net/url"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/util/httputils"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

// STaskMsg is the body returned by write ops that start an async job
// (api.yaml definitions/RsTaskMsg, xml name taskMsg). Start/stop/restart/
// powerOff/delete/deploy all document this shape. completed/progress here
// describe the submission ack; the authoritative completion state is on
// GET /cas/casrs/message/{msgId} (SMessage below).
type STaskMsg struct {
	MsgId      int64  `json:"msgId"`
	Completed  bool   `json:"completed"`
	Progress   int    `json:"progress"`
	TargetId   int64  `json:"targetId"`
	TargetName string `json:"targetName"`
	EventType  int    `json:"eventType"`
}

// SMessage is GET /cas/casrs/message/{msgId} (api.yaml: "查看任务详细信息",
// xml name taskMsg). result is documented as 0=success, 1=failure,
// 2=partial success. failMsg is "失败信息(当任务成功时内容为任务信息)".
type SMessage struct {
	MsgId      int64  `json:"msgId"`
	Name       string `json:"name"`
	TargetId   int64  `json:"targetId"`
	TargetName string `json:"targetName"`
	Detail     string `json:"detail"`
	Completed  bool   `json:"completed"`
	Result     int    `json:"result"`
	FailMsg    string `json:"failMsg"`
	EventType  int    `json:"eventType"`
}

const (
	// TASK_RESULT_* match api.yaml on GET /message/{msgId}.result.
	TASK_RESULT_SUCCESS         = 0
	TASK_RESULT_FAILURE         = 1
	TASK_RESULT_PARTIAL_SUCCESS = 2

	// Defaults for waitTask. Neither the poll interval nor the overall
	// timeout is documented in api.yaml; 5s / 10m mirrors the nutanix
	// client and must be confirmed on a real CAS.
	DEFAULT_TASK_POLL_INTERVAL = 5 * time.Second
	DEFAULT_TASK_TIMEOUT       = 10 * time.Minute
)

// parseTaskMsg decodes an RsTaskMsg body. A missing/zero msgId with
// completed=true is treated as a synchronous success (UNVERIFIED: api.yaml
// examples always show completed=false and a non-zero msgId).
func parseTaskMsg(resp jsonutils.JSONObject) (*STaskMsg, error) {
	resp = unwrapRoot(resp)
	msg := &STaskMsg{}
	if err := resp.Unmarshal(msg); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal RsTaskMsg")
	}
	return msg, nil
}

// GetMessage is GET /cas/casrs/message/{msgId}.
func (cli *SCasClient) GetMessage(msgId int64) (*SMessage, error) {
	resp, err := cli.request(httputils.GET, fmt.Sprintf("message/%d", msgId), nil, nil)
	if err != nil {
		return nil, err
	}
	resp = unwrapRoot(resp)
	msg := &SMessage{}
	if err := resp.Unmarshal(msg); err != nil {
		return nil, errors.Wrapf(err, "Unmarshal message %d", msgId)
	}
	if msg.MsgId == 0 {
		msg.MsgId = msgId
	}
	return msg, nil
}

// waitTask polls GET /cas/casrs/message/{msgId} until completed is true.
// On completion: result 0 -> nil; result 1 -> error with failMsg/detail;
// result 2 (partial success) -> error (treated as failure until real-CAS
// behaviour is known). Unknown: whether a still-running task can return
// 404/409 for the message id, and whether cancel is ever needed.
func (cli *SCasClient) waitTask(msgId int64) (*SMessage, error) {
	if msgId == 0 {
		return nil, errors.Errorf("waitTask: empty msgId")
	}
	interval, timeout := cli.taskPollInterval, cli.taskTimeout
	if interval <= 0 {
		interval = DEFAULT_TASK_POLL_INTERVAL
	}
	if timeout <= 0 {
		timeout = DEFAULT_TASK_TIMEOUT
	}
	var last *SMessage
	err := cloudprovider.Wait(interval, timeout, func() (bool, error) {
		msg, err := cli.GetMessage(msgId)
		if err != nil {
			return false, err
		}
		last = msg
		log.Debugf("cas task %d completed=%v result=%d progress from submit unknown failMsg=%q",
			msg.MsgId, msg.Completed, msg.Result, msg.FailMsg)
		if !msg.Completed {
			return false, nil
		}
		switch msg.Result {
		case TASK_RESULT_SUCCESS:
			return true, nil
		case TASK_RESULT_FAILURE, TASK_RESULT_PARTIAL_SUCCESS:
			text := msg.FailMsg
			if len(text) == 0 {
				text = msg.Detail
			}
			return false, errors.Errorf("cas task %d result=%d: %s", msg.MsgId, msg.Result, text)
		default:
			return false, errors.Errorf("cas task %d unknown result=%d detail=%q", msg.MsgId, msg.Result, msg.Detail)
		}
	})
	if err != nil {
		return last, errors.Wrapf(err, "waitTask %d", msgId)
	}
	return last, nil
}

// doAsync issues a write that returns RsTaskMsg and waits for the task.
// If the ack already has completed=true and msgId==0, it returns without
// polling (UNVERIFIED sync path). Otherwise it requires a non-zero msgId.
func (cli *SCasClient) doAsync(method httputils.THttpMethod, res string, params url.Values, body jsonutils.JSONObject) (*SMessage, error) {
	resp, err := cli.request(method, res, params, body)
	if err != nil {
		return nil, err
	}
	ack, err := parseTaskMsg(resp)
	if err != nil {
		return nil, err
	}
	if ack.MsgId == 0 {
		if ack.Completed {
			return &SMessage{Completed: true, Result: TASK_RESULT_SUCCESS, TargetId: ack.TargetId, TargetName: ack.TargetName}, nil
		}
		return nil, errors.Errorf("%s %s returned RsTaskMsg without msgId", method, res)
	}
	if ack.Completed {
		// UNVERIFIED: start/stop examples show completed=false on submit.
		// If a build completes inline, still confirm via GetMessage.
	}
	return cli.waitTask(ack.MsgId)
}
