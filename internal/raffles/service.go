package raffles

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
	"github.com/joshbarros/golang-sole/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "raffles"
	ConsumerName = "raffles"
)

// Raffle statuses and entry outcomes.
const (
	StatusOpen  = "Open"
	StatusDrawn = "Drawn"

	OutcomeEntered = "Entered"
	OutcomeWon     = "Won"
	OutcomeLost    = "Lost"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// EnterOutcome is what entering did.
type EnterOutcome int

// The ways an entry can end.
const (
	Entered EnterOutcome = iota
	Closed
	AlreadyEntered
	Duplicate
)

// Service is the raffles module's logic.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "raffles"}, Log: log}
}

// Handle implements messaging.Handler for the raffles queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReservePairs](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Enter(ctx, d.MessageID, cmd, time.Now())
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleasePairs](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("raffles does not handle %q", d.RoutingKey)
}

// Enter records one entry per person per raffle, while the raffle is open and before its draw time. Entering takes no pairs.
func (s *Service) Enter(ctx context.Context, messageID uuid.UUID, cmd messaging.ReservePairs, now time.Time) (EnterOutcome, error) {
	outcome := Entered
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		var status string
		var drawAt time.Time
		err = tx.QueryRow(ctx, `SELECT status, draw_at FROM raffles.raffles WHERE id = $1`, cmd.RaffleID).Scan(&status, &drawAt)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (status != StatusOpen || !drawAt.After(now))) {
			outcome = Closed
			return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
				messaging.PairsShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedPairs: cmd.Pairs, Reason: "closed"})
		}
		if err != nil {
			return err
		}
		// The unique constraint on (raffle, owner) is the rule: a second entry by the same person inserts nothing.
		tag, err := tx.Exec(ctx, `
			INSERT INTO raffles.raffle_entries (order_id, owner_id, raffle_id, pairs, reference, outcome, entered_at)
			VALUES ($1,$2,$3,$4,$5,'Entered',$6) ON CONFLICT DO NOTHING`,
			cmd.OrderID, cmd.OwnerID, cmd.RaffleID, cmd.Pairs, cmd.Reference, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			outcome = AlreadyEntered
			return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
				messaging.PairsShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedPairs: cmd.Pairs, Reason: "already"})
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.EnteredKey, cmd.OrderID, messaging.PairsEntered{OrderID: cmd.OrderID, DrawAt: drawAt})
	})
	return outcome, err
}

// DrawDue draws every raffle whose time has come and returns how many this call drew. Each raffle is claimed by a
// conditional UPDATE (Open to Drawn), so several replicas can run this at once and exactly one draws a given raffle.
func (s *Service) DrawDue(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM raffles.raffles WHERE status = 'Open' AND draw_at <= $1 ORDER BY draw_at`, now)
	if err != nil {
		return 0, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	drawn := 0
	for _, id := range ids {
		ok, err := s.Draw(ctx, id, now)
		if err != nil {
			return drawn, err
		}
		if ok {
			drawn++
		}
	}
	return drawn, nil
}

// Draw draws one raffle if it is open and due. It locks the entries it draws (FOR UPDATE), so a withdrawal racing the draw
// either lands first and the entry is not drawn, or lands after and releases the pairs it won.
func (s *Service) Draw(ctx context.Context, raffleID uuid.UUID, now time.Time) (bool, error) {
	drew := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		var seed string
		var pairsLeft int
		err := tx.QueryRow(ctx, `
			UPDATE raffles.raffles SET status = 'Drawn', drawn_at = $2
			WHERE id = $1 AND status = 'Open' AND draw_at <= $2
			RETURNING seed, pairs_left`, raffleID, now).Scan(&seed, &pairsLeft)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // another replica drew it, or it is not time yet
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT order_id, owner_id, pairs, reference FROM raffles.raffle_entries
			WHERE raffle_id = $1 AND outcome = 'Entered' ORDER BY order_id FOR UPDATE`, raffleID)
		if err != nil {
			return err
		}
		type entry struct {
			owner     uuid.UUID
			reference string
		}
		byOrder := map[uuid.UUID]entry{}
		var entries []Entry
		for rows.Next() {
			var id, owner uuid.UUID
			var pairs int
			var ref string
			if err := rows.Scan(&id, &owner, &pairs, &ref); err != nil {
				rows.Close()
				return err
			}
			byOrder[id] = entry{owner, ref}
			entries = append(entries, Entry{OrderID: id, Pairs: pairs})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		taken := 0
		for _, r := range Allocate(seed, entries, pairsLeft) {
			e := byOrder[r.OrderID]
			outcome := OutcomeLost
			if r.Won {
				outcome = OutcomeWon
			}
			if _, err := tx.Exec(ctx, `UPDATE raffles.raffle_entries SET outcome = $2, rank = $3 WHERE order_id = $1`, r.OrderID, outcome, r.Rank); err != nil {
				return err
			}
			if r.Won {
				taken += r.Pairs
				if _, err := tx.Exec(ctx, `INSERT INTO raffles.pair_holds (order_id, owner_id, raffle_id, pairs, status, created_at) VALUES ($1,$2,$3,$4,'Held',$5)`,
					r.OrderID, e.owner, raffleID, r.Pairs, now); err != nil {
					return err
				}
				if err := s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, r.OrderID,
					messaging.PairsHeld{OrderID: r.OrderID, OwnerID: e.owner, RaffleID: raffleID, Pairs: r.Pairs, Reference: e.reference}); err != nil {
					return err
				}
			} else if err := s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, r.OrderID,
				messaging.PairsShort{OrderID: r.OrderID, Reference: e.reference, RequestedPairs: r.Pairs, PairsLeft: pairsLeft - taken, Reason: "drawn", Rank: r.Rank, Entries: len(entries)}); err != nil {
				return err
			}
		}
		if taken > 0 {
			if _, err := tx.Exec(ctx, `UPDATE raffles.raffles SET pairs_left = pairs_left - $2 WHERE id = $1 AND pairs_left >= $2`, raffleID, taken); err != nil {
				return err
			}
		}
		drew = true
		return nil
	})
	return drew, err
}

// Release is leaving the raffle. Before the draw it just deletes the entry; after it, it gives back the pairs the entry won
// (exactly once), and a losing entry has nothing to give back.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleasePairs) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM raffles.raffle_entries WHERE order_id = $1 AND outcome = 'Entered'`, cmd.OrderID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			released = true
			return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.PairsReleased{OrderID: cmd.OrderID, Pairs: 0, Reason: cmd.Reason})
		}
		var raffleID uuid.UUID
		var pairs int
		err = tx.QueryRow(ctx, `
			UPDATE raffles.pair_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE order_id = $1 AND status = 'Held' RETURNING raffle_id, pairs`, cmd.OrderID, cmd.Reason).Scan(&raffleID, &pairs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held (a loser, or already released): nothing to give back
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE raffles.raffles SET pairs_left = pairs_left + $2 WHERE id = $1 AND pairs_left + $2 <= pairs_total`, raffleID, pairs); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.PairsReleased{OrderID: cmd.OrderID, Pairs: pairs, Reason: cmd.Reason})
	})
	return released, err
}

// Run draws due raffles every interval until the context ends. Every replica runs one.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.DrawDue(ctx, time.Now()); err != nil {
				if ctx.Err() == nil {
					s.Log.Warn("drawing raffles failed; it will be tried again", "error", err)
				}
			} else if n > 0 {
				s.Log.Info("drew raffles", "count", n)
			}
		}
	}
}
