package driver

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramLocalOnlyPatterns          = "localOnlyPatterns"
	paramRemoteOnlyPatterns         = "remoteOnlyPatterns"
	paramAppendLogPatterns          = "appendLogPatterns"
	maxMountPathPolicyArgumentBytes = 64 << 10
	maxMountPathPolicyStateBytes    = maxMountStateLength / 4
)

type mountPathPolicy struct {
	LocalOnlyPatterns  []string
	RemoteOnlyPatterns []string
	AppendLogPatterns  []string
}

func effectiveMountPathPolicy(values map[string]string) (mountPathPolicy, error) {
	if len(values[paramLocalOnlyPatterns])+len(values[paramRemoteOnlyPatterns])+len(values[paramAppendLogPatterns]) > maxMountPathPolicyArgumentBytes {
		return mountPathPolicy{}, status.Error(codes.InvalidArgument, "mount path policy is too large")
	}
	local, err := normalizeMountPolicyParameter(
		paramLocalOnlyPatterns,
		"--local-only=",
		values[paramLocalOnlyPatterns],
	)
	if err != nil {
		return mountPathPolicy{}, err
	}
	remote, err := normalizeMountPolicyParameter(
		paramRemoteOnlyPatterns,
		"--remote-only=",
		values[paramRemoteOnlyPatterns],
	)
	if err != nil {
		return mountPathPolicy{}, err
	}
	appendLog, err := normalizeMountPolicyParameter(
		paramAppendLogPatterns,
		"--append-log=",
		values[paramAppendLogPatterns],
	)
	if err != nil {
		return mountPathPolicy{}, err
	}
	policy := mountPathPolicy{
		LocalOnlyPatterns:  local,
		RemoteOnlyPatterns: remote,
		AppendLogPatterns:  appendLog,
	}
	if err := validateMountPathPolicyContract(profileFromParameters(values), policy); err != nil {
		return mountPathPolicy{}, err
	}
	return policy, nil
}

func validateMountPathPolicyContract(profile string, policy mountPathPolicy) error {
	routingPatterns := len(policy.LocalOnlyPatterns) + len(policy.RemoteOnlyPatterns)
	if routingPatterns == 0 && len(policy.AppendLogPatterns) == 0 {
		return nil
	}
	if routingPatterns > 0 && (profile == "none" || profile == "interactive") {
		return status.Errorf(codes.InvalidArgument, "mount path policy requires an overlay profile; profile %q is not supported", profile)
	}

	args := make([]string, 0, routingPatterns+len(policy.AppendLogPatterns))
	encodedLength := 0
	for _, pattern := range policy.LocalOnlyPatterns {
		arg := "--local-only=" + pattern
		args = append(args, arg)
		encodedLength += len(arg) + 1
	}
	for _, pattern := range policy.RemoteOnlyPatterns {
		arg := "--remote-only=" + pattern
		args = append(args, arg)
		encodedLength += len(arg) + 1
	}
	for _, pattern := range policy.AppendLogPatterns {
		arg := "--append-log=" + pattern
		args = append(args, arg)
		encodedLength += len(arg) + 1
	}
	if encodedLength > maxMountPathPolicyArgumentBytes {
		return status.Error(codes.InvalidArgument, "mount path policy is too large")
	}
	serialized, err := json.Marshal(args)
	if err != nil || len(serialized) > maxMountPathPolicyStateBytes {
		return status.Error(codes.InvalidArgument, "mount path policy is too large")
	}
	return nil
}

func normalizeMountPolicyParameter(parameter string, flagPrefix string, raw string) ([]string, error) {
	var patterns []string
	seen := make(map[string]struct{})
	for index, line := range strings.Split(raw, "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" {
			continue
		}
		if err := validateMountPolicyPattern(flagPrefix, pattern); err != nil {
			return nil, status.Errorf(codes.InvalidArgument,
				"%s line %d: invalid path pattern: %v",
				parameter, index+1, err)
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

func validateMountPolicyPattern(flagPrefix string, pattern string) error {
	if !utf8.ValidString(pattern) {
		return fmt.Errorf("pattern contains invalid UTF-8")
	}
	for index := 0; index < len(pattern); index++ {
		value := pattern[index]
		switch {
		case value == 0:
			return fmt.Errorf("pattern contains NUL character")
		case value == '\n' || value == '\t' || value == '\r':
			continue
		case value >= 0x01 && value <= 0x1f:
			return fmt.Errorf("pattern contains control character 0x%02x", value)
		}
	}
	if strings.ContainsRune(pattern, '\\') {
		return fmt.Errorf("pattern contains backslash")
	}
	for _, segment := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("pattern contains %q segment", segment)
		}
	}
	if argumentMayContainCredential(flagPrefix + pattern) {
		return fmt.Errorf("pattern may contain a credential")
	}
	return nil
}

func (m mountPathPolicy) addToVolumeContext(ctx map[string]string) {
	if len(m.LocalOnlyPatterns) > 0 {
		ctx[paramLocalOnlyPatterns] = strings.Join(m.LocalOnlyPatterns, "\n")
	}
	if len(m.RemoteOnlyPatterns) > 0 {
		ctx[paramRemoteOnlyPatterns] = strings.Join(m.RemoteOnlyPatterns, "\n")
	}
	if len(m.AppendLogPatterns) > 0 {
		ctx[paramAppendLogPatterns] = strings.Join(m.AppendLogPatterns, "\n")
	}
}
