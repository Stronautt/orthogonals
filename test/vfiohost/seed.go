package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/kdomanski/iso9660"
	"golang.org/x/crypto/ssh"
)

// The base cloud image, pinned the way every other download in this project is.
// Not in internal/artifacts: that is the bump place for what a user downloads.
const (
	baseImageURL = "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/" +
		"Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2"
	baseImageSHA  = "28680fe5b371a5a82ebf43a31926e086a168e59949d03969c5093e7071f90b7f"
	baseImageName = "Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2"
)

// userCache is where the image is kept between runs, since WorkDir is under
// /var/tmp and gets swept.
func userCache() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "orthogonals-test")
	}
	return WorkDir
}

// ensureBaseImage returns a path to the base image that qemu can read. The
// cache lives under the user's home, which is 0700, so the hypervisor can only
// open the copy staged in WorkDir.
func ensureBaseImage() (string, error) {
	cached := filepath.Join(userCache(), baseImageName)
	if _, err := os.Stat(cached); err != nil {
		if err := download(baseImageURL, cached); err != nil {
			return "", err
		}
	}
	staged := filepath.Join(WorkDir, baseImageName)
	if sameSize(cached, staged) {
		return staged, nil
	}
	logf("staging the base image into %s", WorkDir)
	if err := copyFile(cached, staged, 0o644); err != nil {
		return "", err
	}
	return staged, nil
}

// download fetches url to dest, verifying the pin. A mismatch removes the file
// rather than leaving a poisoned cache for the next run to trust.
func download(url, dest string) error {
	logf("downloading %s", url)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	resp, err := http.Get(url) //nolint:gosec // a compile-time constant URL
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, sum), resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dest)
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != baseImageSHA {
		_ = os.Remove(dest)
		return fmt.Errorf("%s has sha256 %s, want the pinned %s — the pin is stale or the mirror is lying",
			filepath.Base(dest), got, baseImageSHA)
	}
	return nil
}

func sameSize(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && sa.Size() == sb.Size()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ensureSSHKey returns the authorized-keys line for a throwaway keypair,
// generating one on first use. Throwaway on purpose: the guest is destroyed
// after every run, so CI needs no key material of its own.
func ensureSSHKey() (string, error) {
	pubPath := KeyPath + ".pub"
	if b, err := os.ReadFile(pubPath); err == nil {
		if _, err := os.Stat(KeyPath); err == nil {
			return string(bytes.TrimSpace(b)), nil
		}
	}
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "orthogonals vfio tier")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(KeyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	line := bytes.TrimSpace(ssh.MarshalAuthorizedKey(sshPub))
	if err := os.WriteFile(pubPath, append(line, '\n'), 0o644); err != nil {
		return "", err
	}
	logf("generated a throwaway ssh key at %s", KeyPath)
	return string(line), nil
}

func seedPath(name string) string { return filepath.Join(WorkDir, name+"-seed.iso") }

// writeSeedISO builds the cloud-init NoCloud seed. media.BuildISO cannot be
// reused: it labels its output ORTHOGONALS, and cloud-init only reads volumes
// labelled cidata. The names land uppercased, which the iso9660 driver maps
// back down.
func writeSeedISO(name, pubKey string) (string, error) {
	userData := "#cloud-config\n" +
		"disable_root: false\n" +
		"ssh_pwauth: false\n" +
		"users:\n" +
		"  - name: root\n" +
		"    ssh_authorized_keys:\n" +
		"      - " + pubKey + "\n"
	metaData := "instance-id: " + name + "\nlocal-hostname: " + name + "\n"

	w, err := iso9660.NewWriter()
	if err != nil {
		return "", err
	}
	defer func() { _ = w.Cleanup() }()
	for _, f := range []struct{ name, content string }{
		{"user-data", userData},
		{"meta-data", metaData},
	} {
		if err := w.AddFile(bytes.NewReader([]byte(f.content)), f.name); err != nil {
			return "", fmt.Errorf("add %s: %w", f.name, err)
		}
	}
	path := seedPath(name)
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	if err := w.WriteTo(out, "cidata"); err != nil {
		_ = out.Close()
		return "", err
	}
	return path, out.Close()
}
