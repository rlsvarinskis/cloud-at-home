package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"example.com/homecloud/internal/agentd/domain"
	"example.com/homecloud/internal/agentd/store"
	"example.com/homecloud/internal/agentd/store/storetest"
	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/pg"
	"example.com/homecloud/internal/pg/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// openStore returns a Store on a fresh, migrated database.
func openStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pg.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(pool), pool
}

func TestPostgres(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, _ := openStore(t)
		return s
	})
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	s, pool := openStore(t)
	for range 2 {
		if err := Migrate(ctx, pool); err != nil {
			t.Fatalf("Migrate again: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations_agentd").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("schema_migrations_agentd has %d rows, want 1", n)
	}
	// The store still works after repeated migrations.
	if err := s.CreateAgent(ctx, &domain.Agent{ID: "a", ProjectID: "p", Name: "n", VMID: "vm", State: domain.AgentActive}); err != nil {
		t.Fatalf("CreateAgent after migrate: %v", err)
	}
}

func TestConcurrentClaimsEachTaskOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	const tasks, workers = 40, 16
	for i := range tasks {
		tk := &domain.Task{ID: fmt.Sprintf("t%03d", i), AgentID: "a", ProjectID: "p", State: domain.TaskQueued}
		if err := s.CreateTask(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed = map[string]int{}
		errc    = make(chan error, workers)
	)
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("w%02d", w)
			<-start
			for {
				got, err := s.ClaimTasks(ctx, owner, now, time.Hour, 3)
				if err != nil {
					errc <- err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, tk := range got {
					claimed[tk.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Fatalf("ClaimTasks: %v", err)
	}

	if len(claimed) != tasks {
		t.Fatalf("claimed %d distinct tasks, want %d", len(claimed), tasks)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("task %s claimed %d times", id, n)
		}
	}
}

func TestConcurrentAppendsAreDense(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateTask(ctx, &domain.Task{ID: "t1", AgentID: "a", ProjectID: "p", State: domain.TaskRunning}))

	const writers, each = 16, 5
	var (
		wg           sync.WaitGroup
		mu           sync.Mutex
		stepSeqs     []int
		messageSeqs  []int
		appendErrors []error
	)
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range each {
				st := &domain.Step{TaskID: "t1", Command: fmt.Sprint("w", w, "-", i)}
				seq, err := s.AppendStep(ctx, st)
				m := &domain.Message{TaskID: "t1", JSON: []byte(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i))}
				mseq, merr := s.AppendMessage(ctx, m)
				mu.Lock()
				if err != nil || merr != nil {
					appendErrors = append(appendErrors, err, merr)
				}
				stepSeqs = append(stepSeqs, seq)
				messageSeqs = append(messageSeqs, mseq)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range appendErrors {
		if err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	want := make([]int, writers*each)
	for i := range want {
		want[i] = i + 1
	}
	slices.Sort(stepSeqs)
	slices.Sort(messageSeqs)
	if !slices.Equal(stepSeqs, want) || !slices.Equal(messageSeqs, want) {
		t.Fatalf("returned seqs: steps %v, messages %v; want 1..%d", stepSeqs, messageSeqs, len(want))
	}
	steps, err := s.ListSteps(ctx, "t1", 0, 0)
	must(err)
	msgs, err := s.ListMessages(ctx, "t1")
	must(err)
	if len(steps) != len(want) || len(msgs) != len(want) {
		t.Fatalf("stored %d steps and %d messages, want %d each", len(steps), len(msgs), len(want))
	}
	for i := range want {
		if steps[i].Seq != i+1 || msgs[i].Seq != i+1 {
			t.Fatalf("position %d: step seq %d, message seq %d", i, steps[i].Seq, msgs[i].Seq)
		}
	}
}

func TestLeaseTakeoverBetweenWorkers(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateTask(ctx, &domain.Task{ID: "t1", AgentID: "a", ProjectID: "p", State: domain.TaskQueued}))
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	ttl := 30 * time.Second

	// Worker A claims, then dies without extending its lease.
	got, err := s.ClaimTasks(ctx, "A", t0, ttl, 1)
	must(err)
	if len(got) != 1 || got[0].LeaseOwner != "A" || got[0].Attempts != 0 {
		t.Fatalf("A's claim = %+v", got)
	}
	// Worker B cannot take it while the lease is live.
	got, err = s.ClaimTasks(ctx, "B", t0.Add(ttl-time.Second), ttl, 1)
	must(err)
	if len(got) != 0 {
		t.Fatalf("B claimed a task under A's live lease")
	}
	// After expiry B takes over: attempts bumped, start time kept.
	takeover := t0.Add(ttl)
	got, err = s.ClaimTasks(ctx, "B", takeover, ttl, 1)
	must(err)
	if len(got) != 1 || got[0].LeaseOwner != "B" || got[0].Attempts != 1 || !got[0].StartedAt.Equal(t0) {
		t.Fatalf("B's takeover = %+v", got)
	}
	// A's late heartbeat is rejected, B's succeeds.
	err = s.ExtendLease(ctx, "t1", "A", takeover, ttl)
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("A ExtendLease = %v, want ErrConflict", err)
	}
	must(s.ExtendLease(ctx, "t1", "B", takeover.Add(10*time.Second), ttl))
	stored, err := s.GetTask(ctx, "t1")
	must(err)
	if stored.LeaseOwner != "B" || !stored.LeaseUntil.Equal(takeover.Add(10*time.Second+ttl)) {
		t.Fatalf("stored after takeover = owner %q until %v", stored.LeaseOwner, stored.LeaseUntil)
	}
}

func TestMessageBytesRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := openStore(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateTask(ctx, &domain.Task{ID: "t1", AgentID: "a", ProjectID: "p", State: domain.TaskRunning}))

	bodies := [][]byte{
		[]byte(`{"text":"quote \" backslash \\ newline \n tab \t unicode é 漢字 🙂"}`),
		{0x00, 0x01, 0xff, 0xfe, '\'', 0x00},              // NUL and invalid UTF-8
		[]byte("\r\n\x7f\x80\xc3"),                        // control bytes, truncated UTF-8 sequence
		[]byte{},                                          // empty
		[]byte(`{"a":1,"a":2,"b":null, "c" : [ 1 , 2 ]}`), // whitespace and duplicate keys must survive
	}
	for _, b := range bodies {
		m := &domain.Message{TaskID: "t1", JSON: slices.Clone(b), At: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
		_, err := s.AppendMessage(ctx, m)
		must(err)
	}

	got, err := s.ListMessages(ctx, "t1")
	must(err)
	if len(got) != len(bodies) {
		t.Fatalf("got %d messages, want %d", len(got), len(bodies))
	}
	for i, m := range got {
		if !bytes.Equal(m.JSON, bodies[i]) {
			t.Fatalf("message %d = %q, want %q", i+1, m.JSON, bodies[i])
		}
	}
}
