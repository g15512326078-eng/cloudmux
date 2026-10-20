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

// Optional mode (b) of the image import: copy a local file into a CVK
// host's pool directory over SSH. It uses golang.org/x/crypto/ssh exec
// sessions (no SFTP subsystem, so no new module dependency):
//
//  1. test that the target does not exist and the directory does
//  2. "set -C; cat > <tmp>" with the file on stdin (noclobber)
//  3. "sha256sum <tmp>" must match the sha256 computed while sending
//  4. "mv -n <tmp> <final>" and check <tmp> is gone (never overwrites)
//
// On any failure after step 2 started only <tmp> is removed. The password
// and key are never logged or included in errors. A host key must be
// verified against a known_hosts file or a pinned SHA256 fingerprint,
// unless InsecureIgnoreHostKey is set explicitly.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
)

const DEFAULT_SSH_TIMEOUT = 30 * time.Second

type SSSHUploadOptions struct {
	// Addr is host:port; the importer defaults it to <host ip>:22.
	Addr string
	User string
	// Password and PrivateKey (PEM) come from flags or env only.
	Password   string `json:"-"`
	PrivateKey []byte `json:"-"`
	// KnownHostsFile or HostKeySHA256 ("SHA256:..." as printed by
	// ssh-keygen -lf) verifies the host key. InsecureIgnoreHostKey skips
	// the check and must be set explicitly.
	KnownHostsFile        string
	HostKeySHA256         string
	InsecureIgnoreHostKey bool

	LocalPath string

	// RemoteDir/RemoteName are set by the importer from the pool path.
	RemoteDir  string
	RemoteName string

	Timeout time.Duration
}

// String never includes the password or key.
func (o *SSSHUploadOptions) String() string {
	return fmt.Sprintf("ssh %s@%s %s -> %s/%s", o.User, o.Addr, o.LocalPath, o.RemoteDir, o.RemoteName)
}

func (o *SSSHUploadOptions) validate() error {
	if len(o.Addr) == 0 || len(o.User) == 0 || len(o.LocalPath) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "ssh address, user and local file")
	}
	if len(o.Password) == 0 && len(o.PrivateKey) == 0 {
		return errors.Wrap(cloudprovider.ErrMissingParameter, "ssh password or private key")
	}
	n := 0
	if len(o.KnownHostsFile) > 0 {
		n++
	}
	if len(o.HostKeySHA256) > 0 {
		n++
	}
	if o.InsecureIgnoreHostKey {
		n++
	}
	if n != 1 {
		return errors.Wrap(cloudprovider.ErrInputParameter, "set exactly one of known_hosts file, pinned host key SHA256, or the explicit insecure opt-out")
	}
	if len(o.HostKeySHA256) > 0 && !strings.HasPrefix(o.HostKeySHA256, "SHA256:") {
		return errors.Wrap(cloudprovider.ErrInputParameter, "host key fingerprint must look like SHA256:...")
	}
	if !path.IsAbs(o.RemoteDir) || strings.ContainsAny(o.RemoteDir, "'\"\\$`\n;&|<>*?") {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "remote dir %q", o.RemoteDir)
	}
	if !importNameRe.MatchString(o.RemoteName) {
		return errors.Wrapf(cloudprovider.ErrInputParameter, "remote file name %q", o.RemoteName)
	}
	return nil
}

func (o *SSSHUploadOptions) hostKeyCallback() (ssh.HostKeyCallback, error) {
	switch {
	case len(o.KnownHostsFile) > 0:
		cb, err := knownhosts.New(o.KnownHostsFile)
		if err != nil {
			return nil, errors.Wrapf(err, "known_hosts %s", o.KnownHostsFile)
		}
		return cb, nil
	case len(o.HostKeySHA256) > 0:
		want := o.HostKeySHA256
		return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != want {
				return errors.Errorf("host key of %s is %s, pinned %s", hostname, got, want)
			}
			return nil
		}, nil
	case o.InsecureIgnoreHostKey:
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return nil, errors.Wrap(cloudprovider.ErrInputParameter, "no host key verification configured")
}

