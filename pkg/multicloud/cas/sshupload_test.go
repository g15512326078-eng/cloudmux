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

//go:build cas_ssh

package cas

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const sshTestPassword = "S3cret-Upload-Pw"

// sshTestServer is an in-process SSH server that runs exec requests with
// the local /bin/sh, so the real remote commands are exercised.
type sshTestServer struct {
	addr    string
	hostKey ssh.PublicKey
	conns   int32
	execs   []string
	lock    sync.Mutex
	corrupt bool // flip the first uploaded byte
}

func startSSHServer(t *testing.T) *sshTestServer {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
		if c.User() == "root" && string(pw) == sshTestPassword {
			return nil, nil
		}
		return nil, errors.Error("denied")
	}}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &sshTestServer{addr: ln.Addr().String(), hostKey: signer.PublicKey()}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&s.conns, 1)
			go s.serve(nc, cfg)
		}
	}()
	return s
}

func (s *sshTestServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, creqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range creqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var p struct{ Command string }
				ssh.Unmarshal(req.Payload, &p)
				req.Reply(true, nil)
				s.lock.Lock()
				s.execs = append(s.execs, p.Command)
				s.lock.Unlock()
				cmd := exec.Command("/bin/sh", "-c", p.Command)
				var in io.Reader = ch
				if s.corrupt && strings.HasPrefix(p.Command, "set -C; cat > ") {
					in = &flipFirst{r: ch}
				}
				cmd.Stdin, cmd.Stdout, cmd.Stderr = in, ch, ch.Stderr()
				code := 0
				if err := cmd.Run(); err != nil {
					code = 1
					if ee, ok := err.(*exec.ExitError); ok {
						code = ee.ExitCode()
					}
				}
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
				return
			}
		}()
	}
}

type flipFirst struct {
	r    io.Reader
	done bool
}

func (f *flipFirst) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 && !f.done {
		p[0] ^= 0xff
		f.done = true
	}
	return n, err
}

func dirEntries(t *testing.T, dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ret := []string{}
	for _, e := range ents {
		ret = append(ret, e.Name())
	}
	return ret
}

func (s *sshTestServer) opts(local, dir string) *SSSHUploadOptions {
	return &SSSHUploadOptions{Addr: s.addr, User: "root", Password: sshTestPassword,
		HostKeySHA256: ssh.FingerprintSHA256(s.hostKey), LocalPath: local, RemoteDir: dir, RemoteName: "cloud.qcow2"}
}

func TestSSHUploadPinnedKey(t *testing.T) {
	s := startSSHServer(t)
	content := bytes.Repeat([]byte("qcow2-data-"), 100000)
	local, dir := writeLocal(t, content), t.TempDir()
	res, err := uploadFileSSH(s.opts(local, dir))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "cloud.qcow2"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("remote content differs (%v)", err)
	}
	sum, size, _ := localFileSHA256(local)
	if res.SHA256 != sum || res.Size != size {
		t.Fatalf("res = %+v, want %s/%d", res, sum, size)
	}
	if e := dirEntries(t, dir); len(e) != 1 {
		t.Fatalf("leftovers: %v", e)
	}
}

func TestSSHUploadKnownHosts(t *testing.T) {
	s := startSSHServer(t)
	local, dir := writeLocal(t, []byte("abc")), t.TempDir()
	kh := filepath.Join(t.TempDir(), "known_hosts")
	os.WriteFile(kh, []byte(knownhosts.Line([]string{s.addr}, s.hostKey)+"\n"), 0600)
	o := s.opts(local, dir)
	o.HostKeySHA256, o.KnownHostsFile = "", kh
	if _, err := uploadFileSSH(o); err != nil {
		t.Fatal(err)
	}

	// a known_hosts entry for another key is refused before any command
	other := startSSHServer(t)
	os.WriteFile(kh, []byte(knownhosts.Line([]string{other.addr}, s.hostKey)+"\n"), 0600)
	o2 := other.opts(local, t.TempDir())
	o2.HostKeySHA256, o2.KnownHostsFile = "", kh
	if _, err := uploadFileSSH(o2); err == nil || !strings.Contains(err.Error(), "key mismatch") {
		t.Fatalf("err = %v, want key mismatch", err)
	}
	if len(other.execs) != 0 {
		t.Fatalf("commands ran despite host key mismatch: %v", other.execs)
	}
}

