package driver

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountGVisor(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want mountGVisor
	}{
		{name: "missing defaults false"},
		{
			name: "true",
			in:   map[string]string{paramGVisorCompat: "true"},
			want: mountGVisor{Enabled: true},
		},
		{
			name: "false",
			in:   map[string]string{paramGVisorCompat: "false"},
		},
		{
			name: "trimmed true",
			in:   map[string]string{paramGVisorCompat: " true "},
			want: mountGVisor{Enabled: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveMountGVisor(tt.in)
			if err != nil {
				t.Fatalf("effectiveMountGVisor error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("effectiveMountGVisor = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEffectiveMountGVisorRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "yes", "1", "TRUE"} {
		_, err := effectiveMountGVisor(map[string]string{paramGVisorCompat: value})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("effectiveMountGVisor(%q) status = %s, want InvalidArgument (err=%v)",
				value, status.Code(err), err)
		}
	}
}

func TestMountGVisorAddToVolumeContextPersistsCanonicalValue(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		ctx := map[string]string{}
		mountGVisor{Enabled: enabled}.addToVolumeContext(ctx)
		want := "false"
		if enabled {
			want = "true"
		}
		if got := ctx[paramGVisorCompat]; got != want {
			t.Fatalf("gvisorCompat = %q, want %q", got, want)
		}
	}
}
