// Package memusage reads how much memory a container uses, as BrowserPier's
// admission counts it. It is apart from internal/docker so the browser
// image's own tests, which internal/docker imports, can read the same
// number (PROJ-160).
package memusage

import (
	"context"
	"encoding/json"
	"io"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Stats is the one Docker call Read makes: a client's ContainerStats.
// Read is handed that method, never the client, so the client's other
// calls stay in internal/docker, whose set of Docker operations is closed
// and tested (TestDockerOperationsAreAClosedSet).
type Stats func(ctx context.Context, id string, opts client.ContainerStatsOptions) (client.ContainerStatsResult, error)

// Read is how much memory the container uses now, counted as docker stats
// does: usage less the page cache the kernel can drop.
func Read(ctx context.Context, stats Stats, id string) (int64, error) {
	res, err := stats(ctx, id, client.ContainerStatsOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = res.Body.Close() }()
	var s container.StatsResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&s); err != nil {
		return 0, err
	}
	return InUse(s.MemoryStats), nil
}

// InUse is usage less the inactive page cache.
func InUse(m container.MemoryStats) int64 {
	used := m.Usage
	// cgroup v2 calls it inactive_file, v1 total_inactive_file.
	for _, k := range []string{"inactive_file", "total_inactive_file"} {
		if v, ok := m.Stats[k]; ok && v < used {
			return int64(used - v) //nolint:gosec // v < used
		}
	}
	return int64(used) //nolint:gosec // memory sizes fit
}
