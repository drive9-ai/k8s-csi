package driver

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEffectiveMountDurability(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "absent", values: nil},
		{name: "close sync", values: map[string]string{paramDurability: " close-sync "}, want: durabilityCloseSync},
		{name: "write sync", values: map[string]string{paramDurability: "write-sync"}, want: durabilityWriteSync},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := effectiveMountDurability(tt.values)
			if err != nil {
				t.Fatalf("effectiveMountDurability error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("durability = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveMountDurabilityRejectsUnsupportedValues(t *testing.T) {
	for _, value := range []string{"", "auto", "Close-Sync", "fsync"} {
		t.Run(value, func(t *testing.T) {
			_, err := effectiveMountDurability(map[string]string{paramDurability: value})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("status = %s, want InvalidArgument (err=%v)", status.Code(err), err)
			}
			for _, text := range []string{paramDurability, durabilityCloseSync, durabilityWriteSync} {
				if !strings.Contains(err.Error(), text) {
					t.Fatalf("error %q does not contain %q", err, text)
				}
			}
		})
	}
}

func TestValidateDurabilityTuning(t *testing.T) {
	for _, durability := range []string{durabilityCloseSync, durabilityWriteSync} {
		err := validateDurabilityTuning(durability, mountTuning{WritebackBatchWindow: "20ms"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("durability %q status = %s, want InvalidArgument (err=%v)", durability, status.Code(err), err)
		}
		if !strings.Contains(err.Error(), paramWritebackBatchWindow) {
			t.Fatalf("error %q does not name %s", err, paramWritebackBatchWindow)
		}
	}
	if err := validateDurabilityTuning("", mountTuning{WritebackBatchWindow: "20ms"}); err != nil {
		t.Fatalf("empty durability changed existing tuning behavior: %v", err)
	}
}
