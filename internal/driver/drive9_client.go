package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type drive9Credentials struct {
	Server string
	APIKey string
}

type drive9Client struct {
	server string
	apiKey string
	http   *http.Client
}

func credentialsFromSecrets(secrets map[string]string) (drive9Credentials, error) {
	server := firstNonEmpty(secrets["server"], secrets["DRIVE9_SERVER"])
	apiKey := firstNonEmpty(secrets["apiKey"], secrets["api_key"], secrets["DRIVE9_API_KEY"])
	if server == "" {
		return drive9Credentials{}, status.Error(codes.InvalidArgument, "Drive9 server secret is required")
	}
	if apiKey == "" {
		return drive9Credentials{}, status.Error(codes.InvalidArgument, "Drive9 apiKey secret is required")
	}
	if strings.ContainsRune(server, '\x00') || strings.ContainsRune(apiKey, '\x00') {
		return drive9Credentials{}, status.Error(codes.InvalidArgument, "drive9-csi: secret value contains NUL byte, cannot pass to mount process")
	}
	if trimASCIIWhitespace(server) != server || trimASCIIWhitespace(apiKey) != apiKey {
		return drive9Credentials{}, status.Error(codes.InvalidArgument, "Drive9 credential values must not have surrounding ASCII whitespace")
	}
	normalizedServer, err := normalizeDrive9ServerURL(server)
	if err != nil {
		return drive9Credentials{}, status.Errorf(codes.InvalidArgument, "Drive9 server secret: %v", err)
	}
	return drive9Credentials{Server: normalizedServer, APIKey: apiKey}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normalizeDrive9ServerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("must use http or https URL")
	}
	if u.Host == "" {
		return "", errors.New("host is required")
	}
	if u.User != nil {
		return "", errors.New("userinfo is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("query and fragment are not allowed")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func newDrive9Client(creds drive9Credentials) *drive9Client {
	return &drive9Client{
		server: creds.Server,
		apiKey: creds.APIKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *drive9Client) exists(ctx context.Context, remotePath string) (bool, error) {
	resp, err := c.request(ctx, http.MethodHead, remotePath, "", nil)
	if err != nil {
		return false, err
	}
	defer closeBody(resp.Body)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, grpcHTTPError(resp, "stat Drive9 path")
	}
}

func (c *drive9Client) mkdir(ctx context.Context, remotePath string) error {
	resp, err := c.request(ctx, http.MethodPost, remotePath, "mkdir", nil)
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 300 {
		return grpcHTTPError(resp, "mkdir Drive9 path")
	}
	return nil
}

func (c *drive9Client) mkdirAll(ctx context.Context, remotePath string) error {
	normalized, err := normalizeRemotePath(remotePath)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "Drive9 path: %v", err)
	}
	if normalized == "/" {
		return nil
	}
	err = c.mkdir(ctx, normalized)
	switch status.Code(err) {
	case codes.OK, codes.AlreadyExists:
		return nil
	case codes.NotFound:
	default:
		return err
	}

	current := "/"
	for _, part := range strings.Split(strings.Trim(normalized, "/"), "/") {
		current = path.Join(current, part)
		if err := c.mkdir(ctx, current); err != nil && status.Code(err) != codes.AlreadyExists {
			return err
		}
	}
	return nil
}

func (c *drive9Client) writeJSON(ctx context.Context, remotePath string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return status.Errorf(codes.Internal, "encode marker: %v", err)
	}
	resp, err := c.request(ctx, http.MethodPut, remotePath, "", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 300 {
		return grpcHTTPError(resp, "write Drive9 marker")
	}
	return nil
}

func (c *drive9Client) readMarker(ctx context.Context, remotePath string) (volumeMarker, error) {
	resp, err := c.request(ctx, http.MethodGet, remotePath, "", nil)
	if err != nil {
		return volumeMarker{}, err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 300 {
		return volumeMarker{}, grpcHTTPError(resp, "read Drive9 marker")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return volumeMarker{}, status.Errorf(codes.Internal, "read marker body: %v", err)
	}
	return decodeMarker(body)
}

func (c *drive9Client) validateMarker(ctx context.Context, remotePath string, want volumeMarker) error {
	got, err := c.readMarker(ctx, remotePath)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return status.Error(codes.AlreadyExists, "Drive9 path already exists without a CSI marker")
		}
		return err
	}
	if got.VolumeID != want.VolumeID || got.RemoteRoot != want.RemoteRoot || got.Driver != want.Driver || got.Name != want.Name {
		return status.Error(codes.AlreadyExists, "Drive9 path already exists and is owned by a different CSI volume")
	}
	return nil
}

func (c *drive9Client) upsertIndex(ctx context.Context, remotePath string, marker volumeMarker) error {
	exists, err := c.exists(ctx, remotePath)
	if err != nil {
		return err
	}
	if exists {
		if err := c.validateMarker(ctx, remotePath, marker); err != nil {
			return err
		}
		return nil
	}
	return c.writeJSON(ctx, remotePath, marker)
}

func (c *drive9Client) removeAll(ctx context.Context, remotePath string) error {
	resp, err := c.request(ctx, http.MethodDelete, remotePath, "recursive=1", nil)
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		return grpcHTTPError(resp, "delete Drive9 path")
	}
	return nil
}

func (c *drive9Client) request(ctx context.Context, method string, remotePath string, rawQuery string, body io.Reader) (*http.Response, error) {
	encoded, err := encodeDrive9FSPath(remotePath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Drive9 path: %v", err)
	}
	u, err := url.Parse(c.server)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "Drive9 server URL: %v", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + encoded
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create Drive9 request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "Drive9 request failed: %v", err)
	}
	return resp, nil
}

func grpcHTTPError(resp *http.Response, op string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusBadRequest:
		return status.Errorf(codes.InvalidArgument, "%s: %s", op, msg)
	case http.StatusNotFound:
		return status.Errorf(codes.NotFound, "%s: %s", op, msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return status.Errorf(codes.PermissionDenied, "%s: %s", op, msg)
	case http.StatusConflict:
		return status.Errorf(codes.AlreadyExists, "%s: %s", op, msg)
	case http.StatusTooManyRequests:
		return status.Errorf(codes.Unavailable, "%s: HTTP %d: %s", op, resp.StatusCode, msg)
	default:
		if resp.StatusCode >= 500 {
			return status.Errorf(codes.Unavailable, "%s: HTTP %d: %s", op, resp.StatusCode, msg)
		}
		return status.Errorf(codes.Internal, "%s: HTTP %d: %s", op, resp.StatusCode, msg)
	}
}

func closeBody(body io.Closer) {
	_ = body.Close()
}
