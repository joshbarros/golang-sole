package integration

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
	"github.com/joshbarros/golang-sole/internal/raffles"
)

// A raffle row made directly, with a seed we know, so a test can recompute the draw.
func (e *env) rawRaffle(pairs int, drawAt time.Time, seed string) (uuid.UUID, string) {
	e.t.Helper()
	id, _ := uuid.NewV7()
	c := code()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO raffles.raffles (id, code, name, colorway, brand, draw_at, price_cents, pairs_total, pairs_left, status, seed, seed_hash, created_at)
		VALUES ($1,$2,'t','c','b',$3,5000,$4,$4,'Open',$5,$6,now())`, id, c, drawAt, pairs, seed, raffles.Commitment(seed))
	if err != nil {
		e.t.Fatal(err)
	}
	return id, c
}

func (e *env) enterRaw(raffle uuid.UUID, pairs int, now time.Time) (uuid.UUID, raffles.EnterOutcome) {
	e.t.Helper()
	order, owner := uuid.New(), uuid.New()
	out, err := e.app.Raffles.Enter(context.Background(), uuid.New(), messaging.ReservePairs{OrderID: order, OwnerID: owner, RaffleID: raffle, Pairs: pairs, Reference: "ENT-RAW"}, now)
	if err != nil {
		e.t.Fatal(err)
	}
	return order, out
}

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestEnteringTakesNoStockAndOnePersonGetsOneEntry(t *testing.T) {
	e := newEnv(t)
	raffle, _ := e.rawRaffle(5, time.Now().Add(time.Hour), "s")
	owner := uuid.New()
	enter := func(o uuid.UUID) raffles.EnterOutcome {
		out, err := e.app.Raffles.Enter(context.Background(), uuid.New(), messaging.ReservePairs{OrderID: uuid.New(), OwnerID: o, RaffleID: raffle, Pairs: 1, Reference: "R"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if enter(owner) != raffles.Entered || enter(owner) != raffles.AlreadyEntered || enter(uuid.New()) != raffles.Entered {
		t.Fatal("one entry per person: the second by the same owner is refused, another person is fine")
	}
	if e.count(`SELECT count(*) FROM raffles.raffle_entries`) != 2 || e.count(`SELECT pairs_left FROM raffles.raffles WHERE id = $1`, raffle) != 5 || e.count(`SELECT count(*) FROM raffles.pair_holds`) != 0 {
		t.Fatal("entering records entries only: no pairs are taken and nothing is held")
	}
	if e.outbox("raffles", messaging.EnteredKey) != 2 || e.outbox("raffles", messaging.ShortKey) != 1 {
		t.Fatal("two entered events and one refusal")
	}
}

func TestAfterTheDrawTimeEntriesAreRefused(t *testing.T) {
	e := newEnv(t)
	drawAt := time.Now().Add(time.Minute)
	raffle, _ := e.rawRaffle(5, drawAt, "s")
	if _, out := e.enterRaw(raffle, 1, drawAt.Add(time.Second)); out != raffles.Closed {
		t.Fatalf("entry after the draw time: %v", out)
	}
	if e.count(`SELECT count(*) FROM raffles.raffle_entries`) != 0 {
		t.Fatal("a refused entry leaves no row")
	}
	if _, out := e.enterRaw(uuid.New(), 1, time.Now()); out != raffles.Closed {
		t.Fatal("an unknown raffle is closed too")
	}
}

func TestTheDrawHoldsExactlyWhatTheCommittedOrderSaysAndRejectsTheRest(t *testing.T) {
	e := newEnv(t)
	seed := "fixed-seed-for-the-test"
	raffle, _ := e.rawRaffle(4, time.Now().Add(-time.Second), seed)
	// entries must exist before the draw time: make them with a clock in the past
	var in []raffles.Entry
	for i := 0; i < 12; i++ {
		order, out := e.enterRaw(raffle, 1+i%2, time.Now().Add(-time.Hour))
		if out != raffles.Entered {
			t.Fatal(out)
		}
		in = append(in, raffles.Entry{OrderID: order, Pairs: 1 + i%2})
	}
	drew, err := e.app.Raffles.Draw(context.Background(), raffle, time.Now())
	if err != nil || !drew {
		t.Fatalf("draw: %v %v", drew, err)
	}
	// anyone can recompute this from the seed and the order ids
	expected := raffles.Allocate(seed, in, 4)
	won := 0
	taken := 0
	for _, r := range expected {
		var outcome string
		var rank int
		if err := e.pool.QueryRow(context.Background(), `SELECT outcome, rank FROM raffles.raffle_entries WHERE order_id = $1`, r.OrderID).Scan(&outcome, &rank); err != nil {
			t.Fatal(err)
		}
		if (outcome == "Won") != r.Won || rank != r.Rank {
			t.Fatalf("entry %s: got %s rank %d, the recomputed draw says won=%v rank %d", r.OrderID, outcome, rank, r.Won, r.Rank)
		}
		if r.Won {
			won++
			taken += r.Pairs
		}
	}
	if e.count(`SELECT count(*) FROM raffles.pair_holds`) != won || e.count(`SELECT pairs_left FROM raffles.raffles WHERE id = $1`, raffle) != 4-taken {
		t.Fatal("one hold per winner, and the pairs they took are gone")
	}
	if e.outbox("raffles", messaging.HeldKey) != won || e.outbox("raffles", messaging.ShortKey) != 12-won {
		t.Fatal("a held event per winner and a short event per loser")
	}
}

func TestADrawBeforeItsTimeDoesNothingAndARaffleIsDrawnOnce(t *testing.T) {
	e := newEnv(t)
	raffle, _ := e.rawRaffle(2, time.Now().Add(time.Hour), "s")
	e.enterRaw(raffle, 1, time.Now())
	if drew, _ := e.app.Raffles.Draw(context.Background(), raffle, time.Now()); drew {
		t.Fatal("before its time nothing happens")
	}
	if drew, _ := e.app.Raffles.Draw(context.Background(), raffle, time.Now().Add(2*time.Hour)); !drew {
		t.Fatal("at its time it is drawn")
	}
	if drew, _ := e.app.Raffles.Draw(context.Background(), raffle, time.Now().Add(2*time.Hour)); drew {
		t.Fatal("never twice")
	}
	if e.count(`SELECT count(*) FROM raffles.pair_holds`) != 1 {
		t.Fatal("one entry, one hold")
	}
}

func TestSeveralReplicasDrawingAtOnceDrawEachRaffleExactlyOnce(t *testing.T) {
	e := newEnv(t)
	raffle, _ := e.rawRaffle(5, time.Now().Add(-time.Second), "s")
	for i := 0; i < 20; i++ {
		e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := e.app.Raffles.DrawDue(context.Background(), time.Now())
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 || e.count(`SELECT count(*) FROM raffles.pair_holds`) != 5 || e.count(`SELECT pairs_left FROM raffles.raffles WHERE id = $1`, raffle) != 0 || e.outbox("raffles", messaging.HeldKey) != 5 {
		t.Fatalf("drawn %d times, %d holds", total, e.count(`SELECT count(*) FROM raffles.pair_holds`))
	}
}

func release(e *env, order uuid.UUID, reason string) bool {
	ok, err := e.app.Raffles.Release(context.Background(), uuid.New(), messaging.ReleasePairs{OrderID: order, Reason: reason})
	if err != nil {
		e.t.Fatal(err)
	}
	return ok
}

func TestLeavingBeforeTheDrawRemovesTheEntry(t *testing.T) {
	e := newEnv(t)
	raffle, _ := e.rawRaffle(1, time.Now().Add(-time.Second), "s")
	leaver, _ := e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
	stays, _ := e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
	if !release(e, leaver, "refund") || release(e, leaver, "again") {
		t.Fatal("leaving works once")
	}
	if _, err := e.app.Raffles.Draw(context.Background(), raffle, time.Now()); err != nil {
		t.Fatal(err)
	}
	var winner uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT order_id FROM raffles.pair_holds`).Scan(&winner); err != nil || winner != stays {
		t.Fatalf("only the entry that stayed can win: %v %v", winner, err)
	}
	if e.count(`SELECT count(*) FROM raffles.raffle_entries WHERE order_id = $1`, leaver) != 0 {
		t.Fatal("the leaver's entry is gone")
	}
}

