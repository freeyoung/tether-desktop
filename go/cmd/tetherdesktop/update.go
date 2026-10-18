package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/freeyoung/tether-core-go/core"
)

// appVersion is the version of Tether Desktop, which the build scripts set
// from the release tag with -ldflags -X. The menu shows it, and the update
// check compares it with update.json. The engine names its own version in
// hello (core.EngineVersion).
var appVersion = "0.0.0-dev"

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
	case u.MinSupported != "" && core.CompareVersions(current, u.MinSupported) < 0:
		return UpdateRequired
	case u.Latest != "" && core.CompareVersions(current, u.Latest) < 0:
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
	req.Header.Set("User-Agent", "TetherDesktop/"+appVersion)
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
