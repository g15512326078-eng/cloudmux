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
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

type isoEntry struct {
	name string
	data []byte
}

// readISORoot is an independent ISO 9660 reader for the tests: it reads
// the root directory of the volume descriptor in sector vd and returns
// its files (names decoded as UCS-2BE when joliet).
func readISORoot(t *testing.T, img []byte, vd int, joliet bool) (string, []isoEntry) {
	t.Helper()
	d := img[vd*2048 : (vd+1)*2048]
	if string(d[1:6]) != "CD001" {
		t.Fatalf("sector %d: no CD001", vd)
	}
	if joliet && (d[0] != 2 || !bytes.Equal(d[88:91], []byte("%/E"))) {
		t.Fatalf("sector %d is not a Joliet SVD", vd)
	}
	if !joliet && d[0] != 1 {
		t.Fatalf("sector %d is not a PVD", vd)
	}
	if lb := binary.LittleEndian.Uint16(d[128:]); lb != 2048 || binary.BigEndian.Uint16(d[130:]) != 2048 {
		t.Fatalf("logical block size %d", lb)
	}
	label := strings.TrimRight(string(d[40:72]), " ")
	if joliet {
		label = decodeUCS2(d[40:72])
	}
	if vs := binary.LittleEndian.Uint32(d[80:]); int(vs)*2048 != len(img) || binary.BigEndian.Uint32(d[84:]) != vs {
		t.Fatalf("volume space size %d blocks, image %d bytes", vs, len(img))
	}
	root := d[156:190]
	ext := binary.LittleEndian.Uint32(root[2:])
	size := binary.LittleEndian.Uint32(root[10:])
	if binary.BigEndian.Uint32(root[6:]) != ext || binary.BigEndian.Uint32(root[14:]) != size || root[25]&2 == 0 {
		t.Fatalf("bad root record")
	}
	dir := img[int(ext)*2048 : int(ext)*2048+int(size)]
	files := []isoEntry{}
	for off := 0; off < len(dir); {
		l := int(dir[off])
		if l == 0 {
			off = (off/2048 + 1) * 2048
			continue
		}
		r := dir[off : off+l]
		nl := int(r[32])
		name := r[33 : 33+nl]
		if !(nl == 1 && (name[0] == 0 || name[0] == 1)) {
			fe := binary.LittleEndian.Uint32(r[2:])
			fs := binary.LittleEndian.Uint32(r[10:])
			if binary.BigEndian.Uint32(r[6:]) != fe || binary.BigEndian.Uint32(r[14:]) != fs {
				t.Fatalf("both-endian mismatch in %q", name)
			}
			n := string(name)
			if joliet {
				n = decodeUCS2(name)
			}
			files = append(files, isoEntry{name: n, data: img[int(fe)*2048 : int(fe)*2048+int(fs)]})
		}
		off += l
	}
	return label, files
}

func decodeUCS2(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.BigEndian.Uint16(b[2*i:])
	}
	return strings.TrimRight(string(utf16.Decode(u)), " ")
}

func TestBuildISORoundTrip(t *testing.T) {
	a := []byte("<unattend/>\n")
	b := bytes.Repeat([]byte("x"), 5000) // spans sectors
	img, err := BuildISO("TEST_ISO", []SISOFile{{Name: "b-file.txt", Data: b}, {Name: "autounattend.xml", Data: a}}, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(img)%2048 != 0 {
		t.Fatalf("image size %d", len(img))
	}
	label, prim := readISORoot(t, img, 16, false)
	if label != "TEST_ISO" || len(prim) != 2 || prim[0].name != "AUTOUNATTEND.XML;1" || !bytes.Equal(prim[0].data, a) || prim[1].name != "B_FILE.TXT;1" || !bytes.Equal(prim[1].data, b) {
		t.Fatalf("primary tree: %q %+v", label, names(prim))
	}
	jlabel, jol := readISORoot(t, img, 17, true)
	if jlabel != "TEST_ISO" || len(jol) != 2 || jol[0].name != "autounattend.xml" || !bytes.Equal(jol[0].data, a) || jol[1].name != "b-file.txt" || !bytes.Equal(jol[1].data, b) {
		t.Fatalf("joliet tree: %q %+v", jlabel, names(jol))
	}
	if img[18*2048] != 255 || string(img[18*2048+1:18*2048+6]) != "CD001" {
		t.Fatal("no terminator at sector 18")
	}
	// deterministic
	img2, _ := BuildISO("TEST_ISO", []SISOFile{{Name: "autounattend.xml", Data: a}, {Name: "b-file.txt", Data: b}}, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if !bytes.Equal(img, img2) {
		t.Fatal("BuildISO is not deterministic / order independent")
	}
	p := filepath.Join(t.TempDir(), "x.iso")
	os.WriteFile(p, img, 0600)
	if err := CheckISOFile(p); err != nil {
		t.Fatal(err)
	}
}

func names(e []isoEntry) []string {
	r := []string{}
	for _, x := range e {
		r = append(r, x.name)
	}
	return r
}

func TestBuildISORejects(t *testing.T) {
	now := time.Now()
	for name, c := range map[string]struct {
		label string
		files []SISOFile
	}{
		"lower label":  {"bad label", []SISOFile{{Name: "a.txt"}}},
		"long label":   {"ABCDEFGHIJKLMNOPQ", []SISOFile{{Name: "a.txt"}}},
		"no files":     {"OK", nil},
		"dup name":     {"OK", []SISOFile{{Name: "a.txt"}, {Name: "A.TXT"}}},
		"subdir":       {"OK", []SISOFile{{Name: "d/a.txt"}}},
		"empty name":   {"OK", []SISOFile{{Name: ""}}},
		"too big file": {"OK", []SISOFile{{Name: "a.txt", Data: make([]byte, 16<<20+1)}}},
	} {
		if _, err := BuildISO(c.label, c.files, now); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestCheckISOFile(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.iso")
	os.WriteFile(short, []byte("not an iso"), 0600)
	qcow := filepath.Join(dir, "disk.iso")
	os.WriteFile(qcow, append([]byte("QFI\xfb"), make([]byte, 40000)...), 0600)
	for _, p := range []string{short, qcow, filepath.Join(dir, "missing.iso")} {
		if err := CheckISOFile(p); err == nil {
			t.Errorf("%s: want error", p)
		}
	}
}
