package postgres

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rlsvarinskis/cloud-at-home/libs/common/errs"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg"
	"github.com/rlsvarinskis/cloud-at-home/libs/common/pg/pgtest"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/domain"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store"
	"github.com/rlsvarinskis/cloud-at-home/services/vmd/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// openTestPool returns a pool on a fresh, empty database, closed when the test ends. The schema is
// not migrated.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pg.Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestPool is openTestPool followed by Migrate.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestPool(t)
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func testVM(id, project, name string) *domain.VM {
	return &domain.VM{
		ID: id, ProjectID: project, Name: name, CPUs: 1, MemoryMB: 512,
		Desired: domain.PowerStopped, State: domain.VMStopped, CreatedAt: testNow, UpdatedAt: testNow,
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestPostgres runs the shared conformance suite against a fresh database for every subtest.
func TestPostgres(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return New(newTestPool(t)) })
}

func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := openTestPool(t)

	// Several replicas starting at once: the advisory lock serialises them.
	var wg sync.WaitGroup
	errc := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errc <- Migrate(ctx, pool)
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		must(t, err)
	}

	// And again, sequentially, once the schema is current.
	must(t, Migrate(ctx, pool))

	var applied int
	must(t, pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations_vmd").Scan(&applied))
	files, err := fs.Glob(migrations, "migrations/*.up.sql")
	must(t, err)
	if applied != len(files) { // every migration applied exactly once, however many replicas raced
		t.Fatalf("schema_migrations_vmd has %d rows, want %d (one per migration file)", applied, len(files))
	}
}

func TestTxRollbackLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))
	boom := errors.New("boom")

	err := s.Tx(ctx, func(tx store.Store) error {
		must(t, tx.UpsertNode(ctx, &domain.Node{ID: "node-1", Address: "a", State: domain.NodeReady, LastHeartbeat: testNow}))
		must(t, tx.CreateImage(ctx, &domain.Image{ID: "img_1", ProjectID: "p1", Name: "i", State: domain.ImageReady, CreatedAt: testNow, UpdatedAt: testNow}))
		must(t, tx.CreateDisk(ctx, &domain.Disk{ID: "d_1", ProjectID: "p1", Name: "d", SizeGB: 1, State: domain.DiskReady, CreatedAt: testNow, UpdatedAt: testNow}))
		must(t, tx.CreateVM(ctx, testVM("vm_1", "p1", "web")))
		must(t, tx.CreateJob(ctx, &domain.Job{ID: "j_1", VMID: "vm_1", ProjectID: "p1", State: domain.JobRunning, StartedAt: testNow}))
		return boom
	})
	if err != boom {
		t.Fatalf("Tx returned %v, want %v unchanged", err, boom)
	}

	var total int
	must(t, s.db.QueryRow(ctx, `SELECT (SELECT count(*) FROM nodes) + (SELECT count(*) FROM images)
		+ (SELECT count(*) FROM disks) + (SELECT count(*) FROM vms) + (SELECT count(*) FROM jobs)`).Scan(&total))
	if total != 0 {
		t.Fatalf("%d rows survived a rolled-back transaction", total)
	}
}

func TestNestedTxSharesOneTransaction(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))
	boom := errors.New("boom")

	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.Tx(ctx, func(inner store.Store) error {
			return inner.CreateVM(ctx, testVM("vm_inner", "p1", "inner"))
		}); err != nil {
			return err
		}
		// The inner write must be visible here, which proves it ran in the same transaction.
		if _, err := tx.GetVM(ctx, "vm_inner"); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Tx returned %v, want %v", err, boom)
	}
	_, err = s.GetVM(ctx, "vm_inner")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetVM after rolled-back nested Tx: %v, want ErrNotFound", err)
	}
}

func TestDuplicateInsideTxKeepsTxUsable(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))

	must(t, s.Tx(ctx, func(tx store.Store) error {
		must(t, tx.CreateVM(ctx, testVM("vm_a", "p1", "web")))
		err := tx.CreateVM(ctx, testVM("vm_b", "p1", "web"))
		if !errors.Is(err, errs.ErrAlreadyExists) {
			t.Fatalf("duplicate name inside Tx: %v, want ErrAlreadyExists", err)
		}
		// The transaction must still work after the failed statement, as with the memory store.
		return tx.CreateVM(ctx, testVM("vm_c", "p1", "other"))
	}))

	if _, err := s.GetVM(ctx, "vm_a"); err != nil {
		t.Fatalf("vm_a: %v", err)
	}
	if _, err := s.GetVM(ctx, "vm_c"); err != nil {
		t.Fatalf("vm_c: %v", err)
	}
	if _, err := s.GetVM(ctx, "vm_b"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("vm_b: %v, want ErrNotFound", err)
	}
}

func TestConcurrentCreateSameName(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))

	const n = 2
	start := make(chan struct{})
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = s.CreateVM(ctx, testVM(fmt.Sprintf("vm_%d", i), "p1", "same"))
		}()
	}
	close(start)
	wg.Wait()

	var ok, exists int
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errs.ErrAlreadyExists):
			exists++
		default:
			t.Errorf("CreateVM error = %v, want nil or ErrAlreadyExists", err)
		}
	}
	if ok != 1 || exists != 1 {
		t.Fatalf("successes = %d, already-exists = %d; want 1 and 1", ok, exists)
	}
}

func TestClaimVMsContention(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))

	const nVMs, workers, limit = 40, 8, 5
	for i := range nVMs {
		v := testVM(fmt.Sprintf("vm_%02d", i), "p1", fmt.Sprintf("vm-%02d", i))
		v.NextReconcile = testNow.Add(-time.Duration(i) * time.Second)
		must(t, s.CreateVM(ctx, v))
	}

	var (
		mu      sync.Mutex
		claimed = map[string]string{} // VM ID -> owner that claimed it
		total   int
		wg      sync.WaitGroup
	)
	errc := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := fmt.Sprintf("r%d", w)
			for {
				got, err := s.ClaimVMs(ctx, owner, testNow, time.Minute, limit)
				if err != nil {
					errc <- err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, v := range got {
					total++
					if prev, dup := claimed[v.ID]; dup {
						t.Errorf("%s claimed by %s and %s", v.ID, prev, owner)
					}
					claimed[v.ID] = owner
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Errorf("ClaimVMs: %v", err)
	}
	if total != nVMs || len(claimed) != nVMs {
		t.Fatalf("claimed %d times over %d VMs, want %d each exactly once", total, len(claimed), nVMs)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := New(newTestPool(t))

	large := make(map[string]string, 2000)
	for i := range 2000 {
		large[fmt.Sprintf("key-%04d", i)] = strings.Repeat("v", i%50) + fmt.Sprint(i)
	}
	cases := []struct {
		name   string
		labels map[string]string
	}{
		{"nil", nil},
		{"empty", map[string]string{}},
		{"unicode", map[string]string{"ключ": "значение", "emoji": "🚀", "quote": `a"b\c`, "": "empty-key"}},
		{"large", large},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := testVM(fmt.Sprintf("vm_l%d", i), "p1", c.name)
			v.Labels = c.labels
			must(t, s.CreateVM(ctx, v))

			got, err := s.GetVM(ctx, v.ID)
			must(t, err)
			if (got.Labels == nil) != (c.labels == nil) {
				t.Fatalf("Labels nil = %v, want %v", got.Labels == nil, c.labels == nil)
			}
			if !maps.Equal(got.Labels, c.labels) {
				t.Fatalf("Labels round trip: got %d entries, want %d (contents differ)", len(got.Labels), len(c.labels))
			}
		})
	}
}
