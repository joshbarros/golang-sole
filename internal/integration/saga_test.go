package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-sole/internal/claims"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
)

func status(e *env, s session, id string) string { return e.order(s, id).str("status") }

func TestTheWholeSagaFromEntryToSoldAndTheClaim(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(10, time.Hour)
	ana := e.register()
	created := e.enter(ana, raffle, "ENT-1", 4)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["totalCents"] != float64(22400) {
		t.Fatalf("accepted as Requested with the total, fee included: %s", created.Raw)
	}
	e.drain()
	if got := status(e, ana, id); got != "Entered" {
		t.Fatalf("entered: %s", got)
	}
	if e.get("/api/raffles/"+raffle, ana.Token).Body["pairsLeft"] != float64(10) {
		t.Fatal("entering takes no pairs")
	}

	e.drawNow(raffle)
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Sold" || !strings.HasPrefix(o.str("claimNumber"), "SL-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a claim number and no leftover reason: %s", o.Raw)
	}
	if e.get("/api/raffles/"+raffle, ana.Token).Body["pairsLeft"] != float64(6) {
		t.Fatal("4 of 10 pairs are held")
	}
	c := e.get("/api/claims/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("claimNumber") {
		t.Fatalf("the claim: %d %s", c.Status, c.Raw)
	}
}

func TestLosersAreToldWhereTheyCameUpAndGetNoClaim(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(1, time.Hour)
	users := []session{e.register(), e.register(), e.register()}
	var ids []string
	for i, u := range users {
		ids = append(ids, e.enter(u, raffle, "ENT-"+string(rune('A'+i)), 1).str("id"))
	}
	e.drain()
	e.drawNow(raffle)
	e.drain()

	sold, notDrawn := 0, 0
	for i, u := range users {
		o := e.order(u, ids[i])
		switch o.str("status") {
		case "Sold":
			sold++
		case "Rejected":
			if !strings.HasPrefix(o.str("reason"), "Not drawn: place ") || !strings.Contains(o.str("reason"), " of 3") {
				t.Fatalf("a loser is told its place: %s", o.Raw)
			}
			if e.get("/api/claims/"+ids[i], u.Token).Status != 404 {
				t.Fatal("a loser has no claim")
			}
			notDrawn++
		}
	}
	if sold != 1 || notDrawn != 2 {
		t.Fatalf("one pair: 1 sold, 2 not drawn; got %d and %d", sold, notDrawn)
	}
}

func TestPoisonWinsTheDrawIsRejectedAndTheNextStepReleasesThePairs(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	id := e.enter(ana, raffle, "ENT-POISON-1", 2).str("id")
	e.drain()
	e.drawNow(raffle)
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison claim is dead-lettered once: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["pairsReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.get("/api/raffles/"+raffle, ana.Token).Body["pairsLeft"] != float64(5) {
		t.Fatal("the pairs are back")
	}
	if c := e.get("/api/claims/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the claim: %s", c.Raw)
	}
}

func TestRefundBeforeTheDrawWithdrawsTheEntry(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(1, time.Hour)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, raffle, "ENT-A", 1).str("id")
	b := e.enter(bob, raffle, "ENT-B", 1).str("id")
	e.drain()

	if r := e.post("/api/orders/"+a+"/refund", nil, ana.Token); r.Status != 202 || r.str("status") != "Refunded" {
		t.Fatalf("withdraw: %d %s", r.Status, r.Raw)
	}
	e.drain()
	e.drawNow(raffle)
	e.drain()
	if status(e, bob, b) != "Sold" {
		t.Fatalf("with the other entry withdrawn, the only entry wins: %s", status(e, bob, b))
	}
	if status(e, ana, a) != "Refunded" || e.order(ana, a).Body["pairsReleasedAt"] == nil {
		t.Fatal("the withdrawn order stays Refunded")
	}
}

func TestRefundAfterSaleReturnsThePairsButKeepsTheClaim(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	id := e.enter(ana, raffle, "ENT-1", 2).str("id")
	e.drain()
	e.drawNow(raffle)
	e.drain()
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Refunded" || e.get("/api/raffles/"+raffle, ana.Token).Body["pairsLeft"] != float64(5) {
		t.Fatal("refunded and the pairs are back on the shelf")
	}
	if c := e.get("/api/claims/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the claim that was issued")
	}
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, raffle, "ENT-1", 1).str("id"))
	e.drain()
	e.drawNow(raffle)
	// run the draw's events and the claim, but stop before the order sees claim.issued
	for pass := 0; pass < 3; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, claim issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/orders/"+id.String()+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain() // now the final mark arrives
	o := e.order(ana, id.String())
	if o.str("status") != "Refunded" || o.Body["claimNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/claims/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued claim stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	e.enter(ana, raffle, "ENT-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM raffles.raffle_entries`) != 1 || e.outbox("raffles", messaging.EnteredKey) != 1 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotDropped(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, raffle, "ENT-1", 1).str("id"))
	e.drain()
	// the claim is issued but the Held mark has not reached the order: the order is still Entered
	_, err := e.app.Orders.MarkSold(context.Background(), uuid.New(), messaging.ClaimIssued{OrderID: id, Number: "SL-TEST"}, 0)
	var ooo *messaging.OutOfOrderError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Entered" {
		t.Fatal("and change nothing")
	}
	e.drawNow(raffle)
	e.drain()
	if status(e, ana, id.String()) != "Sold" {
		t.Fatal("the normal flow still completes")
	}
}

func TestMarksOnlyMoveAnOrderFromTheStatusTheyExpect(t *testing.T) {
	e := newEnv(t)
	raffle := e.openRaffle(5, time.Hour)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, raffle, "ENT-1", 1).str("id"))
	ctx := context.Background()
	mark := func(f func() (int, error)) int {
		n, err := f()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Requested: the Entered mark applies, then it does not apply again (a new message id each time)
	if mark(func() (int, error) {
		o, err := e.app.Orders.MarkEntered(ctx, uuid.New(), messaging.PairsEntered{OrderID: id, DrawAt: time.Now()})
		return int(o), err
	}) != 0 {
		t.Fatal("Requested -> Entered")
	}
	if got := mark(func() (int, error) {
		o, err := e.app.Orders.MarkEntered(ctx, uuid.New(), messaging.PairsEntered{OrderID: id, DrawAt: time.Now()})
		return int(o), err
	}); got != 1 {
		t.Fatalf("an Entered mark on an Entered order is skipped, got outcome %d", got)
	}
	if status(e, ana, id.String()) != "Entered" {
		t.Fatal("still Entered")
	}
	// a rejection from the claims module cannot move an Entered order
	if got, _ := e.app.Orders.MarkRejected(ctx, uuid.New(), messaging.ClaimRejected{OrderID: id, Reason: "x"}); int(got) != 1 {
		t.Fatalf("a claim rejection only moves Held or Requested orders, got %d", got)
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.PairsHeld{OrderID: id, OwnerID: owner, RaffleID: uuid.New(), Pairs: 1, Reference: "ENT-1"}
	first, err := e.app.Claims.Issue(context.Background(), msg, held)
	again, err2 := e.app.Claims.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != claims.Issued || again != claims.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.PairsHeld{OrderID: uuid.New(), OwnerID: owner, RaffleID: uuid.New(), Pairs: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Claims.Issue(context.Background(), uuid.New(), poison); out != claims.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("claims", messaging.ReleaseKey) != 1 || e.outbox("claims", messaging.RejectedKey) != 1 || e.outbox("claims", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}
