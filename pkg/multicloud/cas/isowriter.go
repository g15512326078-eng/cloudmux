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

// A minimal ISO 9660 writer for the Windows answer media: one volume, a
// root directory with a few small regular files, a primary volume
// descriptor (ISO 9660 level 2 names, upper case, ";1") and a Joliet
// supplementary descriptor (UCS-2 names, original case), as written by
// genisoimage -J. Windows Setup reads either; Joliet keeps the name
// "autounattend.xml" exactly. No Rock Ridge, El Torito, subdirectories
// or multi-extent files.
//
// Why not a library: the vendor tree has no ISO 9660 writer, a new
// dependency would have to be vendored for ~250 lines of fixed-layout
// code, and a floppy image is not an option because the only floppy
// slot carries the virtio drivers (autoVirtio). The format is ECMA-119
// (ISO 9660) plus the Joliet specification; nothing is taken from other
// implementations.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const (
	isoSector = 2048
	// layout (sectors): 0-15 system area, 16 PVD, 17 Joliet SVD,
	// 18 terminator, 19-22 path tables (L/M primary, L/M Joliet),
	// 23 primary root, 24 Joliet root, 25.. file data
	isoPVD       = 16
	isoSVD       = 17
	isoTerm      = 18
	isoPathL     = 19
	isoPathM     = 20
	isoJPathL    = 21
	isoJPathM    = 22
	isoRoot      = 23
	isoJRoot     = 24
	isoFirstData = 25
	// isoMaxFile bounds each file (answer files are a few KiB).
	isoMaxFile = 16 << 20
)

// SISOFile is one file in the root directory.
type SISOFile struct {
	Name string // as Joliet shows it, e.g. "autounattend.xml"
	Data []byte
}

func isoBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
}

func isoBoth16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:2], v)
	binary.BigEndian.PutUint16(b[2:4], v)
}

// isoPrimaryName maps a name to ISO 9660 d-characters plus ";1".
func isoPrimaryName(name string) string {
	up := strings.ToUpper(name)
	b := strings.Builder{}
	for _, r := range up {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String() + ";1"
}

func isoUCS2(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(out[2*i:], c)
	}
	return out
}

// isoDirDate is the 7-byte directory record date (UTC).
func isoDirDate(t time.Time) []byte {
	t = t.UTC()
	return []byte{byte(t.Year() - 1900), byte(t.Month()), byte(t.Day()), byte(t.Hour()), byte(t.Minute()), byte(t.Second()), 0}
}

// isoVolDate is the 17-byte volume descriptor date; zero time = unset.
func isoVolDate(t time.Time) []byte {
	if t.IsZero() {
		b := bytes.Repeat([]byte{'0'}, 16)
		return append(b, 0)
	}
	t = t.UTC()
	return append([]byte(fmt.Sprintf("%04d%02d%02d%02d%02d%02d00", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())), 0)
}

func isoDirRecord(name []byte, lba, size uint32, dir bool, t time.Time) []byte {
	n := 33 + len(name)
	if n%2 == 1 {
		n++
	}
	r := make([]byte, n)
	r[0] = byte(n)
	isoBoth32(r[2:10], lba)
	isoBoth32(r[10:18], size)
	copy(r[18:25], isoDirDate(t))
	if dir {
		r[25] = 2
	}
	isoBoth16(r[28:32], 1)
	r[32] = byte(len(name))
	copy(r[33:], name)
	return r
}

func isoPathTable(rootLBA uint32, bigEndian bool) []byte {
	pt := make([]byte, 10)
	pt[0] = 1 // root id length
	if bigEndian {
		binary.BigEndian.PutUint32(pt[2:6], rootLBA)
		binary.BigEndian.PutUint16(pt[6:8], 1)
	} else {
		binary.LittleEndian.PutUint32(pt[2:6], rootLBA)
		binary.LittleEndian.PutUint16(pt[6:8], 1)
	}
	return pt // pt[8] = 0 (root id), pt[9] pad
}

// padA fills a fixed-width a/d-character field with spaces.
func padA(b []byte, s string) {
	for i := range b {
		b[i] = ' '
	}
	copy(b, s)
}

// padJ fills a Joliet UCS-2 field: the string, then UCS-2 spaces.
func padJ(b []byte, s string) {
	for i := 0; i+1 < len(b); i += 2 {
		b[i], b[i+1] = 0, ' '
	}
	copy(b, isoUCS2(s))
}

