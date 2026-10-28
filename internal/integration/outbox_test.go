package integration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-sole/internal/platform/messaging"
	"github.com/joshbarros/golang-sole/internal/platform/pgdb"
	"github.com/joshbarros/golang-sole/internal/testutil"
)

func enqueue(t *testing.T, e *env, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		err := pgdb.Tx(context.Background(), e.pool, func(tx pgx.Tx) error {
			return e.app.Claims.Outbox.Enqueue(context.Background(), tx, messaging.IssuedKey, uuid.New(), map[string]int{"n": i})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeveralRelaysDrainDisjointBatches(t *testing.T) {
	e := newEnv(t)
	enqueue(t, e, 60)
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := e.app.Claims.Outbox.PublishPending(context.Background(), e.pool, func(_ context.Context, row messaging.Row) error {
					mu.Lock()
					seen[row.ID]++
					mu.Unlock()
					time.Sleep(2 * time.Millisecond)
					return nil
				}, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != 60 {
		t.Fatalf("every row is published, got %d of 60", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Fatalf("row %d was published %d times at once: SKIP LOCKED must keep relays apart", id, times)
		}
	}
	if e.count(`SELECT count(*) FROM claims.outbox_messages WHERE sent_at IS NULL`) != 0 {
		t.Fatal("every published row is marked sent")
	}
}

func TestAFailedPublishLeavesTheRowUnsentAndTheBatchIsRetried(t *testing.T) {
	e := newEnv(t)
	enqueue(t, e, 5)
	published := 0
	_, err := e.app.Claims.Outbox.PublishPending(context.Background(), e.pool, func(context.Context, messaging.Row) error {
		published++
		if published == 3 {
			return errors.New("broker down")
		}
		return nil
	}, 10)
	if err == nil {
		t.Fatal("the failure is reported")
	}
	if e.count(`SELECT count(*) FROM claims.outbox_messages WHERE sent_at IS NULL`) != 5 {
		t.Fatal("the transaction rolled back: nothing is marked sent, so the whole batch goes out again")
	}
	n, err := e.app.Claims.Outbox.PublishPending(context.Background(), e.pool, func(context.Context, messaging.Row) error { return nil }, 10)
	if err != nil || n != 5 {
		t.Fatalf("retry: %d %v", n, err)
	}
}

func TestTrimKeepsTheInboxLongerThanTheOutbox(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	enqueue(t, e, 3)
	if _, err := e.pool.Exec(ctx, `UPDATE claims.outbox_messages SET sent_at = now() - interval '10 days' WHERE id = (SELECT min(id) FROM claims.outbox_messages)`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE claims.outbox_messages SET sent_at = now() - interval '1 day' WHERE id = (SELECT max(id) FROM claims.outbox_messages)`); err != nil {
		t.Fatal(err)
	}
	for _, age := range []string{"40 days", "10 days"} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO claims.inbox_messages (consumer, message_id, claimed_at) VALUES ('t', $1, now() - $2::interval)`, uuid.New(), age); err != nil {
			t.Fatal(err)
		}
	}
	out, in, err := e.app.Claims.Outbox.Trim(ctx, e.pool, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil || out != 1 || in != 1 {
		t.Fatalf("trimmed %d outbox and %d inbox rows (%v): sent 10 days ago goes, 1 day ago stays, unsent stays; the inbox keeps 10 days but not 40", out, in, err)
	}
	if e.count(`SELECT count(*) FROM claims.outbox_messages`) != 2 {
		t.Fatal("the unsent row and the recently sent row remain")
	}
}

func TestMigrationsApplyOnceEvenWhenReplicasStartTogether(t *testing.T) {
	pool := testutil.Pool(t)
	ctx := context.Background()
	schema := "migrate_test"
	_, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })
	files := fstest.MapFS{
		"m/0001_a.sql": {Data: []byte(`CREATE TABLE ` + schema + `.a (id int PRIMARY KEY);`)},
		"m/0002_b.sql": {Data: []byte(`CREATE TABLE ` + schema + `.b (id int PRIMARY KEY); INSERT INTO ` + schema + `.b VALUES (1);`)},
		"m/readme.txt": {Data: []byte("ignored")},
	}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pgdb.Migrate(ctx, pool, schema, files, "m"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var versions, rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.schema_migrations`).Scan(&versions)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM `+schema+`.b`).Scan(&rows)
	if versions != 2 || rows != 1 {
		t.Fatalf("two migrations recorded once and applied once: %d versions, %d rows", versions, rows)
	}
	if err := pgdb.Migrate(ctx, pool, schema, files, "m"); err != nil {
		t.Fatalf("running again is a no-op: %v", err)
	}
	broken := fstest.MapFS{"m/0003_bad.sql": {Data: []byte(`CREATE TABLE ` + schema + `.c (id int); THIS IS NOT SQL;`)}}
	if err := pgdb.Migrate(ctx, pool, schema, broken, "m"); err == nil {
		t.Fatal("a broken migration is an error")
	}
	var c *string
	_ = pool.QueryRow(ctx, `SELECT to_regclass('`+schema+`.c')::text`).Scan(&c)
	if c != nil {
		t.Fatal("a failed migration rolls back completely")
	}
	if err := pgdb.Migrate(ctx, pool, "Bad-Schema;drop", files, "m"); err == nil {
		t.Fatal("a schema name that could carry SQL is refused")
	}
}
