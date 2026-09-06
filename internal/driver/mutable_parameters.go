package driver

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramProfile    = "profile"
	paramDurability = "durability"
)

var supportedMutableMountParameters = map[string]struct{}{
	paramProfile:                     {},
	paramDurability:                  {},
	paramAttrTTL:                     {},
	paramEntryTTL:                    {},
	paramDirTTL:                      {},
	paramPerfEnabled:                 {},
	paramGVisorCompat:                {},
	paramLocalOnlyPatterns:           {},
	paramRemoteOnlyPatterns:          {},
	paramAppendLogPatterns:           {},
	paramReaddirPrefetch:             {},
	paramReaddirPrefetchMaxFiles:     {},
	paramReaddirPrefetchMaxFileBytes: {},
	paramReaddirPrefetchMaxBytes:     {},
	paramWritebackBatchWindow:        {},
}

func effectiveCreateMountParameters(params, mutable map[string]string) (map[string]string, error) {
	if err := validateMutableMountParameterValues(mutable); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(params)+len(mutable))
	for k, v := range params {
		out[k] = v
	}
	for k, v := range mutable {
		out[k] = v
	}
	return out, nil
}

func validateMutableMountParameterValues(values map[string]string) error {
	if err := validateMutableMountParameters(values); err != nil {
		return err
	}
	if _, err := effectiveMountTTLs(values); err != nil {
		return err
	}
	if _, err := effectiveMountPerf(values); err != nil {
		return err
	}
	if _, err := effectiveMountGVisor(values); err != nil {
		return err
	}
	if _, err := effectiveMountPathPolicy(values); err != nil {
		return err
	}
	if _, err := effectiveMountTuning(values); err != nil {
		return err
	}
	return nil
}

func validateMutableMountParameters(values map[string]string) error {
	if err := validateNoAPIKeyInAttributes(values); err != nil {
		return status.Errorf(codes.InvalidArgument, "%v", err)
	}
	for key := range values {
		if _, ok := supportedMutableMountParameters[key]; ok {
			continue
		}
		return status.Errorf(codes.InvalidArgument, "mutable parameter %q is not supported", key)
	}
	return nil
}

func profileFromParameters(values map[string]string) string {
	return strings.TrimSpace(values[paramProfile])
}

func durabilityFromParameters(values map[string]string) string {
	return strings.TrimSpace(values[paramDurability])
}

func addDurabilityToVolumeContext(ctx map[string]string, durability string) {
	if durability != "" {
		ctx[paramDurability] = durability
	}
}
