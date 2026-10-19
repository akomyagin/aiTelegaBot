package mtproto

import (
	"math"
	"testing"
)

func TestToMTProtoID(t *testing.T) {
	tests := []struct {
		name   string
		in     int64
		want   int64
		wantOK bool
	}{
		{"typical channel", -1001234567890, 1234567890, true},
		{"minimal valid", -1000000000001, 1, true},
		{"large id", -1009999999999, 9999999999, true},
		{"boundary exact offset", -1000000000000, 0, false}, // X would be 0
		{"basic group", -123456, 0, false},
		{"user positive", 123456, 0, false},
		{"zero", 0, 0, false},
		{"min int64 overflow", math.MinInt64, 0, false},
		{"too large negative", -2000000000001, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ToMTProtoID(tt.in)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("ToMTProtoID(%d) = (%d, %v), want (%d, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestToBotAPIID(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"typical channel", 1234567890, -1001234567890},
		{"minimal", 1, -1000000000001},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToBotAPIID(tt.in); got != tt.want {
				t.Fatalf("ToBotAPIID(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestIDRoundtrip: -100X -> X -> -100X for a range of valid ids.
func TestIDRoundtrip(t *testing.T) {
	for _, botAPIID := range []int64{-1000000000001, -1001234567890, -1002147483647} {
		x, ok := ToMTProtoID(botAPIID)
		if !ok {
			t.Fatalf("ToMTProtoID(%d) not ok", botAPIID)
		}
		if back := ToBotAPIID(x); back != botAPIID {
			t.Errorf("roundtrip %d -> %d -> %d", botAPIID, x, back)
		}
	}
}
