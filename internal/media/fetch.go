package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
)

// stallTimeout bounds how long a download may deliver no bytes.
var stallTimeout = 60 * time.Second

// maxDownloadBytes stops a bad mirror filling the disk before the checksum can
// reject it. Every pin is under 1 GiB. A var so tests can shrink it.
var maxDownloadBytes int64 = 4 * utils.BytesPerGiB

// stallResetReader pushes the watchdog forward on every successful read.
type stallResetReader struct {
	io.Reader
	watchdog *time.Timer
}

func (r stallResetReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.watchdog.Reset(stallTimeout)
	}
	return n, err
}

// CachePath is where pinned downloads live.
const CachePath = steps.StateDirPath + "/cache"

// CacheDir is CachePath under root.
func CacheDir(root string) string { return filepath.Join(root, CachePath) }

// Fetch returns the cached path for d, downloading and pin-verifying when absent.
func Fetch(root string, d artifacts.Download, out io.Writer) (string, error) {
	dest := filepath.Join(CacheDir(root), d.File)
	if _, err := os.Stat(dest); err == nil {
		sum, err := utils.FileSHA256(dest)
		if err != nil {
			return "", err
		}
		if sum != d.SHA256 {
			return "", fmt.Errorf("%s: cached %s has SHA256 %s, pinned %s — delete the file and re-run", d.Name, dest, sum, d.SHA256)
		}
		fmt.Fprintf(out, "%s %s: using cached %s\n", d.Name, d.Version, dest)
		return dest, nil
	}
	if err := os.MkdirAll(CacheDir(root), 0o755); err != nil {
		return "", err
	}

	fmt.Fprintf(out, "fetching %s %s from %s\n", d.Name, d.Version, d.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchdog := time.AfterFunc(stallTimeout, cancel)
	defer watchdog.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return "", fmt.Errorf("%s: %w", d.Name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: %w", d.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: GET %s: %s", d.Name, d.URL, resp.Status)
	}

	part := dest + ".part"
	sum, err := hashCopy(part, stallResetReader{resp.Body, watchdog})
	if ctx.Err() != nil {
		err = fmt.Errorf("no data for %v — connection stalled", stallTimeout)
	}
	if err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("%s: download: %w", d.Name, err)
	}
	if sum != d.SHA256 {
		_ = os.Remove(part)
		return "", fmt.Errorf("%s: downloaded file has SHA256 %s, pinned %s — refusing to use it", d.Name, sum, d.SHA256)
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "verified %s (SHA256 %s)\n", d.File, d.SHA256)
	return dest, nil
}

// ImportInstaller copies a user-supplied installer into the cache under d's stable filename.
func ImportInstaller(root string, d artifacts.Download, src string, out io.Writer) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("%s: %w", d.Name, err)
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(CacheDir(root), 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(CacheDir(root), d.File)
	part := dest + ".part"
	sum, err := hashCopy(part, in)
	if err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("%s: %w", d.Name, err)
	}
	if err := os.Rename(part, dest); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "%s: using user-supplied %s (SHA256 %s — not pin-verified)\n", d.Name, src, sum)
	return dest, nil
}

// hashCopy streams r into a new file at dest, hashing in the same pass.
func hashCopy(dest string, r io.Reader) (string, error) {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	// One byte past the cap: io.EOF at the cap would read as a complete body,
	// and the checksum would blame the mirror for our own truncation.
	n, err := io.CopyN(io.MultiWriter(f, h), r, maxDownloadBytes+1)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if err == nil && n > maxDownloadBytes {
		err = fmt.Errorf("body is larger than the %d-byte download limit", maxDownloadBytes)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
