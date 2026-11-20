package clock

import (
	"testing"
	"time"
)

func TestSystemReturnsUTC(t *testing.T) {
	if loc := (System{}).Now().Location(); loc != time.UTC {
		t.Fatalf("System.Now deveria estar em UTC, obtive %v", loc)
	}
}

func TestManual(t *testing.T) {
	base := time.Date(2026, 1, 31, 12, 0, 0, 0, time.FixedZone("BRT", -3*3600))
	c := NewManual(base)
	if !c.Now().Equal(base) || c.Now().Location() != time.UTC {
		t.Fatalf("Now=%v", c.Now())
	}
	c.Advance(36 * time.Hour)
	if !c.Now().Equal(base.Add(36 * time.Hour)) {
		t.Fatalf("Advance: %v", c.Now())
	}
	next := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	c.Set(next)
	if !c.Now().Equal(next) {
		t.Fatalf("Set: %v", c.Now())
	}
}
