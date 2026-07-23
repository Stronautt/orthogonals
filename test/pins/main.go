// Command pins checks that every pinned download in internal/artifacts is
// still reachable, so a rotted vendor URL surfaces here rather than in a user's
// `orthogonals media` run. It deliberately does not verify SHA256: the pinned
// driver alone is hundreds of megabytes, and the release job already fetches
// and checks the artifacts it ships.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/stronautt/orthogonals/internal/artifacts"
)

func main() {
	client := &http.Client{Timeout: 60 * time.Second}
	failed := 0
	for _, d := range artifacts.Downloads() {
		status, err := reach(client, d.URL)
		switch {
		case err != nil:
			fmt.Printf("FAIL %-24s %s\n       %v\n", d.Name, d.URL, err)
			failed++
		case status >= 400:
			fmt.Printf("FAIL %-24s %s\n       HTTP %d\n", d.Name, d.URL, status)
			failed++
		default:
			fmt.Printf("ok   %-24s %s (%s, HTTP %d)\n", d.Name, d.Version, d.File, status)
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d pinned download(s) unreachable\n", failed)
		os.Exit(1)
	}
	fmt.Printf("\nall %d pinned downloads reachable\n", len(artifacts.Downloads()))
}

// reach probes a URL with HEAD, falling back to a single-byte ranged GET for
// the hosts that refuse HEAD.
func reach(client *http.Client, url string) (int, error) {
	resp, err := client.Head(url)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 400 {
			return resp.StatusCode, nil
		}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	get, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = get.Body.Close() }()
	return get.StatusCode, nil
}
