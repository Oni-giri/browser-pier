package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestNoToolTouchesRecordings: no MCP tool can read, list, or turn on or
// off session recordings (PROJ-23). An agent that could turn recording off
// would hide what it did.
func TestNoToolTouchesRecordings(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(f.all, versions[0], nil)
	list, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		raw, _ := json.Marshal(tl)
		if s := strings.ToLower(string(raw)); strings.Contains(s, "record") {
			t.Errorf("tool %s mentions recording: %s", tl.Name, raw)
		}
	}
}