func (o *SSSHUploadOptions) clientConfig() (*ssh.ClientConfig, error) {
	cb, err := o.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	auth := []ssh.AuthMethod{}
	if len(o.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(o.PrivateKey)
		if err != nil {
			// the parse error never contains key material
			return nil, errors.Wrap(err, "parse ssh private key")
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(o.Password) > 0 {
		auth = append(auth, ssh.Password(o.Password))
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DEFAULT_SSH_TIMEOUT
	}
	return &ssh.ClientConfig{User: o.User, Auth: auth, HostKeyCallback: cb, Timeout: timeout}, nil
}

// shellQuote single-quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func localFileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, errors.Wrapf(err, "open %s", p)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, errors.Wrapf(err, "read %s", p)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type SSSHUploadResult struct {
	RemotePath string
	Size       int64
	SHA256     string
}

func runSSH(client *ssh.Client, cmd string, stdin io.Reader) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", errors.Wrap(err, "ssh session")
	}
	defer sess.Close()
	var stdout, stderr bytes.Buffer
	sess.Stdout, sess.Stderr = &stdout, &stderr
	if stdin != nil {
		sess.Stdin = stdin
	}
	if err := sess.Run(cmd); err != nil {
		return stdout.String(), errors.Wrapf(err, "remote command failed: %s", strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// uploadFileSSH copies o.LocalPath to RemoteDir/RemoteName. Callers must
// have checked the account is writable; the importer does.
func uploadFileSSH(o *SSSHUploadOptions) (*SSSHUploadResult, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	cfg, err := o.clientConfig()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(o.LocalPath)
	if err != nil {
		return nil, errors.Wrapf(err, "open %s", o.LocalPath)
	}
	defer f.Close()

	client, err := ssh.Dial("tcp", o.Addr, cfg)
	if err != nil {
		return nil, errors.Wrapf(err, "ssh dial %s@%s", o.User, o.Addr)
	}
	defer client.Close()

	final := path.Join(o.RemoteDir, o.RemoteName)
	rnd := make([]byte, 6)
	if _, err := rand.Read(rnd); err != nil {
		return nil, err
	}
	tmp := path.Join(o.RemoteDir, fmt.Sprintf(".%s.cmx-upload-%s", o.RemoteName, hex.EncodeToString(rnd)))
	qf, qt, qd := shellQuote(final), shellQuote(tmp), shellQuote(o.RemoteDir)

	if _, err := runSSH(client, fmt.Sprintf("test -d %s && test ! -e %s && test ! -e %s", qd, qf, qt), nil); err != nil {
		return nil, errors.Wrapf(cloudprovider.ErrDuplicateId, "%s already exists or %s is not a directory on the host (%v)", final, o.RemoteDir, err)
	}
	cleanup := func(cause error) error {
		if _, err := runSSH(client, fmt.Sprintf("rm -f -- %s", qt), nil); err != nil {
			return errors.NewAggregate([]error{cause, errors.Wrapf(err, "remove partial upload %s", tmp)})
		}
		return cause
	}

	h := sha256.New()
	counter := &countWriter{}
	if _, err := runSSH(client, fmt.Sprintf("set -C; cat > %s", qt), io.TeeReader(f, io.MultiWriter(h, counter))); err != nil {
		return nil, cleanup(errors.Wrap(err, "upload"))
	}
	local := hex.EncodeToString(h.Sum(nil))
	out, err := runSSH(client, fmt.Sprintf("sha256sum -b -- %s", qt), nil)
	if err != nil {
		return nil, cleanup(errors.Wrap(err, "remote sha256sum"))
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != local {
		return nil, cleanup(errors.Errorf("sha256 mismatch: local %s, remote %q", local, strings.TrimSpace(out)))
	}
	if _, err := runSSH(client, fmt.Sprintf("mv -n -- %s %s && test ! -e %s", qt, qf, qt), nil); err != nil {
		return nil, cleanup(errors.Wrapf(err, "move into place (%s may have appeared meanwhile)", final))
	}
	return &SSSHUploadResult{RemotePath: final, Size: counter.n, SHA256: local}, nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}
