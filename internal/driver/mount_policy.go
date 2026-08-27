package driver

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramLocalOnlyPatterns  = "localOnlyPatterns"
	paramRemoteOnlyPatterns = "remoteOnlyPatterns"
)

type mountPathPolicy struct {
	LocalOnlyPatterns  []string
	RemoteOnlyPatterns []string
}

func effectiveMountPathPolicy(values map[string]string) (mountPathPolicy, error) {
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
	return mountPathPolicy{
		LocalOnlyPatterns:  local,
		RemoteOnlyPatterns: remote,
	}, nil
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
				"%s line %d: invalid path pattern %q: %v",
				parameter, index+1, pattern, err)
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
}
