package dispatch

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	base := 10 * time.Millisecond
	max := 100 * time.Millisecond
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 10 * time.Millisecond},
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},
		{5, 100 * time.Millisecond},
		{6, 100 * time.Millisecond},
		{100, 100 * time.Millisecond},
	}
	for _, c := range cases {
		if got := Backoff(c.attempt, base, max); got != c.want {
			t.Errorf("Backoff(%d) = %s, 期望 %s", c.attempt, got, c.want)
		}
	}
}

func TestBackoffHandlesZeroMax(t *testing.T) {
	if got := Backoff(3, time.Second, 0); got != time.Second {
		t.Errorf("max=0 时应返回 base，实际 %s", got)
	}
}
