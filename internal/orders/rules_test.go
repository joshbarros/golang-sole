package orders

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		ref   string
		pairs int64
		bad   string
	}{
		{"ENT-1", 1, ""},
		{"ENT-1", 6, ""},
		{"", 1, "reference is required"},
		{"   ", 1, "reference is required"},
		{strings.Repeat("x", 41), 1, "at most 40"},
		{strings.Repeat("x", 40), 1, ""},
		{"ENT-1", 0, "at least 1 pair"},
		{"ENT-1", -3, "at least 1 pair"},
		{"ENT-1", 7, "at most 6 pairs"},
		{"ENT-1", 4294967297, "at most 6 pairs"}, // would wrap to 1 if narrowed to 32 bits
		{"ENT-1", math.MaxInt64, "at most 6 pairs"},
	}
	for _, c := range cases {
		got := Validate(c.ref, c.pairs)
		if (c.bad == "") != (got == "") || (c.bad != "" && !strings.Contains(got, c.bad)) {
			t.Errorf("Validate(%q, %d) = %q, want it to contain %q", c.ref, c.pairs, got, c.bad)
		}
	}
}

func TestTotalIsFaceValuePlusTwelvePercentTruncated(t *testing.T) {
	for _, c := range []struct {
		price int64
		pairs int
		want  int64
	}{{5000, 2, 11200}, {1, 1, 1}, {7, 1, 7}, {100, 1, 112}, {5000, 4, 22400}} {
		got, err := TotalCents(c.price, c.pairs)
		if err != nil || got != c.want {
			t.Errorf("TotalCents(%d,%d) = %d (%v), want %d", c.price, c.pairs, got, err, c.want)
		}
	}
}

func TestTotalThrowsInsteadOfWrapping(t *testing.T) {
	for _, c := range []struct {
		price int64
		pairs int
	}{{math.MaxInt64, 2}, {math.MaxInt64 / 2, 2}, {math.MaxInt64 / 1000, 1}, {-1, 1}} {
		if _, err := TotalCents(c.price, c.pairs); err == nil {
			t.Errorf("TotalCents(%d,%d) must overflow", c.price, c.pairs)
		}
	}
}

func TestShortReasonsSayWhy(t *testing.T) {
	id := uuid.New()
	cases := map[string]string{
		"closed":  "Closed:",
		"already": "Already entered:",
		"drawn":   "Not drawn: place 3 of 7",
	}
	for reason, want := range cases {
		got := ShortReason(messaging.PairsShort{OrderID: id, Reason: reason, Rank: 3, Entries: 7, RequestedPairs: 2, PairsLeft: 1})
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q should contain %q", reason, got, want)
		}
	}
}
