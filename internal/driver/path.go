package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
)

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func buildRemoteRoot(prefix string, volumeName string) (string, error) {
	prefix, err := normalizeRemotePath(prefix)
	if err != nil {
		return "", err
	}
	if prefix == "/" {
		return "", errors.New("remoteRootPrefix must not be /")
	}
	base := safeFileName(volumeName)
	if base == "" {
		base = "volume"
	}
	sum := sha256.Sum256([]byte(volumeName))
	suffix := hex.EncodeToString(sum[:])[:12]
	root := path.Join(prefix, base+"-"+suffix)
	return normalizeRemotePath(root)
}

func normalizeRemotePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is empty")
	}
	if strings.HasPrefix(raw, ":") {
		raw = strings.TrimPrefix(raw, ":")
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	if strings.Contains(raw, "\x00") {
		return "", errors.New("path contains NUL")
	}
	for _, part := range strings.Split(strings.Trim(raw, "/"), "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("unsafe path segment %q", part)
		}
	}
	clean := path.Clean(raw)
	if clean == "." {
		clean = "/"
	}
	if clean != raw && strings.HasSuffix(raw, "/") && clean != "/" {
		raw = clean
	} else {
		raw = clean
	}
	for _, part := range strings.Split(strings.Trim(raw, "/"), "/") {
		if part == "" {
			continue
		}
		if part == "." || part == ".." {
			return "", fmt.Errorf("unsafe path segment %q", part)
		}
	}
	return raw, nil
}

func validateRemoteRoot(raw string) error {
	_, err := normalizeRemotePath(raw)
	return err
}

func safeFileName(name string) string {
	name = strings.TrimSpace(name)
	name = unsafeNameChars.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if len(name) > 80 {
		name = name[:80]
		name = strings.Trim(name, "-.")
	}
	return name
}

func encodeDrive9FSPath(remotePath string) (string, error) {
	normalized, err := normalizeRemotePath(remotePath)
	if err != nil {
		return "", err
	}
	if normalized == "/" {
		return "/v1/fs/", nil
	}
	parts := strings.Split(strings.TrimPrefix(normalized, "/"), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return "/v1/fs/" + strings.Join(parts, "/"), nil
}
