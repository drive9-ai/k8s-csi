package driver

import "testing"

func TestDurabilityFromParameters(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "absent", values: nil},
		{name: "empty", values: map[string]string{paramDurability: "  "}},
		{name: "trimmed", values: map[string]string{paramDurability: " custom-durability "}, want: "custom-durability"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := durabilityFromParameters(tt.values)
			if got != tt.want {
				t.Fatalf("durability = %q, want %q", got, tt.want)
			}
		})
	}
}
