package driver

import (
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const paramPerfEnabled = "perfEnabled"

type mountPerf struct {
	Enabled bool
}

func effectiveMountPerf(values map[string]string) (mountPerf, error) {
	enabled, err := effectiveMountPerfEnabled(values)
	if err != nil {
		return mountPerf{}, err
	}
	return mountPerf{Enabled: enabled}, nil
}

func effectiveMountPerfEnabled(values map[string]string) (bool, error) {
	raw, ok := values[paramPerfEnabled]
	if !ok {
		return false, nil
	}
	switch strings.TrimSpace(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, status.Errorf(codes.InvalidArgument, "%s must be true or false", paramPerfEnabled)
	}
}

func (m mountPerf) addToVolumeContext(ctx map[string]string) {
	if m.Enabled {
		ctx[paramPerfEnabled] = "true"
		return
	}
	ctx[paramPerfEnabled] = "false"
}

func (d *Driver) drive9PerfDir(volumeID string) string {
	return filepath.Join(d.cfg.StateDir, "perf", safeFileName(volumeID))
}
