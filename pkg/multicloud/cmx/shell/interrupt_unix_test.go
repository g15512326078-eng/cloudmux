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

//go:build unix

package shell

import (
	"syscall"
	"testing"
	"time"
)

func TestInterruptContext(t *testing.T) {
	ctx, stop := interruptContext()
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the import context")
	}
	stop()
	stop() // idempotent

	// a normal stop cancels too, without a signal
	ctx2, stop2 := interruptContext()
	stop2()
	if ctx2.Err() == nil {
		t.Fatal("stop did not cancel")
	}
}
