package scheduler

import (
	"testing"
	"time"
)

func TestNextSlot(t *testing.T) {
	utc := time.UTC
	msk, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		t.Fatalf("load Europe/Moscow: %v", err)
	}

	tests := []struct {
		name       string
		now        time.Time
		loc        *time.Location
		hour, min  int
		wantY      int
		wantMonth  time.Month
		wantD      int
		wantH      int
		wantMinute int
	}{
		{
			name: "slot later today",
			now:  time.Date(2026, 7, 8, 8, 0, 0, 0, utc),
			loc:  utc, hour: 9, min: 0,
			wantY: 2026, wantMonth: time.July, wantD: 8, wantH: 9, wantMinute: 0,
		},
		{
			name: "slot already passed today -> tomorrow",
			now:  time.Date(2026, 7, 8, 10, 0, 0, 0, utc),
			loc:  utc, hour: 9, min: 0,
			wantY: 2026, wantMonth: time.July, wantD: 9, wantH: 9, wantMinute: 0,
		},
		{
			name: "now exactly at slot -> tomorrow",
			now:  time.Date(2026, 7, 8, 9, 0, 0, 0, utc),
			loc:  utc, hour: 9, min: 0,
			wantY: 2026, wantMonth: time.July, wantD: 9, wantH: 9, wantMinute: 0,
		},
		{
			// 05:00 UTC == 08:00 MSK, so a 09:00 MSK slot is still ahead today.
			name: "timezone honored (Europe/Moscow)",
			now:  time.Date(2026, 7, 8, 5, 0, 0, 0, utc),
			loc:  msk, hour: 9, min: 0,
			wantY: 2026, wantMonth: time.July, wantD: 8, wantH: 9, wantMinute: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextSlot(tt.now, tt.loc, tt.hour, tt.min)
			gy, gm, gd := got.In(tt.loc).Date()
			gh, gmin, _ := got.In(tt.loc).Clock()
			if gy != tt.wantY || gm != tt.wantMonth || gd != tt.wantD || gh != tt.wantH || gmin != tt.wantMinute {
				t.Errorf("nextSlot = %v, want %04d-%02d-%02d %02d:%02d",
					got.In(tt.loc), tt.wantY, tt.wantMonth, tt.wantD, tt.wantH, tt.wantMinute)
			}
			if !got.After(tt.now) {
				t.Errorf("nextSlot %v is not after now %v", got, tt.now)
			}
		})
	}
}
