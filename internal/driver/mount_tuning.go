package driver

import (
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	paramReaddirPrefetch             = "readdirPrefetch"
	paramReaddirPrefetchMaxFiles     = "readdirPrefetchMaxFiles"
	paramReaddirPrefetchMaxFileBytes = "readdirPrefetchMaxFileBytes"
	paramReaddirPrefetchMaxBytes     = "readdirPrefetchMaxBytes"
	paramWritebackBatchWindow        = "writebackBatchWindow"
)

type mountTuning struct {
	ReaddirPrefetchGiven        bool
	ReaddirPrefetch             bool
	ReaddirPrefetchMaxFiles     string
	ReaddirPrefetchMaxFileBytes string
	ReaddirPrefetchMaxBytes     string
	WritebackBatchWindow        string
}

func effectiveMountTuning(values map[string]string) (mountTuning, error) {
	readdirPrefetch, readdirPrefetchGiven, err := optionalBoolParam(values, paramReaddirPrefetch)
	if err != nil {
		return mountTuning{}, err
	}
	maxFiles, err := optionalPositiveIntParam(values, paramReaddirPrefetchMaxFiles)
	if err != nil {
		return mountTuning{}, err
	}
	maxFileBytes, err := optionalPositiveIntParam(values, paramReaddirPrefetchMaxFileBytes)
	if err != nil {
		return mountTuning{}, err
	}
	maxBytes, err := optionalPositiveIntParam(values, paramReaddirPrefetchMaxBytes)
	if err != nil {
		return mountTuning{}, err
	}
	writebackBatchWindow, err := optionalPositiveDurationParam(values, paramWritebackBatchWindow)
	if err != nil {
		return mountTuning{}, err
	}
	return mountTuning{
		ReaddirPrefetchGiven:        readdirPrefetchGiven,
		ReaddirPrefetch:             readdirPrefetch,
		ReaddirPrefetchMaxFiles:     maxFiles,
		ReaddirPrefetchMaxFileBytes: maxFileBytes,
		ReaddirPrefetchMaxBytes:     maxBytes,
		WritebackBatchWindow:        writebackBatchWindow,
	}, nil
}

func optionalBoolParam(values map[string]string, key string) (bool, bool, error) {
	raw, ok := values[key]
	if !ok {
		return false, false, nil
	}
	switch strings.TrimSpace(raw) {
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	default:
		return false, false, status.Errorf(codes.InvalidArgument, "%s must be true or false", key)
	}
}

func optionalPositiveIntParam(values map[string]string, key string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return "", nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive integer", key)
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%s: invalid integer %q", key, raw)
	}
	if value <= 0 {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive integer", key)
	}
	return strconv.FormatInt(value, 10), nil
}

func optionalPositiveDurationParam(values map[string]string, key string) (string, error) {
	raw, ok := values[key]
	if !ok {
		return "", nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive duration", key)
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "%s: invalid duration %q", key, raw)
	}
	if value <= 0 {
		return "", status.Errorf(codes.InvalidArgument, "%s must be a positive duration", key)
	}
	return value.String(), nil
}

func (m mountTuning) addToVolumeContext(ctx map[string]string) {
	if m.ReaddirPrefetchGiven {
		ctx[paramReaddirPrefetch] = strconv.FormatBool(m.ReaddirPrefetch)
	}
	if m.ReaddirPrefetchMaxFiles != "" {
		ctx[paramReaddirPrefetchMaxFiles] = m.ReaddirPrefetchMaxFiles
	}
	if m.ReaddirPrefetchMaxFileBytes != "" {
		ctx[paramReaddirPrefetchMaxFileBytes] = m.ReaddirPrefetchMaxFileBytes
	}
	if m.ReaddirPrefetchMaxBytes != "" {
		ctx[paramReaddirPrefetchMaxBytes] = m.ReaddirPrefetchMaxBytes
	}
	if m.WritebackBatchWindow != "" {
		ctx[paramWritebackBatchWindow] = m.WritebackBatchWindow
	}
}
