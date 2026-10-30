package pacelever

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// defaultNodeEnv is the panewire node env file, relative to the home directory.
const defaultNodeEnv = ".config/panewire/hub-node.env"

// nodeEnvMaxBytes bounds the node env read; a larger file is refused, not
// truncated, so a cut-off assignment can never leave a stale id in use.
const nodeEnvMaxBytes = 1 << 20

// Presence refusals. The messages carry no id, path or file content.
var (
	errNoNodeID     = errors.New("presence not published: no node id (HUB_MACHINE_ID in the node env file)")
	errHostMismatch = errors.New("presence not published: configured host id does not match the node id")
)

// nodeIDRE is the host id alphabet; the hub's token map and hk grants use it.
var nodeIDRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// nodeEnvPath is the node env file: quota.node_env, else the panewire default.
// A leading "~/" in the configured value names the home directory.
func (q QuotaConfig) nodeEnvPath() string {
	home, _ := os.UserHomeDir()
	path := strings.TrimSpace(q.NodeEnv)
	switch {
	case path == "":
		if home == "" {
			return ""
		}
		return filepath.Join(home, defaultNodeEnv)
	case strings.HasPrefix(path, "~/") && home != "":
		return filepath.Join(home, path[2:])
	}
	return path
}

// nodeID reads HUB_MACHINE_ID from the node env file. It reads the file
// directly: no panewire binary and no hub round trip. Only that one value
// leaves the parser; the token in the same file is discarded.
func (q QuotaConfig) nodeID() (string, bool) {
	path := q.nodeEnvPath()
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > nodeEnvMaxBytes {
		return "", false
	}
	id := ""
	scanner := bufio.NewScanner(io.LimitReader(f, nodeEnvMaxBytes))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return "", false
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			if value, err = strconv.Unquote(value); err != nil {
				return "", false
			}
		}
		if key == "HUB_MACHINE_ID" {
			id = value
		}
	}
	if scanner.Err() != nil || !nodeIDRE.MatchString(id) {
		return "", false
	}
	return id, true
}

// presenceHostID is the id this host publishes presence under: the node's
// HUB_MACHINE_ID. An explicit PACELEVER_HOST_ID / HANDOFFKEEP_HOST_ID or
// quota.host_id is never an alternative source; each one that is set must equal
// the node id, or publishing is refused. With no node id nothing is published.
// The error is safe to print.
func (q QuotaConfig) presenceHostID() (string, error) {
	id, ok := q.nodeID()
	if !ok {
		return "", errNoNodeID
	}
	for _, explicit := range []string{
		strings.TrimSpace(FirstEnv("PACELEVER_HOST_ID")),
		strings.TrimSpace(FirstEnv("HANDOFFKEEP_HOST_ID")),
		strings.TrimSpace(q.HostID),
	} {
		if explicit != "" && explicit != id {
			return "", errHostMismatch
		}
	}
	return id, nil
}
