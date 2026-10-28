// Package raffles owns the raffle, its entries, the draw and the pair holds.
package raffles

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// The draw, as pure functions so anyone can recompute it. Entries are ordered by SHA-256("seed:orderId"); the pairs are
// then handed out in that order, each entry taking its pairs when they still fit (first fit). The seed is committed (as
// its hash) before any entry exists, so the operator cannot choose the order after seeing the entries.

// NewSeed is 128 random bits.
func NewSeed() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Commitment is the SHA-256 of the seed, published when the raffle opens.
func Commitment(seed string) string { return hash(seed) }

// Key is the number an entry is ordered by.
func Key(seed string, orderID uuid.UUID) string { return hash(seed + ":" + orderID.String()) }

// Matches reports whether the seed is the one that was committed to.
func Matches(seed, seedHash string) bool { return strings.EqualFold(Commitment(seed), seedHash) }

// Entry is what the draw needs to know about one entry.
type Entry struct {
	OrderID uuid.UUID
	Pairs   int
}

// Result is one entry's outcome.
type Result struct {
	OrderID uuid.UUID
	Pairs   int
	Won     bool
	Rank    int
}

// Allocate orders the entries and hands the pairs out first fit. Ties on the key cannot happen for distinct ids in
// practice; the id breaks them anyway.
func Allocate(seed string, entries []Entry, pairsLeft int) []Result {
	type keyed struct {
		Entry
		key string
	}
	ordered := make([]keyed, len(entries))
	for i, e := range entries {
		ordered[i] = keyed{e, Key(seed, e.OrderID)}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].key != ordered[j].key {
			return ordered[i].key < ordered[j].key
		}
		return ordered[i].OrderID.String() < ordered[j].OrderID.String()
	})
	out := make([]Result, 0, len(ordered))
	remaining := pairsLeft
	for i, e := range ordered {
		won := e.Pairs <= remaining
		if won {
			remaining -= e.Pairs
		}
		out = append(out, Result{OrderID: e.OrderID, Pairs: e.Pairs, Won: won, Rank: i + 1})
	}
	return out
}
