package raffles

import (
	"math/rand"
	"regexp"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func entries(n, pairs int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{OrderID: uuid.MustParse("00000000-0000-0000-0000-" + pad(i+1)), Pairs: pairs}
	}
	return out
}

func pad(n int) string { return string(append([]byte("000000000000")[:12-len(itoa(n))], itoa(n)...)) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestCommitmentIsTheSHA256OfTheSeed(t *testing.T) {
	if got := Commitment("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("got %s", got)
	}
	if !Matches("abc", "BA7816BF8F01CFEA414140DE5DAE2223B00361A396177A9CB410FF61F20015AD") || Matches("abd", Commitment("abc")) {
		t.Fatal("Matches must be case-insensitive on the hash and exact on the seed")
	}
}

func TestNewSeedIs128RandomBits(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		s := NewSeed()
		if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(s) || seen[s] {
			t.Fatalf("bad or repeated seed %q", s)
		}
		seen[s] = true
	}
}

func TestTheDrawDoesNotDependOnTheOrderEntriesArriveIn(t *testing.T) {
	in := entries(30, 1)
	shuffled := slices.Clone(in)
	rand.New(rand.NewSource(7)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	if !slices.Equal(Allocate("seed-1", in, 10), Allocate("seed-1", shuffled, 10)) {
		t.Fatal("the same entries and seed must give the same outcome in any arrival order")
	}
}

func TestAnotherSeedGivesAnotherOrder(t *testing.T) {
	order := func(seed string) []uuid.UUID {
		var ids []uuid.UUID
		for _, r := range Allocate(seed, entries(30, 1), 10) {
			ids = append(ids, r.OrderID)
		}
		return ids
	}
	if slices.Equal(order("seed-1"), order("seed-2")) {
		t.Fatal("two seeds must not give the same order of 30 entries")
	}
}

func TestWinnersNeverTakeMoreThanThePairsThatExist(t *testing.T) {
	var in []Entry
	for i, e := range entries(40, 1) {
		e.Pairs = 1 + i%3
		in = append(in, e)
	}
	result := Allocate("seed-x", in, 17)
	taken := 0
	for i, r := range result {
		if r.Rank != i+1 {
			t.Fatalf("ranks must be 1..n in draw order, got %d at %d", r.Rank, i)
		}
		if r.Won {
			taken += r.Pairs
		}
	}
	if taken > 17 || len(result) != 40 {
		t.Fatalf("took %d of 17 pairs over %d entries", taken, len(result))
	}
}

func TestFirstFitLetsASmallerEntryWinAfterABiggerOneFails(t *testing.T) {
	big := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	small := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	for _, seed := range []string{"a", "b", "c", "d", "e"} {
		got := map[uuid.UUID]bool{}
		for _, r := range Allocate(seed, []Entry{{big, 3}, {small, 1}}, 2) {
			got[r.OrderID] = r.Won
		}
		if got[big] || !got[small] {
			t.Fatalf("seed %s: 2 pairs left, a party of 3 can never win and a party of 1 always can: %v", seed, got)
		}
	}
}

func TestEveryEntryIsEquallyLikelyToWin(t *testing.T) {
	// 20 entries, 5 pairs: each should win about 25% of the time. 4000 deterministic seeds keep the test stable.
	in := entries(20, 1)
	wins := map[uuid.UUID]int{}
	for s := 0; s < 4000; s++ {
		for _, r := range Allocate("fair-"+itoa(s), in, 5) {
			if r.Won {
				wins[r.OrderID]++
			}
		}
	}
	total := 0
	for _, e := range in {
		w := wins[e.OrderID]
		total += w
		if float64(w) < 4000*0.25*0.85 || float64(w) > 4000*0.25*1.15 {
			t.Errorf("entry %s won %d of 4000 draws, expected about 1000", e.OrderID, w)
		}
	}
	if total != 4000*5 {
		t.Fatalf("5 pairs per draw expected, got %d in total", total)
	}
}

func TestKeyIsStable(t *testing.T) {
	if Key("s", uuid.Nil) != Key("s", uuid.Nil) || Key("s", uuid.Nil) == Key("t", uuid.Nil) {
		t.Fatal("a key depends on the seed and the id, and only on them")
	}
}
