package messaging

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPoisonIsACaseSensitiveSubstring(t *testing.T) {
	for ref, want := range map[string]bool{"POISON": true, "ENT-POISON-7": true, "xPOISONx": true, "poison": false, "Poison": false, "POISO N": false, "": false} {
		if IsPoison(ref) != want {
			t.Errorf("IsPoison(%q) should be %v", ref, want)
		}
	}
}

func TestRetryPolicy(t *testing.T) {
	transient := errors.New("database is restarting")
	for attempt, want := range map[int]FailureAction{0: ActionRetry, 1: ActionRetry, 2: ActionDeadLetter, 3: ActionDeadLetter} {
		if got := Decide(attempt, transient); got != want {
			t.Errorf("attempt %d: got %v, want %v", attempt, got, want)
		}
	}
	if Decide(0, fmt.Errorf("%w: reason", ErrPoison)) != ActionDeadLetter {
		t.Error("a poison message is dead-lettered at once, even on the first attempt")
	}
	if Decide(0, &OutOfOrderError{Reason: "early"}) != ActionRetry {
		t.Error("an out-of-order message is retried")
	}
}

func TestMessagesUseTheWireNamesTheOtherSystemsUse(t *testing.T) {
	id := uuid.MustParse("01a1831c-3d9d-72e6-8f15-269852212912")
	b, _ := json.Marshal(PairsShort{OrderID: id, Reference: "R", RequestedPairs: 2, PairsLeft: 1, Reason: "drawn", Rank: 3, Entries: 7})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"orderId", "reference", "requestedPairs", "pairsLeft", "reason", "rank", "entries"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %q in %s", k, b)
		}
	}
	back, err := Parse[PairsShort](b)
	if err != nil || back.Rank != 3 || back.OrderID != id {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	if _, err := Parse[PairsHeld]([]byte("{not json")); err == nil {
		t.Error("a malformed body is an error")
	}
}

func TestTheKafkaEnvelope(t *testing.T) {
	row := Row{MessageID: uuid.New(), Module: "orders", RoutingKey: ReserveKey, Key: "k", Payload: []byte(`{"orderId":"x"}`), CreatedAt: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	b, err := EnvelopeOf(row)
	if err != nil {
		t.Fatal(err)
	}
	var e map[string]json.RawMessage
	_ = json.Unmarshal(b, &e)
	for _, k := range []string{"id", "type", "module", "occurredAt", "payload"} {
		if _, ok := e[k]; !ok {
			t.Errorf("envelope lacks %q: %s", k, b)
		}
	}
	if string(e["type"]) != `"pairs.reserve"` || string(e["payload"]) != `{"orderId":"x"}` {
		t.Errorf("unexpected envelope %s", b)
	}
}

func TestRoutingKeysAreDistinct(t *testing.T) {
	keys := []string{ReserveKey, ReleaseKey, HeldKey, ShortKey, EnteredKey, ReleasedKey, IssuedKey, RejectedKey}
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			t.Fatalf("routing key %q is empty or repeated", k)
		}
		seen[k] = true
	}
}
