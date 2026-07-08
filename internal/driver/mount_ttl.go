package driver

import (
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramAttrTTL  = "attrTTL"
	paramEntryTTL = "entryTTL"
	paramDirTTL   = "dirTTL"

	defaultMountTTL = 30 * time.Second
)

type mountTTLs struct {
	AttrTTL  string
	EntryTTL string
	DirTTL   string
}

func effectiveMountTTLs(values map[string]string) (mountTTLs, error) {
	attrTTL, err := effectiveMountTTLValue(values, paramAttrTTL)
	if err != nil {
		return mountTTLs{}, err
	}
	entryTTL, err := effectiveMountTTLValue(values, paramEntryTTL)
	if err != nil {
		return mountTTLs{}, err
	}
	dirTTL, err := effectiveMountTTLValue(values, paramDirTTL)
	if err != nil {
		return mountTTLs{}, err
	}
	return mountTTLs{
		AttrTTL:  attrTTL,
		EntryTTL: entryTTL,
		DirTTL:   dirTTL,
	}, nil
}

func effectiveMountTTLValue(values map[string]string, key string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return defaultMountTTL.String(), nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive duration", key)
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%s: invalid duration %q", key, raw)
	}
	if ttl <= 0 {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive duration", key)
	}
	return ttl.String(), nil
}

func mountTTLsOrDefault(attrTTL string, entryTTL string, dirTTL string) mountTTLs {
	return mountTTLs{
		AttrTTL:  mountTTLOrDefault(attrTTL),
		EntryTTL: mountTTLOrDefault(entryTTL),
		DirTTL:   mountTTLOrDefault(dirTTL),
	}
}

func mountTTLOrDefault(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultMountTTL.String()
	}
	return value
}

func (m mountTTLs) addToVolumeContext(ctx map[string]string) {
	ctx[paramAttrTTL] = m.AttrTTL
	ctx[paramEntryTTL] = m.EntryTTL
	ctx[paramDirTTL] = m.DirTTL
}
