package memusage

import (
	"testing"

	"github.com/moby/moby/api/types/container"
)

func TestInUse(t *testing.T) {
	if got := InUse(container.MemoryStats{Usage: 1000, Stats: map[string]uint64{"inactive_file": 300}}); got != 700 {
		t.Errorf("cgroup v2: %d", got)
	}
	if got := InUse(container.MemoryStats{Usage: 1000, Stats: map[string]uint64{"total_inactive_file": 100}}); got != 900 {
		t.Errorf("cgroup v1: %d", got)
	}
	if got := InUse(container.MemoryStats{Usage: 1000}); got != 1000 {
		t.Errorf("no stats: %d", got)
	}
}
