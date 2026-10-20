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

package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"yunion.io/x/cloudmux/pkg/multicloud/cas"
)

func winOpts() *WindowsTemplateOptions {
	return &WindowsTemplateOptions{
		WindowsAnswerOptions: WindowsAnswerOptions{OsVersion: "Microsoft Windows Server 2019(64-bit)", ImageIndex: 2, DiskLayout: "mbr"},
		HostId:               1, IsoPool: "isopool", IsoFile: "win.iso", DiskPool: "defaultpool", TemplateName: "w", Vswitch: "vs",
		InstallTimeout: "2h", PollInterval: "30s", AnswerIsoFile: "a.iso",
	}
}

func TestWindowsTemplateOptions(t *testing.T) {
	t.Setenv(WIN_ADMIN_PASSWORD_ENV, "env-Secret-1")
	t.Setenv(WIN_PRODUCT_KEY_ENV, "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY")
	o := winOpts()
	io, err := o.importOptions()
	if err != nil {
		t.Fatal(err)
	}
	if io.InstallTimeout != 2*time.Hour || io.PollInterval != 30*time.Second || io.IsoStager != nil || io.AnswerStager != nil {
		t.Fatalf("%+v", io)
	}
	if io.Unattend.AdminPassword.Reveal() != "env-Secret-1" || io.Unattend.ProductKey.Reveal() != "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY" {
		t.Fatal("secrets not read from the environment")
	}
	if s := fmt.Sprintf("%+v", io); strings.Contains(s, "env-Secret-1") || strings.Contains(s, "PQRST") {
		t.Fatal("options print a secret")
	}
	o.AdminPassword = "flag-pw"
	if io, _ := o.importOptions(); io.Unattend.AdminPassword.Reveal() != "flag-pw" || len(o.secretWarnings()) != 1 {
		t.Fatal("flag password / warning")
	}

	// http answer upload on a pool-file install ISO
	h := winOpts()
	h.AnswerIsoFile, h.AnswerUpload, h.ServeAddr = "", "http", "198.51.100.7:9292"
	hi, err := h.importOptions()
	if err != nil || hi.AnswerStager == nil || hi.AnswerMode != "http" {
		t.Fatalf("answer http: %v", err)
	}
	st, err := hi.AnswerStager("/tmp/answer.iso")
	hs, ok := st.(*cas.SHTTPStager)
	if err != nil || !ok || hs.Options.LocalPath != "/tmp/answer.iso" || hs.Options.ImageType != "raw" || !hs.Options.Sensitive {
		t.Fatalf("answer stager %v %+v", err, st)
	}

	// http install ISO: must be an ISO file
	dir := t.TempDir()
	notISO := filepath.Join(dir, "x.iso")
	os.WriteFile(notISO, []byte("nope"), 0600)
	b := winOpts()
	b.HttpUpload, b.ServeAddr = notISO, "198.51.100.7:9292"
	if _, err := b.importOptions(); err == nil {
		t.Fatal("non-ISO upload accepted")
	}
	isoData, _ := cas.BuildISO("X", []cas.SISOFile{{Name: "a.txt", Data: []byte("a")}}, time.Now())
	good := filepath.Join(dir, "good.iso")
	os.WriteFile(good, isoData, 0600)
	b.HttpUpload = good
	if bi, err := b.importOptions(); err != nil || bi.IsoStager == nil || bi.IsoStager.(*cas.SHTTPStager).Options.Sensitive {
		t.Fatalf("http install ISO: %v", err)
	}
	m := winOpts()
	m.MinInstalledGb, m.WaitCastools, m.Castools = 7, true, true
	if mi, err := m.importOptions(); err != nil || mi.MinInstalledGB != 7 || !mi.WaitCastools {
		t.Fatalf("min installed / castools: %v", err)
	}

	for name, mod := range map[string]func(o *WindowsTemplateOptions){
		"bad timeout":           func(o *WindowsTemplateOptions) { o.InstallTimeout = "soon" },
		"tiny poll":             func(o *WindowsTemplateOptions) { o.PollInterval = "10ms" },
		"serve-addr unused":     func(o *WindowsTemplateOptions) { o.ServeAddr = "a:1" },
		"ssh flag unused":       func(o *WindowsTemplateOptions) { o.SshKnownHosts = "k" },
		"http-upload no mode":   func(o *WindowsTemplateOptions) { o.UploadMode = "pool-file"; o.HttpUpload = "x" },
		"http answer no serve":  func(o *WindowsTemplateOptions) { o.AnswerIsoFile = ""; o.AnswerUpload = "http" },
		"ssh and http uploads":  func(o *WindowsTemplateOptions) { o.SshUpload, o.HttpUpload = "a", "b" },
		"ssh mode without file": func(o *WindowsTemplateOptions) { o.UploadMode = "ssh" },
	} {
		o := winOpts()
		mod(o)
		if _, err := o.importOptions(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestWriteAnswerISO(t *testing.T) {
	out := filepath.Join(t.TempDir(), "answer.iso")
	if err := writeAnswerISO(out, []byte("data")); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(out)
	if st.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if err := writeAnswerISO(out, []byte("other")); err == nil {
		t.Fatal("overwrote an existing file")
	}
	if b, _ := os.ReadFile(out); string(b) != "data" {
		t.Fatal("content changed")
	}
}

func TestWindowsTemplateHelpMarksUnverified(t *testing.T) {
	for _, want := range []string{"UNVERIFIED", "virtio", "boot-order", "always disconnected", "keeps the temporary VM"} {
		if want == "boot-order" {
			want = "boot-order behaviour"
		}
		if !strings.Contains(windowsTemplateHelp, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestRunWindowsAnswerISO(t *testing.T) {
	t.Setenv(WIN_ADMIN_PASSWORD_ENV, "Smoke!Pw1")
	out := filepath.Join(t.TempDir(), "a.iso")
	a := &WindowsAnswerIsoOptions{WindowsAnswerOptions: WindowsAnswerOptions{OsVersion: "Microsoft Windows Server 2016(64-bit)", ImageName: "Windows Server 2016 SERVERDATACENTER"}, Out: out}
	n, err := runWindowsAnswerISO(a, time.Now())
	if err != nil || n == 0 {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if len(data) != n || cas.CheckISOFile(out) != nil || !strings.Contains(string(data), `A:\amd64\2k16`) || strings.Contains(string(data), "Smoke!Pw1") {
		t.Fatal("answer ISO content")
	}
	a.ImageIndex = 1 // both index and name
	a.Out = filepath.Join(t.TempDir(), "b.iso")
	if _, err := runWindowsAnswerISO(a, time.Now()); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(a.Out); !os.IsNotExist(err) {
		t.Fatal("file written on error")
	}
}

func TestWindowsTemplateNoKeepMediaFlag(t *testing.T) {
	if _, ok := reflect.TypeOf(WindowsTemplateOptions{}).FieldByName("KeepMedia"); ok {
		t.Fatal("--keep-media must not exist")
	}
}