func TestSSHUploadPinnedKeyMismatch(t *testing.T) {
	s := startSSHServer(t)
	o := s.opts(writeLocal(t, []byte("abc")), t.TempDir())
	o.HostKeySHA256 = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := uploadFileSSH(o); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("err = %v", err)
	}
	if len(s.execs) != 0 {
		t.Fatalf("execs = %v", s.execs)
	}
}

func TestSSHUploadHostKeyPolicyRequired(t *testing.T) {
	s := startSSHServer(t)
	o := s.opts(writeLocal(t, []byte("abc")), t.TempDir())
	o.HostKeySHA256 = ""
	if _, err := uploadFileSSH(o); errors.Cause(err) != cloudprovider.ErrInputParameter {
		t.Fatalf("none: %v", err)
	}
	o.HostKeySHA256, o.InsecureIgnoreHostKey = ssh.FingerprintSHA256(s.hostKey), true
	if _, err := uploadFileSSH(o); errors.Cause(err) != cloudprovider.ErrInputParameter {
		t.Fatalf("two: %v", err)
	}
	if atomic.LoadInt32(&s.conns) != 0 {
		t.Fatal("connected without a valid host key policy")
	}
	// explicit opt-out works
	o.HostKeySHA256 = ""
	if _, err := uploadFileSSH(o); err != nil {
		t.Fatalf("insecure opt-out: %v", err)
	}
}