func TestAfterTheDrawAWinnerGivesItsPairsBackOnceAndALoserHasNothingToGive(t *testing.T) {
	e := newEnv(t)
	raffle, _ := e.rawRaffle(1, time.Now().Add(-time.Second), "s")
	a, _ := e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
	b, _ := e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
	if _, err := e.app.Raffles.Draw(context.Background(), raffle, time.Now()); err != nil {
		t.Fatal(err)
	}
	var winner uuid.UUID
	_ = e.pool.QueryRow(context.Background(), `SELECT order_id FROM raffles.pair_holds`).Scan(&winner)
	loser := a
	if winner == a {
		loser = b
	}
	if release(e, loser, "refund") {
		t.Fatal("a loser has nothing to give back")
	}
	if !release(e, winner, "refund") || release(e, winner, "again") {
		t.Fatal("a winner gives its pairs back exactly once")
	}
	if e.count(`SELECT pairs_left FROM raffles.raffles WHERE id = $1`, raffle) != 1 {
		t.Fatal("the pair is back on the shelf, once")
	}
}

func TestLeavingAndTheDrawRacingNeverLoseOrDoubleCountAPair(t *testing.T) {
	for round := 0; round < 8; round++ {
		e := newEnv(t)
		raffle, _ := e.rawRaffle(3, time.Now().Add(-time.Second), "s")
		var orders []uuid.UUID
		for i := 0; i < 6; i++ {
			o, _ := e.enterRaw(raffle, 1, time.Now().Add(-time.Hour))
			orders = append(orders, o)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.app.Raffles.Draw(context.Background(), raffle, time.Now()); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for _, o := range orders[:3] {
				release(e, o, "refund")
			}
		}()
		close(start)
		wg.Wait()

		left := e.count(`SELECT pairs_left FROM raffles.raffles WHERE id = $1`, raffle)
		held := e.count(`SELECT coalesce(sum(pairs),0) FROM raffles.pair_holds WHERE status = 'Held'`)
		if left+held != 3 {
			t.Fatalf("round %d: every pair is on the shelf or held, once: left %d + held %d", round, left, held)
		}
		// all three leavers are gone for good: whichever side of the draw they landed on, none still holds a pair
		for _, o := range orders[:3] {
			if e.count(`SELECT count(*) FROM raffles.pair_holds WHERE order_id = $1 AND status = 'Held'`, o) != 0 {
				t.Fatalf("round %d: a leaver still holds a pair", round)
			}
		}
	}
}

