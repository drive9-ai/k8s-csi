package driver

import (
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountPathPolicyNormalizesLists(t *testing.T) {
	got, err := effectiveMountPathPolicy(map[string]string{
		paramLocalOnlyPatterns:  " **/.cache/** \r\n\n**/tmp/**\n**/.cache/**\n--foreground ",
		paramRemoteOnlyPatterns: " **/tmp/**\n**/.tmp/**\n**/tmp/** ",
	})
	if err != nil {
		t.Fatalf("effectiveMountPathPolicy error = %v", err)
	}
	want := mountPathPolicy{
		LocalOnlyPatterns:  []string{"**/.cache/**", "**/tmp/**", "--foreground"},
		RemoteOnlyPatterns: []string{"**/tmp/**", "**/.tmp/**"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("effectiveMountPathPolicy = %#v, want %#v", got, want)
	}
}

func TestEffectiveMountPathPolicyAbsentOrEmpty(t *testing.T) {
	for _, values := range []map[string]string{
		nil,
		{},
		{paramLocalOnlyPatterns: " \n\t", paramRemoteOnlyPatterns: ""},
	} {
		got, err := effectiveMountPathPolicy(values)
		if err != nil {
			t.Fatalf("effectiveMountPathPolicy(%q) error = %v", values, err)
		}
		if len(got.LocalOnlyPatterns) != 0 || len(got.RemoteOnlyPatterns) != 0 {
			t.Fatalf("effectiveMountPathPolicy(%q) = %#v, want empty", values, got)
		}
	}
}

func TestEffectiveMountPathPolicyRejectsInvalidPatterns(t *testing.T) {
	tests := []struct {
		name  string
		param string
		raw   string
		line  string
	}{
		{name: "invalid UTF-8", param: paramLocalOnlyPatterns, raw: "ok\n" + string([]byte{0xff}), line: "line 2"},
		{name: "NUL", param: paramLocalOnlyPatterns, raw: "**/bad\x00/**", line: "line 1"},
		{name: "backslash", param: paramRemoteOnlyPatterns, raw: `**\\tmp\\**`, line: "line 1"},
		{name: "control", param: paramRemoteOnlyPatterns, raw: "**/bad\x01/**", line: "line 1"},
		{name: "dot", param: paramLocalOnlyPatterns, raw: "**/./bad/**", line: "line 1"},
		{name: "dotdot", param: paramRemoteOnlyPatterns, raw: "**/../bad/**", line: "line 1"},
		{name: "credential-like", param: paramLocalOnlyPatterns, raw: "**/drive9_api_key_secret/**", line: "line 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := effectiveMountPathPolicy(map[string]string{tt.param: tt.raw})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
			if !strings.Contains(err.Error(), tt.param) || !strings.Contains(err.Error(), tt.line) {
				t.Fatalf("error = %q, want parameter %q and %q", err, tt.param, tt.line)
			}
		})
	}
}

func TestEffectiveMountPathPolicyAllowsCredentialShapedPathPatterns(t *testing.T) {
	want := []string{
		"token=metadata",
		"--password value",
		"authorization: docs/**",
		"server=production",
	}
	got, err := effectiveMountPathPolicy(map[string]string{
		paramLocalOnlyPatterns: strings.Join(want, "\n"),
	})
	if err != nil {
		t.Fatalf("effectiveMountPathPolicy error = %v", err)
	}
	if !reflect.DeepEqual(got.LocalOnlyPatterns, want) {
		t.Fatalf("local-only patterns = %q, want %q", got.LocalOnlyPatterns, want)
	}
}

func TestEffectiveMountPathPolicyRedactsRejectedPattern(t *testing.T) {
	const secretPattern = "token=super-secret-value/../bad"
	_, err := effectiveMountPathPolicy(map[string]string{
		paramRemoteOnlyPatterns: "allowed\n" + secretPattern,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
	}
	for _, secret := range []string{secretPattern, "super-secret-value"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked rejected pattern: %q", err)
		}
	}
	for _, want := range []string{paramRemoteOnlyPatterns, "line 2", `pattern contains ".." segment`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestMountPathPolicyAddToVolumeContextUsesCanonicalNewlines(t *testing.T) {
	ctx := map[string]string{"preserved": "value"}
	mountPathPolicy{
		LocalOnlyPatterns:  []string{"**/.cache/**", "**/tmp/**"},
		RemoteOnlyPatterns: []string{"**/tmp/**", "**/.tmp/**"},
	}.addToVolumeContext(ctx)

	if got, want := ctx[paramLocalOnlyPatterns], "**/.cache/**\n**/tmp/**"; got != want {
		t.Fatalf("localOnlyPatterns = %q, want %q", got, want)
	}
	if got, want := ctx[paramRemoteOnlyPatterns], "**/tmp/**\n**/.tmp/**"; got != want {
		t.Fatalf("remoteOnlyPatterns = %q, want %q", got, want)
	}
	if ctx["preserved"] != "value" {
		t.Fatal("unrelated context value changed")
	}
}

func TestMountPathPolicyAddToVolumeContextOmitsEmptyLists(t *testing.T) {
	ctx := map[string]string{}
	mountPathPolicy{}.addToVolumeContext(ctx)
	if _, ok := ctx[paramLocalOnlyPatterns]; ok {
		t.Fatal("empty localOnlyPatterns was persisted")
	}
	if _, ok := ctx[paramRemoteOnlyPatterns]; ok {
		t.Fatal("empty remoteOnlyPatterns was persisted")
	}
}

func TestEffectiveMountPathPolicyUsesWholeValueMutablePrecedence(t *testing.T) {
	merged, err := effectiveCreateMountParameters(
		map[string]string{
			paramLocalOnlyPatterns:  "legacy-local",
			paramRemoteOnlyPatterns: "legacy-remote",
		},
		map[string]string{
			paramLocalOnlyPatterns:  "vac-local",
			paramRemoteOnlyPatterns: "",
		},
	)
	if err != nil {
		t.Fatalf("effectiveCreateMountParameters error = %v", err)
	}
	got, err := effectiveMountPathPolicy(merged)
	if err != nil {
		t.Fatalf("effectiveMountPathPolicy error = %v", err)
	}
	want := mountPathPolicy{LocalOnlyPatterns: []string{"vac-local"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %#v, want %#v", got, want)
	}
}
