package docker

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// HostDataDir returns dataDir (an absolute path as this process sees it) as
// the Docker daemon sees it, so it can be bind-mounted into browser
// containers. Outside a container they're the same path. Inside one, it is
// read from this container's own mounts; override, when set, must agree.
func (c *Client) HostDataDir(ctx context.Context, dataDir, override string) (string, error) {
	id, inContainer := ownContainer()
	if !inContainer {
		if override != "" {
			return filepath.Clean(override), nil
		}
		return dataDir, nil
	}
	if id == "" {
		id, _ = os.Hostname()
	}
	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		if override != "" {
			return filepath.Clean(override), nil
		}
		return "", fmt.Errorf("running in a container but can't find it through Docker (%w); set BROWSERPIER_HOST_DATA_DIR to the data dir's path on the host", err)
	}
	detected, ok := hostPath(res.Container.Mounts, dataDir)
	if !ok {
		return "", fmt.Errorf("the data dir %s isn't on a volume or bind mount, so profiles would be lost with this container; mount one there", dataDir)
	}
	if override != "" && filepath.Clean(override) != detected {
		return "", fmt.Errorf("BROWSERPIER_HOST_DATA_DIR is %s but this container's %s is mounted from %s", override, dataDir, detected)
	}
	return detected, nil
}

// hostPath maps path through the mount that contains it, if any.
func hostPath(mounts []container.MountPoint, path string) (string, bool) {
	var best *container.MountPoint
	for i := range mounts {
		m := &mounts[i]
		if m.Type != mount.TypeBind && m.Type != mount.TypeVolume {
			continue
		}
		if m.Source == "" || !within(path, m.Destination) {
			continue
		}
		if best == nil || len(m.Destination) > len(best.Destination) {
			best = m
		}
	}
	if best == nil {
		return "", false
	}
	rel, _ := filepath.Rel(best.Destination, path)
	return filepath.Join(best.Source, rel), true
}

func within(path, dir string) bool {
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, strings.TrimSuffix(dir, "/")+"/")
}

// ownContainer reports whether this process runs in a container and, if it
// can tell, that container's ID.
func ownContainer() (id string, inContainer bool) {
	if f, err := os.Open("/proc/self/mountinfo"); err == nil {
		id = containerIDFromMountinfo(f)
		_ = f.Close()
	}
	if id != "" {
		return id, true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return "", true
		}
	}
	return "", false
}

// The runtime bind-mounts a per-container hostname file over /etc/hostname,
// from a path that holds the container ID: Docker uses
// .../containers/<id>/hostname and Podman .../<id>/userdata/hostname.
var hostnameSourceRE = regexp.MustCompile(`/([0-9a-f]{64})/(?:userdata/)?hostname$`)

func containerIDFromMountinfo(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		// Fields: id, parent, major:minor, root, mount point, ...
		f := strings.Fields(sc.Text())
		if len(f) < 5 || f[4] != "/etc/hostname" {
			continue
		}
		if m := hostnameSourceRE.FindStringSubmatch(f[3]); m != nil {
			return m[1]
		}
	}
	return ""
}
