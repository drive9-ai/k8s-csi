package driver

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramProfile        = "profile"
	profileNone         = "none"
	paramDurability     = "durability"
	durabilityCloseSync = "close-sync"
	durabilityWriteSync = "write-sync"
)

var supportedMutableMountParameters = map[string]struct{}{
	paramProfile:                     {},
	paramDurability:                  {},
	paramAttrTTL:                     {},
	paramEntryTTL:                    {},
	paramDirTTL:                      {},
	paramPerfEnabled:                 {},
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
	durability, err := effectiveMountDurability(values)
	if err != nil {
		return err
	}
	tuning, err := effectiveMountTuning(values)
	if err != nil {
		return err
	}
	return validateDurabilityTuning(durability, tuning)
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

func effectiveMountDurability(values map[string]string) (string, error) {
	raw, ok := values[paramDurability]
	if !ok {
		return "", nil
	}
	value := strings.TrimSpace(raw)
	if value != durabilityCloseSync && value != durabilityWriteSync {
		return "", status.Errorf(codes.InvalidArgument, "%s must be %s or %s",
			paramDurability, durabilityCloseSync, durabilityWriteSync)
	}
	return value, nil
}

func validateDurabilityTuning(durability string, tuning mountTuning) error {
	if durability != "" && tuning.WritebackBatchWindow != "" {
		return status.Errorf(codes.InvalidArgument, "%s cannot be combined with %s",
			paramDurability, paramWritebackBatchWindow)
	}
	return nil
}

func validateMNMWMountParameters(profile string, durability string) error {
	if profile != profileNone {
		return status.Errorf(codes.InvalidArgument, "%s must be %s for MULTI_NODE_MULTI_WRITER",
			paramProfile, profileNone)
	}
	if durability != durabilityCloseSync && durability != durabilityWriteSync {
		return status.Errorf(codes.InvalidArgument, "%s must be %s or %s for MULTI_NODE_MULTI_WRITER",
			paramDurability, durabilityCloseSync, durabilityWriteSync)
	}
	return nil
}

func addDurabilityToVolumeContext(ctx map[string]string, durability string) {
	if durability != "" {
		ctx[paramDurability] = durability
	}
}