func TestSSHUploadRefusesOverwrite(t *testing.T) {
	s := startSSHServer(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "cloud.qcow2"), []byte("ORIGINAL"), 0600)
	_, err := uploadFileSSH(s.opts(writeLocal(t, []byte("new")), dir))
	if errors.Cause(err) != cloudprovider.ErrDuplicateId {
		t.Fatalf("err = %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "cloud.qcow2"))
	if string(got) != "ORIGINAL" {
		t.Fatalf("existing file changed: %q", got)
	}
}

func TestSSHUploadChecksumMismatchCleansUp(t *testing.T) {
	s := startSSHServer(t)
	s.corrupt = true
	dir := t.TempDir()
	_, err := uploadFileSSH(s.opts(writeLocal(t, []byte("qcow2 bytes")), dir))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if e := dirEntries(t, dir); len(e) != 0 {
		t.Fatalf("leftovers after mismatch: %v", e)
	}
}

func TestSSHUploadSecretsNotInErrors(t *testing.T) {
	s := startSSHServer(t)
	o := s.opts(writeLocal(t, []byte("abc")), t.TempDir())
	o.Password = "Wrong-" + sshTestPassword
	_, err := uploadFileSSH(o)
	if err == nil {
		t.Fatal("want auth error")
	}
	if strings.Contains(err.Error(), o.Password) || strings.Contains(o.String(), o.Password) {
		t.Fatal("password leaked into error/String")
	}
}

func impSSHFake(t *testing.T, dir string) *impFake {
	f := newImpFake(t)
	f.poolPath = dir
	f.srcHidden = true // CAS lists the file only after refresh
	f.srcOnDisk = func() bool {
		_, err := os.Stat(filepath.Join(dir, impSource))
		return err == nil
	}
	return f
}

func TestImportQcow2WithSSHUpload(t *testing.T) {
	s := startSSHServer(t)
	dir := t.TempDir()
	f := impSSHFake(t, dir)
	o := impOpts()
	up := s.opts(writeLocal(t, []byte("cloud image")), "")
	up.Addr = s.addr
	o.Stager = NewSSHStager(*up)
	res, err := f.client(t).ImportQcow2AsTemplate(o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps[0].Step != "refresh-pool-before-upload" || res.Steps[1].Step != "ssh-upload" || res.Steps[2].Step != "refresh-pool" || res.Steps[3].Step != "create-overlay" {
		t.Fatalf("steps = %+v", res.Steps)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "cloud.qcow2")); string(got) != "cloud image" {
		t.Fatalf("uploaded = %q", got)
	}
}

func TestImportQcow2SSHDryRunAndReadOnlyNoConnection(t *testing.T) {
	s := startSSHServer(t)
	dir := t.TempDir()
	for _, mode := range []string{"dry-run", "read-only"} {
		f := impSSHFake(t, dir)
		cli := f.client(t)
		o := impOpts()
		o.Stager = NewSSHStager(*s.opts(writeLocal(t, []byte("x")), ""))
		if mode == "dry-run" {
			o.DryRun = true
		} else {
			cli.cpcfg.ReadOnly = true
		}
		res, err := cli.ImportQcow2AsTemplate(o)
		if mode == "dry-run" && (err != nil || res.Steps[1].Step != "ssh-upload" || res.Steps[1].Done || res.Steps[0].Done) {
			t.Fatalf("%s: %v %+v", mode, err, res)
		}
		if mode == "read-only" && errors.Cause(err) != cloudprovider.ErrAccountReadOnly {
			t.Fatalf("%s: %v", mode, err)
		}
		if w := f.writes(); len(w) != 0 {
			t.Fatalf("%s: writes %v", mode, w)
		}
	}
	if n := atomic.LoadInt32(&s.conns); n != 0 {
		t.Fatalf("ssh connections = %d, want 0", n)
	}
	if e := dirEntries(t, dir); len(e) != 0 {
		t.Fatalf("files written: %v", e)
	}
}

// Skipping host-key checks is reported as a warning in the plan (and
// so in the import result); verified modes give none. Plan never connects.
func TestSSHStagerInsecureWarning(t *testing.T) {
	s := startSSHServer(t)
	local := writeLocal(t, []byte("img"))
	target := SImportTarget{HostIp: "127.0.0.1", PoolPath: t.TempDir(), FileName: "cloud.qcow2"}

	up := s.opts(local, "")
	plan, err := NewSSHStager(*up).Plan(target)
	if err != nil || len(plan.Warnings) != 0 || strings.Contains(plan.Detail, "NOT VERIFIED") {
		t.Fatalf("pinned: %v %+v", err, plan)
	}
	up.HostKeySHA256 = ""
	up.InsecureIgnoreHostKey = true
	plan, err = NewSSHStager(*up).Plan(target)
	if err != nil || len(plan.Warnings) != 1 || plan.Warnings[0] != SSHInsecureHostKeyWarning || !strings.Contains(plan.Detail, "HOST KEY NOT VERIFIED") {
		t.Fatalf("insecure: %v %+v", err, plan)
	}
	if n := atomic.LoadInt32(&s.conns); n != 0 {
		t.Fatalf("Plan connected %d times", n)
	}

	f := impSSHFake(t, target.PoolPath)
	o := impOpts()
	o.DryRun = true
	up.Addr = s.addr
	o.Stager = NewSSHStager(*up)
	res, err := f.client(t).ImportQcow2AsTemplate(o)
	if err != nil || len(res.Warnings) == 0 || res.Warnings[0] != SSHInsecureHostKeyWarning {
		t.Fatalf("import result warnings: %v %+v", err, res)
	}
}

// The bytes streamed must match the hash shown in the plan.
func TestSSHUploadLocalFileChanged(t *testing.T) {
	s := startSSHServer(t)
	dir := t.TempDir()
	o := s.opts(writeLocal(t, []byte("now different")), dir)
	o.ExpectSHA256 = strings.Repeat("0", 64)
	_, err := uploadFileSSH(o)
	if err == nil || !strings.Contains(err.Error(), "changed while uploading") {
		t.Fatalf("err = %v", err)
	}
	if e := dirEntries(t, dir); len(e) != 0 {
		t.Fatalf("leftovers: %v", e)
	}
}

func TestSSHStagerStageNeedsPlan(t *testing.T) {
	s := startSSHServer(t)
	_, err := NewSSHStager(*s.opts(writeLocal(t, []byte("x")), "")).Stage(SImportTarget{HostIp: "127.0.0.1", PoolPath: t.TempDir(), FileName: "a.qcow2"})
	if err == nil || !strings.Contains(err.Error(), "before Plan") {
		t.Fatalf("err = %v", err)
	}
}