func TestTheDrawViewHidesTheSeedUntilTheDrawAndThenLetsAnyoneCheckIt(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(1, time.Hour)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, raffle, "ENT-A", 1).str("id")
	b := e.enter(bob, raffle, "ENT-B", 1).str("id")
	e.drain()

	before := e.get("/api/raffles/"+raffle, ana.Token)
	if before.Body["seed"] != nil || before.str("seedHash") == "" || before.Body["status"] != "Open" || before.Body["entries"] != float64(2) {
		t.Fatalf("before the draw only the commitment is public: %s", before.Raw)
	}
	view := e.get("/api/raffles/"+raffle+"/draw", ana.Token)
	if view.Body["seed"] != nil || view.Body["seedMatchesHash"] != false {
		t.Fatalf("no seed before the draw: %s", view.Raw)
	}

	e.drawNow(raffle)
	after := e.get("/api/raffles/"+raffle, ana.Token)
	seed := after.str("seed")
	if after.Body["status"] != "Drawn" || !raffles.Matches(seed, before.str("seedHash")) {
		t.Fatalf("the revealed seed must hash to the commitment: %s", after.Raw)
	}
	mine := e.get("/api/raffles/"+raffle+"/draw", ana.Token)
	rows, _ := mine.Body["mine"].([]any)
	if mine.Body["seedMatchesHash"] != true || len(rows) != 1 || rows[0].(map[string]any)["orderId"] != a {
		t.Fatalf("a person sees only their own entry: %s", mine.Raw)
	}
	all := e.get("/api/raffles/"+raffle+"/draw", e.admin().Token)
	staffRows, _ := all.Body["mine"].([]any)
	if len(staffRows) != 2 || all.Body["winners"] != float64(1) {
		t.Fatalf("staff see every entry: %s", all.Raw)
	}
	keys := []string{}
	for _, r := range staffRows {
		row := r.(map[string]any)
		id, _ := uuid.Parse(row["orderId"].(string))
		if row["key"] != raffles.Key(seed, id) {
			t.Fatalf("each entry's key is SHA-256(seed:orderId): %v", row)
		}
		keys = append(keys, row["key"].(string))
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatal("entries come back in draw order, lowest key first")
	}
	if staffRows[0].(map[string]any)["outcome"] != "Won" || staffRows[1].(map[string]any)["outcome"] != "Lost" {
		t.Fatalf("one pair, first fit in key order: %s", all.Raw)
	}
	_ = b
}
