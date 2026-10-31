package chromium

import (
	"encoding/json"
	"testing"
)

type seccompRule struct {
	Names  []string
	Action string
	Args   []struct {
		Index    int
		Value    uint64
		ValueTwo uint64
		Op       string
	}
	Includes struct{ Caps []string }
}

// TestSeccompAllowsOnlySandboxNamespaces fails if the profile lets a container
// without CAP_SYS_ADMIN create any namespace other than user, PID and network.
func TestSeccompAllowsOnlySandboxNamespaces(t *testing.T) {
	var p struct {
		DefaultAction string
		Syscalls      []seccompRule
	}
	if err := json.Unmarshal(SeccompProfile, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Fatalf("defaultAction = %s, want SCMP_ACT_ERRNO", p.DefaultAction)
	}

	const (
		newNS, newCgroup, newUTS, newIPC = 0x00020000, 0x02000000, 0x04000000, 0x08000000
		newTime                          = 0x00000080
		forbidden                        = newNS | newCgroup | newUTS | newIPC
	)
	allowed := map[string]bool{}
	for _, r := range p.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" || len(r.Includes.Caps) > 0 {
			continue
		}
		for _, name := range r.Names {
			if name != "clone" && name != "unshare" && name != "clone3" && name != "setns" {
				continue
			}
			if name == "clone3" || name == "setns" {
				t.Errorf("%s allowed without CAP_SYS_ADMIN", name)
				continue
			}
			want := uint64(forbidden)
			if name == "unshare" {
				want |= newTime
			}
			if len(r.Args) != 1 || r.Args[0].Op != "SCMP_CMP_MASKED_EQ" || r.Args[0].ValueTwo != 0 || r.Args[0].Value&want != want {
				t.Errorf("%s allowed without masking out mount, cgroup, UTS and IPC namespaces: %+v", name, r.Args)
				continue
			}
			allowed[name] = true
		}
	}
	if !allowed["clone"] || !allowed["unshare"] {
		t.Errorf("Chromium's sandbox needs clone and unshare; allowed: %v", allowed)
	}
}