// BuildISO returns an ISO 9660 + Joliet image with files in its root.
// label is the volume id (d-characters, at most 16 so the Joliet copy
// fits). t is used for every timestamp (deterministic output).
func BuildISO(label string, files []SISOFile, t time.Time) ([]byte, error) {
	if len(files) == 0 {
		return nil, errors.Wrap(cloudprovider.ErrMissingParameter, "iso: no files")
	}
	if len(label) == 0 || len(label) > 16 || strings.Trim(strings.ToUpper(label), "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "iso: volume label %q (A-Z 0-9 _, at most 16)", label)
	}
	label = strings.ToUpper(label)
	fs := append([]SISOFile{}, files...)
	sort.Slice(fs, func(i, j int) bool { return isoPrimaryName(fs[i].Name) < isoPrimaryName(fs[j].Name) })
	seen := map[string]bool{}
	lba := uint32(isoFirstData)
	extents := make([]uint32, len(fs))
	for i, f := range fs {
		pn := isoPrimaryName(f.Name)
		if len(f.Name) == 0 || len(f.Name) > 64 || len(pn) > 31 || strings.ContainsAny(f.Name, "/\\:*?\"<>|;") {
			return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "iso: file name %q", f.Name)
		}
		if seen[pn] {
			return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "iso: two files map to %s", pn)
		}
		seen[pn] = true
		if len(f.Data) > isoMaxFile {
			return nil, errors.Wrapf(cloudprovider.ErrInputParameter, "iso: %s is %d bytes (max %d)", f.Name, len(f.Data), isoMaxFile)
		}
		extents[i] = lba
		lba += uint32((len(f.Data) + isoSector - 1) / isoSector)
		if len(f.Data) == 0 {
			extents[i] = 0 // empty file: no extent
		}
	}
	total := lba
	img := make([]byte, int(total)*isoSector)
	sec := func(n int) []byte { return img[n*isoSector : (n+1)*isoSector] }

	// root directories
	buildRoot := func(at int, joliet bool) error {
		d := sec(at)
		off := 0
		put := func(r []byte) error {
			if off+len(r) > isoSector {
				return errors.Wrapf(cloudprovider.ErrInputParameter, "iso: too many files for one directory sector")
			}
			copy(d[off:], r)
			off += len(r)
			return nil
		}
		put(isoDirRecord([]byte{0}, uint32(at), isoSector, true, t))
		put(isoDirRecord([]byte{1}, uint32(at), isoSector, true, t))
		for i, f := range fs {
			name := []byte(isoPrimaryName(f.Name))
			if joliet {
				name = isoUCS2(f.Name)
			}
			if err := put(isoDirRecord(name, extents[i], uint32(len(f.Data)), false, t)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := buildRoot(isoRoot, false); err != nil {
		return nil, err
	}
	if err := buildRoot(isoJRoot, true); err != nil {
		return nil, err
	}
	copy(sec(isoPathL), isoPathTable(isoRoot, false))
	copy(sec(isoPathM), isoPathTable(isoRoot, true))
	copy(sec(isoJPathL), isoPathTable(isoJRoot, false))
	copy(sec(isoJPathM), isoPathTable(isoJRoot, true))
	for i, f := range fs {
		if len(f.Data) > 0 {
			copy(img[int(extents[i])*isoSector:], f.Data)
		}
	}

	vd := func(at int, joliet bool) {
		d := sec(at)
		d[0] = 1
		if joliet {
			d[0] = 2
		}
		copy(d[1:6], "CD001")
		d[6] = 1
		str := padA
		if joliet {
			str = padJ
		}
		str(d[8:40], "")
		str(d[40:72], label)
		isoBoth32(d[80:88], total)
		if joliet {
			copy(d[88:91], "%/E") // UCS-2 level 3
		}
		isoBoth16(d[120:124], 1)
		isoBoth16(d[124:128], 1)
		isoBoth16(d[128:132], isoSector)
		isoBoth32(d[132:140], 10)
		root, pl, pm := uint32(isoRoot), uint32(isoPathL), uint32(isoPathM)
		if joliet {
			root, pl, pm = isoJRoot, isoJPathL, isoJPathM
		}
		binary.LittleEndian.PutUint32(d[140:144], pl)
		binary.BigEndian.PutUint32(d[148:152], pm)
		copy(d[156:190], isoDirRecord([]byte{0}, root, isoSector, true, t))
		str(d[190:318], "")
		str(d[318:446], "")
		str(d[446:574], "")
		str(d[574:702], "CLOUDMUX CAS")
		str(d[702:739], "")
		str(d[739:776], "")
		str(d[776:813], "")
		copy(d[813:830], isoVolDate(t))
		copy(d[830:847], isoVolDate(t))
		copy(d[847:864], isoVolDate(time.Time{}))
		copy(d[864:881], isoVolDate(time.Time{}))
		d[881] = 1
	}
	vd(isoPVD, false)
	vd(isoSVD, true)
	term := sec(isoTerm)
	term[0] = 255
	copy(term[1:6], "CD001")
	term[6] = 1
	return img, nil
}

// CheckISOFile refuses a local file that is not ISO 9660 media: the
// primary volume descriptor at sector 16 must carry "CD001".
func CheckISOFile(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 6)
	if _, err := f.ReadAt(buf, 16*isoSector); err != nil {
		return errors.Wrapf(cloudprovider.ErrNotSupported, "%s is not an ISO 9660 image (too short)", p)
	}
	if string(buf[1:6]) != "CD001" {
		return errors.Wrapf(cloudprovider.ErrNotSupported, "%s is not an ISO 9660 image (no CD001 at sector 16)", p)
	}
	return nil
}
