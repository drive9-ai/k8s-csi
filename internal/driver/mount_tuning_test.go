package driver

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountTuning(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want mountTuning
	}{
		{
			name: "missing leaves all flags unset",
			in:   nil,
			want: mountTuning{},
		},
		{
			name: "configured",
			in: map[string]string{
				paramReaddirPrefetch:             "true",
				paramReaddirPrefetchMaxFiles:     "064",
				paramReaddirPrefetchMaxFileBytes: "50000",
				paramReaddirPrefetchMaxBytes:     "4194304",
				paramWritebackBatchWindow:        "20ms",
			},
			want: mountTuning{
				ReaddirPrefetchGiven:        true,
				ReaddirPrefetch:             true,
				ReaddirPrefetchMaxFiles:     "64",
				ReaddirPrefetchMaxFileBytes: "50000",
				ReaddirPrefetchMaxBytes:     "4194304",
				WritebackBatchWindow:        "20ms",
			},
		},
		{
			name: "explicit false",
			in: map[string]string{
				paramReaddirPrefetch: " false ",
			},
			want: mountTuning{
				ReaddirPrefetchGiven: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveMountTuning(tt.in)
			if err != nil {
				t.Fatalf("effectiveMountTuning error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("effectiveMountTuning = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEffectiveMountTuningRejectsInvalidValues(t *testing.T) {
	tests := []map[string]string{
		{paramReaddirPrefetch: ""},
		{paramReaddirPrefetch: "yes"},
		{paramReaddirPrefetchMaxFiles: ""},
		{paramReaddirPrefetchMaxFiles: "0"},
		{paramReaddirPrefetchMaxFiles: "-1"},
		{paramReaddirPrefetchMaxFiles: "abc"},
		{paramReaddirPrefetchMaxFileBytes: "0"},
		{paramReaddirPrefetchMaxBytes: "0"},
		{paramWritebackBatchWindow: ""},
		{paramWritebackBatchWindow: "0s"},
		{paramWritebackBatchWindow: "-1ms"},
		{paramWritebackBatchWindow: "soon"},
	}

	for _, values := range tests {
		_, err := effectiveMountTuning(values)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("effectiveMountTuning(%v) status = %s, want InvalidArgument (err=%v)", values, status.Code(err), err)
		}
	}
}
