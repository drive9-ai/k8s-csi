package driver

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const paramGVisorCompat = "gvisorCompat"

type mountGVisor struct {
	Enabled bool
}

func effectiveMountGVisor(values map[string]string) (mountGVisor, error) {
	raw, ok := values[paramGVisorCompat]
	if !ok {
		return mountGVisor{}, nil
	}
	switch strings.TrimSpace(raw) {
	case "true":
		return mountGVisor{Enabled: true}, nil
	case "false":
		return mountGVisor{}, nil
	default:
		return mountGVisor{}, status.Errorf(codes.InvalidArgument,
			"%s must be true or false", paramGVisorCompat)
	}
}

func (m mountGVisor) addToVolumeContext(ctx map[string]string) {
	if m.Enabled {
		ctx[paramGVisorCompat] = "true"
		return
	}
	ctx[paramGVisorCompat] = "false"
}
