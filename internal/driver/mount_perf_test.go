package driver

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountPerf(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want mountPerf
	}{
		{
			name: "missing defaults false",
			in:   nil,
			want: mountPerf{},
		},
		{
			name: "true",
			in:   map[string]string{paramPerfEnabled: "true"},
			want: mountPerf{Enabled: true},
		},
		{
			name: "false",
			in:   map[string]string{paramPerfEnabled: "false"},
			want: mountPerf{},
		},
		{
			name: "trimmed true",
			in:   map[string]string{paramPerfEnabled: " true "},
			want: mountPerf{Enabled: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveMountPerf(tt.in)
			if err != nil {
				t.Fatalf("effectiveMountPerf error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("effectiveMountPerf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEffectiveMountPerfRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "yes", "1", "TRUE"} {
		_, err := effectiveMountPerf(map[string]string{paramPerfEnabled: value})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("effectiveMountPerf(%q) status = %s, want InvalidArgument (err=%v)", value, status.Code(err), err)
		}
	}
}
