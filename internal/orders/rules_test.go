package orders

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-bistro/internal/platform/messaging"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		ref    string
		covers int64
		bad    string
	}{
		{"RSV-1", 1, ""},
		{"RSV-1", 6, ""},
		{"", 1, "reference is required"},
		{"   ", 1, "reference is required"},
		{strings.Repeat("x", 41), 1, "at most 40"},
		{strings.Repeat("x", 40), 1, ""},
		{"RSV-1", 0, "at least 1 cover"},
		{"RSV-1", -3, "at least 1 cover"},
		{"RSV-1", 7, "at most 6 covers"},
		{"RSV-1", 4294967297, "at most 6 covers"}, // would wrap to 1 if narrowed to 32 bits
		{"RSV-1", math.MaxInt64, "at most 6 covers"},
	}
	for _, c := range cases {
		got := Validate(c.ref, c.covers)
		if (c.bad == "") != (got == "") || (c.bad != "" && !strings.Contains(got, c.bad)) {
			t.Errorf("Validate(%q, %d) = %q, want it to contain %q", c.ref, c.covers, got, c.bad)
		}
	}
}

func TestTotalIsFaceValuePlusTwelvePercentTruncated(t *testing.T) {
	for _, c := range []struct {
		price  int64
		covers int
		want   int64
	}{{5000, 2, 11200}, {1, 1, 1}, {7, 1, 7}, {100, 1, 112}, {5000, 4, 22400}} {
		got, err := TotalCents(c.price, c.covers)
		if err != nil || got != c.want {
			t.Errorf("TotalCents(%d,%d) = %d (%v), want %d", c.price, c.covers, got, err, c.want)
		}
	}
}

func TestTotalThrowsInsteadOfWrapping(t *testing.T) {
	for _, c := range []struct {
		price  int64
		covers int
	}{{math.MaxInt64, 2}, {math.MaxInt64 / 2, 2}, {math.MaxInt64 / 1000, 1}, {-1, 1}} {
		if _, err := TotalCents(c.price, c.covers); err == nil {
			t.Errorf("TotalCents(%d,%d) must overflow", c.price, c.covers)
		}
	}
}

func TestShortReasonsSayWhy(t *testing.T) {
	id := uuid.New()
	cases := map[string]string{
		"cap":   "Cap: at most 2 covers per guest",
		"stock": "Short: 3 covers wanted, 1 left.",
	}
	for reason, want := range cases {
		got := ShortReason(messaging.CoversShort{OrderID: id, Reason: reason, PerGuestLimit: 2, RequestedCovers: 3, CoversLeft: 1})
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q should contain %q", reason, got, want)
		}
	}
}
