package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The build scripts set these with -ldflags -X: Version from the release
// tag, MinPeerVersion from scripts/min-version.
var (
	Version        = "0.0.0-dev"
	MinPeerVersion = "0.0.0"
)

// versionFeature names this app and its version in hello. tetherd and the
// iPhone app ignore features they do not know.
const versionFeature = "tether-desktop/"

// peerTooOld tells whether a peer's version is below MinPeerVersion. The
// minimum 0.0.0 means none, so that builds without a release version
// (0.0.0-dev) still work with each other.
func peerTooOld(version string) bool {
	return CompareVersions(MinPeerVersion, "0.0.0") > 0 && CompareVersions(version, MinPeerVersion) < 0
}

func helloFeatures() []string {
	return append(append([]string{}, features...), versionFeature+Version)
}

// peerVersion returns the Tether Desktop version in a peer's hello, or
// false for a peer that names none (tetherd, the iPhone app).
func peerVersion(features []string) (string, bool) {
	for _, f := range features {
		if v, ok := strings.CutPrefix(f, versionFeature); ok {
			return v, true
		}
	}
	return "", false
}

// CompareVersions compares versions such as "0.3.0", "v0.3.1" and
// "0.4.0-rc.1": -1, 0 or 1. A version with a suffix comes before the same
// version without one. Parts that are not numbers count as 0.
func CompareVersions(a, b string) int {
	na, pa := splitVersion(a)
	nb, pb := splitVersion(b)
	for i := range 3 {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	case pa < pb:
		return -1
	}
	return 1
}

func splitVersion(v string) ([3]int, string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, pre, _ := strings.Cut(v, "-")
	var n [3]int
	for i, part := range strings.SplitN(v, ".", 3) {
		n[i], _ = strconv.Atoi(part)
	}
	return n, pre
}

// UpdateInfo is update.json, which each release publishes: the newest
// version, and the oldest one that may still run.
type UpdateInfo struct {
	Latest       string `json:"latest"`
	MinSupported string `json:"min_supported"`
}

// UpdateState is what an app of version current should do.
type UpdateState int

const (
	UpToDate UpdateState = iota
	UpdateAvailable
	UpdateRequired
)

func (u UpdateInfo) State(current string) UpdateState {
	switch {
	case u.MinSupported != "" && CompareVersions(current, u.MinSupported) < 0:
		return UpdateRequired
	case u.Latest != "" && CompareVersions(current, u.Latest) < 0:
		return UpdateAvailable
	}
	return UpToDate
}

// FetchUpdateInfo reads update.json from url.
func FetchUpdateInfo(ctx context.Context, url string) (UpdateInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return UpdateInfo{}, err
	}
	req.Header.Set("User-Agent", "TetherDesktop/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return UpdateInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UpdateInfo{}, fmt.Errorf("update check: %s", resp.Status)
	}
	var info UpdateInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return UpdateInfo{}, fmt.Errorf("update check: %w", err)
	}
	return info, nil
}
