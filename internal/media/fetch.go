package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/steps"
)

// stallTimeout is how long a download may deliver no bytes before it is
// aborted; var so tests can shrink it.
var stallTimeout = 60 * time.Second

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

// CachePath is where pinned downloads live; plain undo keeps it, undo --purge
// removes it with the rest of the state dir. hostcfg's lg-build template
// bakes the same path in.
const CachePath = steps.StateDirPath + "/cache"

// CacheDir is CachePath under root (the test seam).
func CacheDir(root string) string { return filepath.Join(root, CachePath) }

// Fetch returns the cached path for d, downloading and pin-verifying when
// absent. Any SHA256 mismatch — cached file or fresh download — is a hard
// fail; a corrupted cache entry is never silently re-fetched.
func Fetch(root string, d artifacts.Download, out io.Writer) (string, error) {
	dest := filepath.Join(CacheDir(root), d.File)
	if _, err := os.Stat(dest); err == nil {
		sum, err := fileSHA256(dest)
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
	// downloads are multi-GB, so no overall client timeout; instead a
	// watchdog cancels the request when no bytes arrive for a while, so a
	// stalled connection fails instead of hanging `media`/`up` forever
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// arm the watchdog before the request so DNS/connect/TLS/response-header
	// stalls are covered too, not just the body read — a server that accepts
	// the connection but never sends headers would otherwise hang forever.
	// Reads push it forward, so only a genuine stall (no bytes for
	// stallTimeout) cancels.
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

// ImportInstaller copies a user-supplied installer into the cache under d's
// stable filename — the last-resort path when the pinned download does not
// cover the hardware. No pin to verify; the hash is printed for the record.
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
	sum, err := hashCopy(dest, in)
	if err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("%s: %w", d.Name, err)
	}
	fmt.Fprintf(out, "%s: using user-supplied %s (SHA256 %s — not pin-verified)\n", d.Name, src, sum)
	return dest, nil
}

// hashCopy streams r into a new file at dest, hashing in the same pass — the
// payloads run hundreds of MB to multi-GB. Returns the SHA256 hex digest.
// Callers remove dest on error.
func hashCopy(dest string, r io.Reader) (string, error) {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
