package led

import (
	"testing"
	"time"
)

func TestHealthyDeadlineClamping(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		remaining time.Duration
		want      int
	}{{5 * time.Second, 2}, {2500 * time.Millisecond, 0}, {time.Minute, 57}, {time.Hour, 90}, {-time.Second, 0}} {
		if got := HealthyTTL(now, now.Add(tt.remaining), 2*time.Second, 500*time.Millisecond); got != tt.want {
			t.Errorf("%s: got %d want %d", tt.remaining, got, tt.want)
		}
	}
}
