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
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

const (
	testWinPassword = "S3cr3t!<&>Pw"
	testWinKey      = "ABCDE-FGHIJ-KLMNO-PQRST-UVWXY"
)

func testUnattend() SUnattendOptions {
	return SUnattendOptions{
		OsVersion: "Microsoft Windows Server 2019(64-bit)", ImageIndex: 2,
		AdminPassword: SecretString(testWinPassword), ProductKey: SecretString(testWinKey),
		CastoolsInstaller: DEFAULT_CASTOOLS_INSTALLER, CastoolsArgs: "/S",
	}
}

func wellFormed(t *testing.T, data []byte) {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("not well-formed: %v\n%s", err, data)
		}
	}
}

func TestUnattendRender(t *testing.T) {
	o := testUnattend()
	data, err := o.Render()
	if err != nil {
		t.Fatal(err)
	}
	wellFormed(t, data)
	s := string(data)
	for _, want := range []string{
		`<Path>A:\amd64\2k19</Path>`,
		`<settings pass="windowsPE">`, `<settings pass="offlineServicing">`, `<settings pass="oobeSystem">`,
		`Microsoft-Windows-PnpCustomizationsWinPE`, `Microsoft-Windows-PnpCustomizationsNonWinPE`,
		`<DiskID>0</DiskID>`, `<WillWipeDisk>true</WillWipeDisk>`,
		`<Key>/IMAGE/INDEX</Key>`, `<Value>2</Value>`,
		`processorArchitecture="amd64"`,
		UNATTEND_SYSPREP_COMMAND, `/generalize`, `/oobe`, `/shutdown`,
		`CAS_tools_setup.exe /S`,
		`<PlainText>false</PlainText>`,
		encodeUnattendPassword(testWinPassword, "AdministratorPassword"),
		encodeUnattendPassword(testWinPassword, "Password"),
		testWinKey,
		`<TimeZone>China Standard Time</TimeZone>`, `<UILanguage>en-US</UILanguage>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered XML lacks %q", want)
		}
	}
	if strings.Contains(s, testWinPassword) || strings.Contains(s, "<PlainText>true") {
		t.Error("plain-text password in the answer file")
	}
	if strings.Index(s, "CAS_tools_setup.exe") > strings.Index(s, UNATTEND_SYSPREP_COMMAND) {
		t.Error("CAStools must install before Sysprep")
	}

	// decode a bit of structure: the encoded password element
	var doc struct {
		Settings []struct {
			Pass       string `xml:"pass,attr"`
			Components []struct {
				Name      string `xml:"name,attr"`
				UserAccts struct {
					Admin struct {
						Value     string `xml:"Value"`
						PlainText string `xml:"PlainText"`
					} `xml:"AdministratorPassword"`
				} `xml:"UserAccounts"`
			} `xml:"component"`
		} `xml:"settings"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, st := range doc.Settings {
		for _, c := range st.Components {
			if st.Pass == "oobeSystem" && c.Name == "Microsoft-Windows-Shell-Setup" {
				found = c.UserAccts.Admin.Value == encodeUnattendPassword(testWinPassword, "AdministratorPassword") && c.UserAccts.Admin.PlainText == "false"
			}
		}
	}
	if !found {
		t.Error("oobeSystem Shell-Setup AdministratorPassword not encoded")
	}
}

func TestUnattendRedacted(t *testing.T) {
	o := testUnattend()
	red, err := o.RenderRedacted()
	if err != nil {
		t.Fatal(err)
	}
	wellFormed(t, red)
	s := string(red)
	for _, secret := range []string{testWinPassword, testWinKey, encodeUnattendPassword(testWinPassword, "AdministratorPassword"), encodeUnattendPassword(testWinPassword, "Password")} {
		if strings.Contains(s, secret) {
			t.Errorf("redacted XML contains %q", secret)
		}
	}
	if !strings.Contains(s, "&lt;redacted&gt;") && !strings.Contains(s, redactedSecret) {
		t.Error("no redaction marker")
	}
	// the options themselves never print their secrets
	for _, out := range []string{fmt.Sprintf("%v", o), fmt.Sprintf("%+v", o), fmt.Sprintf("%#v", o), fmt.Sprintf("%s %q %x", o.AdminPassword, o.AdminPassword, o.ProductKey)} {
		if strings.Contains(out, testWinPassword) || strings.Contains(out, testWinKey) {
			t.Errorf("fmt leaks a secret: %s", out)
		}
	}
	js, _ := json.Marshal(o)
	if strings.Contains(string(js), testWinPassword) || strings.Contains(string(js), testWinKey) {
		t.Errorf("json leaks a secret: %s", js)
	}
}

func TestUnattendVariants(t *testing.T) {
	o := SUnattendOptions{OsVersion: "Microsoft Windows 10(32-bit)", ImageName: "Windows 10 Pro & <x>", AdminPassword: "p", DiskLayout: "gpt",
		DriverPaths: []string{`A:\i386\w10`, `E:\viostor\w10\x86`}, Locale: "zh-CN", TimeZone: "UTC"}
	data, err := o.Render()
	if err != nil {
		t.Fatal(err)
	}
	wellFormed(t, data)
	s := string(data)
	for _, want := range []string{`processorArchitecture="x86"`, `<Key>/IMAGE/NAME</Key>`, `Windows 10 Pro &amp; &lt;x&gt;`, `A:\i386\w10`, `E:\viostor\w10\x86`, `<Type>EFI</Type>`, `<Type>MSR</Type>`, `<UILanguage>zh-CN</UILanguage>`, `<TimeZone>UTC</TimeZone>`} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(s, "CAS_tools_setup.exe") || strings.Contains(s, "<ProductKey>") {
		t.Error("castools / product key rendered although not asked for")
	}
}

func TestUnattendValidation(t *testing.T) {
	base := testUnattend()
	for name, mod := range map[string]func(o *SUnattendOptions){
		"linux os":         func(o *SUnattendOptions) { o.OsVersion = "CentOS 7" },
		"index and name":   func(o *SUnattendOptions) { o.ImageName = "x" },
		"no edition":       func(o *SUnattendOptions) { o.ImageIndex = 0 },
		"no password":      func(o *SUnattendOptions) { o.AdminPassword = "" },
		"password newline": func(o *SUnattendOptions) { o.AdminPassword = "a\nb" + testWinPassword },
		"bad key":          func(o *SUnattendOptions) { o.ProductKey = "SECRETKEY-0001" },
		"bad locale":       func(o *SUnattendOptions) { o.Locale = "en_US<" },
		"bad driver path":  func(o *SUnattendOptions) { o.DriverPaths = []string{"A:/x\"y"} },
		"no default path":  func(o *SUnattendOptions) { o.OsVersion = "Microsoft Windows XP" },
		"bad layout":       func(o *SUnattendOptions) { o.DiskLayout = "lvm" },
		"castools inject":  func(o *SUnattendOptions) { o.CastoolsArgs = "/S & del C:" },
		"tz inject":        func(o *SUnattendOptions) { o.TimeZone = "</TimeZone>" },
	} {
		o := base
		mod(&o)
		_, err := o.Render()
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		for _, secret := range []string{testWinPassword, testWinKey, "SECRETKEY-0001"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error leaks a secret: %v", name, err)
			}
		}
	}
}

func TestEncodeUnattendPassword(t *testing.T) {
	// "pw" + "Password" as UTF-16LE, base64
	if got := encodeUnattendPassword("pw", "Password"); got != "cAB3AFAAYQBzAHMAdwBvAHIAZAA=" {
		t.Fatalf("got %s", got)
	}
}

func TestAnswerISOContent(t *testing.T) {
	o := testUnattend()
	img, err := o.BuildAnswerISO(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := o.Render()
	label, prim := readISORoot(t, img, 16, false)
	_, jol := readISORoot(t, img, 17, true)
	if label != ANSWER_ISO_LABEL || len(prim) != 1 || prim[0].name != "AUTOUNATTEND.XML;1" || !bytes.Equal(prim[0].data, want) {
		t.Fatalf("primary: %s %v", label, names(prim))
	}
	if len(jol) != 1 || jol[0].name != "autounattend.xml" || !bytes.Equal(jol[0].data, want) {
		t.Fatalf("joliet: %v", names(jol))
	}
	if !bytes.Contains(img, []byte(encodeUnattendPassword(testWinPassword, "AdministratorPassword"))) || !bytes.Contains(img, []byte(testWinKey)) {
		t.Fatal("the answer ISO must carry the encoded secrets")
	}
}
