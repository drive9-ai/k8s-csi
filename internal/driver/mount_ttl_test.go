package driver

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountTTLsDefaultsAndNormalizes(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want mountTTLs
	}{
		{
			name: "default",
			in:   nil,
			want: mountTTLs{AttrTTL: "30s", EntryTTL: "30s", DirTTL: "30s"},
		},
		{
			name: "explicit",
			in: map[string]string{
				paramAttrTTL:  "1000ms",
				paramEntryTTL: "1m",
				paramDirTTL:   "2m30s",
			},
			want: mountTTLs{AttrTTL: "1s", EntryTTL: "1m0s", DirTTL: "2m30s"},
		},
		{
			name: "partial",
			in: map[string]string{
				paramEntryTTL: "5s",
			},
			want: mountTTLs{AttrTTL: "30s", EntryTTL: "5s", DirTTL: "30s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveMountTTLs(tt.in)
			if err != nil {
				t.Fatalf("effectiveMountTTLs error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("effectiveMountTTLs = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEffectiveMountTTLsRejectsInvalidValues(t *testing.T) {
	tests := []map[string]string{
		{paramAttrTTL: ""},
		{paramEntryTTL: "abc"},
		{paramDirTTL: "0s"},
		{paramAttrTTL: "-1s"},
	}

	for _, values := range tests {
		if _, err := effectiveMountTTLs(values); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("effectiveMountTTLs(%v) status = %s, want InvalidArgument (err=%v)", values, status.Code(err), err)
		}
	}
}
