package driver

import (
	"fmt"
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
		paramAppendLogPatterns:  " data/app.db-wal \r\n\nlogs/events.log\ndata/app.db-wal\n--foreground ",
	})
	if err != nil {
		t.Fatalf("effectiveMountPathPolicy error = %v", err)
	}
	want := mountPathPolicy{
		LocalOnlyPatterns:  []string{"**/.cache/**", "**/tmp/**", "--foreground"},
		RemoteOnlyPatterns: []string{"**/tmp/**", "**/.tmp/**"},
		AppendLogPatterns:  []string{"data/app.db-wal", "logs/events.log", "--foreground"},
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
		{paramAppendLogPatterns: " \r\n\t"},
	} {
		got, err := effectiveMountPathPolicy(values)
		if err != nil {
			t.Fatalf("effectiveMountPathPolicy(%q) error = %v", values, err)
		}
		if len(got.LocalOnlyPatterns) != 0 || len(got.RemoteOnlyPatterns) != 0 || len(got.AppendLogPatterns) != 0 {
			t.Fatalf("effectiveMountPathPolicy(%q) = %#v, want empty", values, got)
		}
	}
}

func TestEffectiveMountPathPolicyRejectsNonOverlayProfile(t *testing.T) {
	for _, profile := range []string{"none", "interactive"} {
		t.Run(profile, func(t *testing.T) {
			_, err := effectiveMountPathPolicy(map[string]string{
				paramProfile:            profile,
				paramRemoteOnlyPatterns: "**/tmp/**",
				paramAppendLogPatterns:  "**/tmp/**",
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
			if !strings.Contains(err.Error(), "requires an overlay profile") {
				t.Fatalf("error = %q, want overlay profile requirement", err)
			}
		})
	}
}

func TestEffectiveMountPathPolicyRejectsOversizedPolicy(t *testing.T) {
	shortPatterns := make([]string, 5_000)
	for index := range shortPatterns {
		shortPatterns[index] = fmt.Sprintf("p%d", index)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "raw value", raw: strings.Repeat("a", 1<<20)},
		{name: "expanded arguments", raw: strings.Join(shortPatterns, "\n")},
		{name: "serialized state", raw: strings.Repeat("<", 50<<10)},
	}
	for _, parameter := range []string{paramLocalOnlyPatterns, paramAppendLogPatterns} {
		for _, test := range tests {
			t.Run(parameter+"/"+test.name, func(t *testing.T) {
				_, err := effectiveMountPathPolicy(map[string]string{
					parameter: test.raw,
				})
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
				}
				if !strings.Contains(err.Error(), "mount path policy is too large") {
					t.Fatalf("error = %q, want policy size error", err)
				}
			})
		}
	}
}

func TestEffectiveMountPathPolicyCombinedAppendLogBudgets(t *testing.T) {
	shortPatterns := make([]string, 1500)
	for i := range shortPatterns {
		shortPatterns[i] = fmt.Sprintf("p%d", i)
	}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "raw", raw: strings.Repeat("a", 22<<10)},
		{name: "argv", raw: strings.Join(shortPatterns, "\n")},
		{name: "JSON", raw: strings.Repeat("<", 15<<10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			combined := map[string]string{}
			for _, key := range []string{paramLocalOnlyPatterns, paramRemoteOnlyPatterns, paramAppendLogPatterns} {
				if _, err := effectiveMountPathPolicy(map[string]string{key: test.raw}); err != nil {
					t.Fatalf("individual %s policy must fit its budget: %v", key, err)
				}
				combined[key] = test.raw
			}
			if _, err := effectiveMountPathPolicy(combined); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("combined policy error = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestEffectiveMountPathPolicyAppendLogProfilesAndArgumentBoundary(t *testing.T) {
	for _, profile := range []string{"", "none", "interactive", "coding-agent", "portable", "custom"} {
		t.Run(profile, func(t *testing.T) {
			values := map[string]string{
				paramProfile:           profile,
				paramAppendLogPatterns: strings.Repeat("a", maxMountPathPolicyArgumentBytes-len("--append-log=")-1),
			}
			if _, err := effectiveMountPathPolicy(values); err != nil {
				t.Fatalf("policy at argv limit: %v", err)
			}
			values[paramAppendLogPatterns] += "a"
			if _, err := effectiveMountPathPolicy(values); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("policy above argv limit: %v, want InvalidArgument", err)
			}
		})
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
		for _, parameter := range []string{tt.param, paramAppendLogPatterns} {
			t.Run(parameter+"/"+tt.name, func(t *testing.T) {
				_, err := effectiveMountPathPolicy(map[string]string{parameter: tt.raw})
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
				}
				if !strings.Contains(err.Error(), parameter) || !strings.Contains(err.Error(), tt.line) {
					t.Fatalf("error = %q, want parameter %q and %q", err, parameter, tt.line)
				}
			})
		}
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
		paramAppendLogPatterns: strings.Join(want, "\n"),
	})
	if err != nil {
		t.Fatalf("effectiveMountPathPolicy error = %v", err)
	}
	if !reflect.DeepEqual(got.LocalOnlyPatterns, want) {
		t.Fatalf("local-only patterns = %q, want %q", got.LocalOnlyPatterns, want)
	}
	if !reflect.DeepEqual(got.AppendLogPatterns, want) {
		t.Fatalf("append-log patterns = %q, want %q", got.AppendLogPatterns, want)
	}
}

func TestEffectiveMountPathPolicyRedactsRejectedPattern(t *testing.T) {
	const secretPattern = "token=super-secret-value/../bad"
	for _, parameter := range []string{paramRemoteOnlyPatterns, paramAppendLogPatterns} {
		_, err := effectiveMountPathPolicy(map[string]string{
			parameter: "allowed\n" + secretPattern,
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
		}
		for _, secret := range []string{secretPattern, "super-secret-value"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked rejected pattern: %q", err)
			}
		}
		for _, want := range []string{parameter, "line 2", `pattern contains ".." segment`} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want %q", err, want)
			}
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
	if _, ok := ctx[paramAppendLogPatterns]; ok {
		t.Fatal("empty appendLogPatterns was persisted")
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
