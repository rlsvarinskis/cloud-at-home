package shard_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/ids"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/store"
	"example.com/homecloud/internal/vmd/store/shard"
	"example.com/homecloud/internal/vmd/store/storetest"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// memories returns n fresh in-memory shards.
func memories(n int) []*store.Memory {
	out := make([]*store.Memory, n)
	for i := range out {
		out[i] = store.NewMemory()
	}
	return out
}

// asStores widens memories to the store.Store slice that shard.New takes.
func asStores(mems []*store.Memory) []store.Store {
	out := make([]store.Store, len(mems))
	for i, m := range mems {
		out[i] = m
	}
	return out
}

// newRouter returns a sharded store over n fresh memories.
func newRouter(t *testing.T, n int) (*shard.Store, []*store.Memory) {
	t.Helper()
	mems := memories(n)
	st, err := shard.New(asStores(mems))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st, mems
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func vm(id, project string, next time.Time) *domain.VM {
	return &domain.VM{
		ID: id, ProjectID: project, Name: id, CPUs: 1, MemoryMB: 512,
		Desired: domain.PowerStopped, State: domain.VMStopped, NextReconcile: next,
		CreatedAt: now, UpdatedAt: now,
	}
}

func image(id, project string) *domain.Image {
	return &domain.Image{ID: id, ProjectID: project, Name: id, State: domain.ImageReady, CreatedAt: now, UpdatedAt: now}
}

func disk(id, project, imageID string) *domain.Disk {
	return &domain.Disk{ID: id, ProjectID: project, Name: id, SizeGB: 10, ImageID: imageID, State: domain.DiskReady,
		CreatedAt: now, UpdatedAt: now}
}

func projectName(i int) string { return fmt.Sprintf("proj-%03d", i) }

// suiteTxProject is the project hint for every transaction the conformance suite runs; the suite uses
// project p1 throughout. The suite's Tx test also writes a node row and reads it back through the outer
// store, and node rows live only on shard 0, so p1 must hash to shard 0. newConformanceStore checks it.
const suiteTxProject = "p1"

// txHinted gives the conformance suite the project hint it has no way to supply itself. A transaction
// can span only one shard, so every row the suite writes in a transaction lands on p1's shard, and the
// suite's outer reads find those rows by probing.
type txHinted struct{ *shard.Store }

func (h txHinted) Tx(ctx context.Context, fn func(store.Store) error) error {
	return h.Store.Tx(shard.WithProject(ctx, suiteTxProject), fn)
}

// counting counts GetVM calls and forwards everything else.
type counting struct {
	store.Store
	getVM atomic.Int64
}

func (c *counting) GetVM(ctx context.Context, id string) (*domain.VM, error) {
	c.getVM.Add(1)
	return c.Store.GetVM(ctx, id)
}

// failingClaims fails every ClaimVMs call and forwards everything else.
type failingClaims struct{ store.Store }

func (failingClaims) ClaimVMs(context.Context, string, time.Time, time.Duration, int) ([]*domain.VM, error) {
	return nil, errs.Unavailable("shard down")
}

// ---- (a) conformance ----

func TestConformanceThreeShards(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		st, _ := newRouter(t, 3)
		if st.ForProject(suiteTxProject) != 0 {
			t.Fatalf("%s hashes to shard %d; the suite's node-row transactions need shard 0", suiteTxProject, st.ForProject(suiteTxProject))
		}
		return txHinted{st}
	})
}

func TestConformanceOneShard(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		st, _ := newRouter(t, 1)
		return st
	})
}

// ---- (b) distribution and stability ----

func TestDistributionWithinTolerance(t *testing.T) {
	st, _ := newRouter(t, 4)
	r := rand.New(rand.NewPCG(7, 11))
	const n = 2000
	counts := make([]int, 4)
	for i := 0; i < n; i++ {
		counts[st.ForProject(fmt.Sprintf("p-%016x", r.Uint64()))]++
	}
	even := float64(n) / 4
	for shardIdx, c := range counts {
		if dev := (float64(c) - even) / even; dev < -0.15 || dev > 0.15 {
			t.Errorf("shard %d got %d of %d projects (%.1f%% off even), want within 15%%", shardIdx, c, n, dev*100)
		}
	}
}

func TestAddingShardMovesOnlyItsShare(t *testing.T) {
	four, _ := newRouter(t, 4)
	five, _ := newRouter(t, 5)
	r := rand.New(rand.NewPCG(3, 5))
	const n = 2000
	moved := 0
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("p-%016x", r.Uint64())
		before, after := four.ForProject(p), five.ForProject(p)
		if before != after {
			moved++
			if after != 4 {
				t.Fatalf("project %q moved from shard %d to shard %d; only the new shard 4 may gain projects", p, before, after)
			}
		}
	}
	frac := float64(moved) / n
	if frac < 0.2-0.08 || frac > 0.2+0.08 {
		t.Fatalf("%.1f%% of projects moved when adding a 5th shard, want about 20%% (+-8%%)", frac*100)
	}
}

func TestPlacementDeterministic(t *testing.T) {
	a, _ := newRouter(t, 3)
	b, _ := newRouter(t, 3)
	for i := 0; i < 500; i++ {
		p := projectName(i)
		if a.ForProject(p) != b.ForProject(p) {
			t.Fatalf("ForProject(%q) differs between two stores with the same shard count", p)
		}
		if got := a.ForProject(p); got < 0 || got >= 3 {
			t.Fatalf("ForProject(%q) = %d, out of range", p, got)
		}
	}
	if a.ForProject("") != 0 {
		t.Fatalf("ForProject(\"\") = %d, want the system shard 0", a.ForProject(""))
	}
	if a.Shards() != 3 {
		t.Fatalf("Shards() = %d, want 3", a.Shards())
	}
}

func TestNewIDUsesProjectShard(t *testing.T) {
	st, _ := newRouter(t, 4)
	id := st.NewID("vm", "proj-042")
	got, ok := ids.ShardOf(id)
	if !ok || got != st.ForProject("proj-042") {
		t.Fatalf("NewID shard suffix = %d, %v; want ForProject = %d", got, ok, st.ForProject("proj-042"))
	}
}

// ---- (c) placement and routing ----

func TestRowsLandOnlyOnTheirProjectShard(t *testing.T) {
	st, mems := newRouter(t, 3)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		p := projectName(i)
		must(t, st.CreateVM(ctx, vm(st.NewID("vm", p), p, time.Time{})))
		must(t, st.CreateDisk(ctx, disk(st.NewID("disk", p), p, "")))
		must(t, st.CreateJob(ctx, &domain.Job{ID: st.NewID("job", p), VMID: "vm_x", ProjectID: p, State: domain.JobRunning, StartedAt: now}))
	}
	for i := 0; i < 30; i++ {
		p := projectName(i)
		home := st.ForProject(p)
		for shardIdx, m := range mems {
			vms, err := m.ListVMs(ctx, domain.VMFilter{ProjectID: p, Limit: 100})
			must(t, err)
			disks, err := m.ListDisks(ctx, p, "", 100)
			must(t, err)
			want := 0
			if shardIdx == home {
				want = 1
			}
			if len(vms) != want || len(disks) != want {
				t.Fatalf("project %s on shard %d: %d VMs, %d disks; want %d each (home %d)", p, shardIdx, len(vms), len(disks), want, home)
			}
		}
	}
}

func TestShardedIDsRouteWithoutProbing(t *testing.T) {
	mems := memories(3)
	counters := make([]*counting, 3)
	wrapped := make([]store.Store, 3)
	for i, m := range mems {
		counters[i] = &counting{Store: m}
		wrapped[i] = counters[i]
	}
	st, err := shard.New(wrapped)
	must(t, err)
	ctx := context.Background()

	id := st.NewID("vm", "proj-007")
	must(t, st.CreateVM(ctx, vm(id, "proj-007", time.Time{})))
	for _, c := range counters {
		c.getVM.Store(0)
	}
	got, err := st.GetVM(ctx, id)
	must(t, err)
	if got.ID != id {
		t.Fatalf("GetVM(%q).ID = %q", id, got.ID)
	}
	for i, c := range counters {
		want := int64(0)
		if i == st.ForProject("proj-007") {
			want = 1
		}
		if n := c.getVM.Load(); n != want {
			t.Fatalf("shard %d received %d GetVM calls for a sharded ID, want %d", i, n, want)
		}
	}
}

func TestUnsuffixedIDsAreProbed(t *testing.T) {
	mems := memories(3)
	counters := make([]*counting, 3)
	wrapped := make([]store.Store, 3)
	for i, m := range mems {
		counters[i] = &counting{Store: m}
		wrapped[i] = counters[i]
	}
	st, err := shard.New(wrapped)
	must(t, err)
	ctx := context.Background()

	// A legacy ID with no suffix is created on its project's shard but is found by probing.
	must(t, st.CreateVM(ctx, vm("vm_legacy", "proj-007", time.Time{})))
	home := st.ForProject("proj-007")
	for _, c := range counters {
		c.getVM.Store(0)
	}
	got, err := st.GetVM(ctx, "vm_legacy")
	must(t, err)
	if got.ProjectID != "proj-007" {
		t.Fatalf("GetVM(vm_legacy) = %+v", got)
	}
	var total int64
	for _, c := range counters {
		total += c.getVM.Load()
	}
	if total < 1 || total > 3 {
		t.Fatalf("probing made %d GetVM calls, want 1..3", total)
	}
	if counters[home].getVM.Load() < 1 {
		t.Fatalf("probe did not try the home shard %d first", home)
	}

	// A missing unsuffixed ID is NotFound only after every shard was tried.
	for _, c := range counters {
		c.getVM.Store(0)
	}
	_, err = st.GetVM(ctx, "vm_absent")
	if !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetVM(vm_absent) error = %v, want NotFound", err)
	}
	for i, c := range counters {
		if c.getVM.Load() != 1 {
			t.Fatalf("shard %d probed %d times for a missing ID, want 1", i, c.getVM.Load())
		}
	}

	// Creating the same unsuffixed ID under another project is rejected.
	err = st.CreateVM(ctx, vm("vm_legacy", "proj-900", time.Time{}))
	if !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("duplicate unsuffixed create error = %v, want AlreadyExists", err)
	}
}

func TestUpdateAndDeleteFindTheHoldingShard(t *testing.T) {
	st, _ := newRouter(t, 3)
	ctx := context.Background()
	must(t, st.CreateVM(ctx, vm("vm_upd", "proj-011", time.Time{})))
	got, err := st.GetVM(ctx, "vm_upd")
	must(t, err)
	got.CPUs = 4
	must(t, st.UpdateVM(ctx, got))
	must(t, st.DeleteVM(ctx, "vm_upd"))
	if _, err := st.GetVM(ctx, "vm_upd"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("GetVM after delete = %v, want NotFound", err)
	}
	if err := st.DeleteVM(ctx, "vm_upd"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("second DeleteVM = %v, want NotFound", err)
	}
}

// ---- (d) images: shared rows and merged pagination ----

func TestSharedImagesLiveOnShardZero(t *testing.T) {
	st, mems := newRouter(t, 3)
	ctx := context.Background()
	shared := st.NewID("img", "")
	must(t, st.CreateImage(ctx, image(shared, "")))
	if _, err := mems[0].GetImage(ctx, shared); err != nil {
		t.Fatalf("shared image not on shard 0: %v", err)
	}
	for i := 1; i < 3; i++ {
		if _, err := mems[i].GetImage(ctx, shared); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("shared image also on shard %d: %v", i, err)
		}
	}

	for i := 0; i < 6; i++ {
		p := projectName(i)
		must(t, st.CreateImage(ctx, image(st.NewID("img", p), p)))
	}
	for i := 0; i < 6; i++ {
		p := projectName(i)
		got, err := st.ListImages(ctx, p, true, "", 0)
		must(t, err)
		if len(got) != 2 || !slices.ContainsFunc(got, func(im *domain.Image) bool { return im.ID == shared }) {
			t.Fatalf("ListImages(%s, shared) = %d images, want own image plus the shared one", p, len(got))
		}
		own, err := st.ListImages(ctx, p, false, "", 0)
		must(t, err)
		if len(own) != 1 {
			t.Fatalf("ListImages(%s, no shared) = %d images, want 1", p, len(own))
		}
	}
	sharedOnly, err := st.ListImages(ctx, "", true, "", 0)
	must(t, err)
	if len(sharedOnly) != 1 || sharedOnly[0].ID != shared {
		t.Fatalf("ListImages(\"\", shared) = %v, want only the shared image", sharedOnly)
	}
}

func TestImagePaginationMergesAcrossShards(t *testing.T) {
	st, _ := newRouter(t, 3)
	ctx := context.Background()
	var want []string
	for i := 0; i < 4; i++ {
		id := st.NewID("img", "proj-003")
		must(t, st.CreateImage(ctx, image(id, "proj-003")))
		want = append(want, id)
	}
	for i := 0; i < 3; i++ {
		id := st.NewID("img", "")
		must(t, st.CreateImage(ctx, image(id, "")))
		want = append(want, id)
	}
	for i := 0; i < 5; i++ {
		must(t, st.CreateImage(ctx, image(st.NewID("img", projectName(i+10)), projectName(i+10))))
	}
	sort.Strings(want)

	var got []string
	cursor := ""
	for page := 0; page < 100; page++ {
		items, err := st.ListImages(ctx, "proj-003", true, cursor, 2)
		must(t, err)
		if len(items) > 2 {
			t.Fatalf("page has %d items, limit 2", len(items))
		}
		if len(items) == 0 {
			break
		}
		for _, im := range items {
			got = append(got, im.ID)
		}
		cursor = items[len(items)-1].ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paginated images = %v\nwant %v", got, want)
	}
}

// ---- fan-out operations ----

func TestFanOutCountsAndLists(t *testing.T) {
	st, _ := newRouter(t, 3)
	ctx := context.Background()
	must(t, st.CreateImage(ctx, image("img_base", "")))
	for i := 0; i < 6; i++ {
		p := projectName(i)
		must(t, st.CreateDisk(ctx, disk(st.NewID("disk", p), p, "img_base")))
		v := vm(st.NewID("vm", p), p, time.Time{})
		v.NodeID = "node-1"
		must(t, st.CreateVM(ctx, v))
	}
	n, err := st.CountDisksUsingImage(ctx, "img_base")
	must(t, err)
	if n != 6 {
		t.Fatalf("CountDisksUsingImage = %d, want 6", n)
	}
	ready, err := st.ListDisksByState(ctx, domain.DiskReady, 0)
	must(t, err)
	if len(ready) != 6 || !sort.SliceIsSorted(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID }) {
		t.Fatalf("ListDisksByState returned %d disks, want 6 sorted by ID", len(ready))
	}
	onNode, err := st.ListVMsByNode(ctx, "node-1")
	must(t, err)
	if len(onNode) != 6 {
		t.Fatalf("ListVMsByNode = %d VMs, want 6", len(onNode))
	}
	capped, err := st.ListDisksByState(ctx, domain.DiskReady, 2)
	must(t, err)
	if len(capped) != 2 {
		t.Fatalf("ListDisksByState limit 2 returned %d", len(capped))
	}
}

func TestListJobsPaginatesAcrossShards(t *testing.T) {
	st, _ := newRouter(t, 3)
	ctx := context.Background()
	var want []string
	for i := 0; i < 8; i++ {
		p := projectName(i)
		id := st.NewID("job", p)
		want = append(want, id)
		must(t, st.CreateJob(ctx, &domain.Job{ID: id, VMID: "vm_shared_job", ProjectID: p, State: domain.JobRunning, StartedAt: now}))
	}
	sort.Strings(want)
	var got []string
	cursor := ""
	for {
		page, err := st.ListJobs(ctx, "vm_shared_job", cursor, 3)
		must(t, err)
		if len(page) == 0 {
			break
		}
		for _, j := range page {
			got = append(got, j.ID)
		}
		cursor = page[len(page)-1].ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListJobs pages = %v\nwant %v", got, want)
	}
	running, err := st.ListRunningJobs(ctx, "vm_shared_job")
	must(t, err)
	if len(running) != 8 {
		t.Fatalf("ListRunningJobs = %d, want 8", len(running))
	}
}

// ---- (e) ClaimVMs ----

// seedDue creates two due VMs in each of projects 0..n-1 and returns the claimable ID count.
func seedDue(t *testing.T, st *shard.Store, n int) int {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		p := projectName(i)
		for j := 0; j < 2; j++ {
			must(t, st.CreateVM(ctx, vm(st.NewID("vm", p), p, now)))
		}
	}
	return 2 * n
}

func TestClaimVMsSpansEveryShard(t *testing.T) {
	st, _ := newRouter(t, 3)
	total := seedDue(t, st, 30)

	seen := map[int]bool{}
	for i := 0; i < 30; i++ {
		seen[st.ForProject(projectName(i))] = true
	}
	if len(seen) != 3 {
		t.Fatalf("test data reaches %d shards, want 3", len(seen))
	}

	ctx := context.Background()
	first, err := st.ClaimVMs(ctx, "r1", now, time.Minute, 7)
	must(t, err)
	if len(first) != 7 {
		t.Fatalf("limit 7 claimed %d VMs", len(first))
	}
	claimedShards := map[int]bool{}
	for _, v := range first {
		claimedShards[st.ForProject(v.ProjectID)] = true
	}
	if !sort.SliceIsSorted(first, func(i, j int) bool { return first[i].ID < first[j].ID }) {
		t.Fatal("claimed VMs with equal NextReconcile are not ordered by ID")
	}

	rest, err := st.ClaimVMs(ctx, "r2", now, time.Minute, 0)
	must(t, err)
	if len(rest) != total-7 {
		t.Fatalf("second claim got %d VMs, want %d", len(rest), total-7)
	}
	ids := map[string]bool{}
	for _, v := range append(first, rest...) {
		if ids[v.ID] {
			t.Fatalf("VM %s claimed twice", v.ID)
		}
		ids[v.ID] = true
	}
	again, err := st.ClaimVMs(ctx, "r3", now, time.Minute, 0)
	must(t, err)
	if len(again) != 0 {
		t.Fatalf("claims within lease returned %d VMs", len(again))
	}
}

func TestClaimVMsRotatesStartShard(t *testing.T) {
	st, _ := newRouter(t, 3)
	seedDue(t, st, 30)
	ctx := context.Background()
	shards := map[int]bool{}
	for i := 0; i < 3; i++ {
		got, err := st.ClaimVMs(ctx, "r", now, time.Minute, 1)
		must(t, err)
		if len(got) != 1 {
			t.Fatalf("claim %d returned %d VMs", i, len(got))
		}
		shards[st.ForProject(got[0].ProjectID)] = true
	}
	if len(shards) != 3 {
		t.Fatalf("three single-VM claims hit %d shards, want 3 (no shard starves)", len(shards))
	}
}

func TestClaimVMsSurfacesShardError(t *testing.T) {
	mems := memories(2)
	st, err := shard.New([]store.Store{mems[0], failingClaims{mems[1]}})
	must(t, err)
	ctx := context.Background()
	must(t, st.CreateVM(ctx, vm("vm_ok", projectName(0), now)))
	_, err = st.ClaimVMs(ctx, "r", now, time.Minute, 0)
	if !errors.Is(err, errs.ErrUnavailable) {
		t.Fatalf("ClaimVMs error = %v, want the failing shard's Unavailable", err)
	}
}

func TestClaimVMsConcurrentNoDoubleClaim(t *testing.T) {
	st, _ := newRouter(t, 3)
	total := seedDue(t, st, 30)
	ctx := context.Background()

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := st.ClaimVMs(ctx, "worker", now, time.Hour, 3)
				if err != nil {
					t.Errorf("ClaimVMs: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, v := range got {
					claimed[v.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != total {
		t.Fatalf("claimed %d distinct VMs, want %d", len(claimed), total)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("VM %s claimed %d times", id, n)
		}
	}
}

// ---- (f) transactions ----

func TestTxRoutesByProjectHint(t *testing.T) {
	st, mems := newRouter(t, 3)
	ctx := context.Background()
	home := st.ForProject("proj-005")
	err := st.Tx(shard.WithProject(ctx, "proj-005"), func(tx store.Store) error {
		if err := tx.CreateVM(ctx, vm("vm_tx_a", "proj-005", time.Time{})); err != nil {
			return err
		}
		return tx.CreateVM(ctx, vm("vm_tx_b", "proj-005", time.Time{}))
	})
	must(t, err)
	for i, m := range mems {
		list, err := m.ListVMs(ctx, domain.VMFilter{ProjectID: "proj-005", Limit: 10})
		must(t, err)
		want := 0
		if i == home {
			want = 2
		}
		if len(list) != want {
			t.Fatalf("shard %d holds %d VMs of proj-005 after Tx, want %d", i, len(list), want)
		}
	}
	got, err := st.GetVM(ctx, "vm_tx_b")
	must(t, err)
	if got.ProjectID != "proj-005" {
		t.Fatalf("committed VM = %+v", got)
	}
}

func TestTxWithoutHintNeedsOneShard(t *testing.T) {
	st, _ := newRouter(t, 3)
	called := false
	err := st.Tx(context.Background(), func(store.Store) error {
		called = true
		return nil
	})
	if !errors.Is(err, errs.ErrInvalidArgument) {
		t.Fatalf("Tx without hint error = %v, want InvalidArgument", err)
	}
	if called {
		t.Fatal("fn ran although Tx had no shard to use")
	}

	one, _ := newRouter(t, 1)
	must(t, one.Tx(context.Background(), func(store.Store) error { return nil }))
}

func TestTxRollbackIsAtomicWithinShard(t *testing.T) {
	st, _ := newRouter(t, 3)
	ctx := context.Background()
	must(t, st.CreateVM(ctx, vm("vm_keep", "proj-002", time.Time{})))
	boom := errors.New("boom")
	err := st.Tx(shard.WithProject(ctx, "proj-002"), func(tx store.Store) error {
		if err := tx.CreateVM(ctx, vm("vm_gone", "proj-002", time.Time{})); err != nil {
			return err
		}
		v, err := tx.GetVM(ctx, "vm_keep")
		if err != nil {
			return err
		}
		v.CPUs = 8
		if err := tx.UpdateVM(ctx, v); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Tx error = %v, want the callback's error unchanged", err)
	}
	if _, err := st.GetVM(ctx, "vm_gone"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("rolled-back VM visible: %v", err)
	}
	kept, err := st.GetVM(ctx, "vm_keep")
	must(t, err)
	if kept.CPUs != 1 {
		t.Fatalf("CPUs = %d after rollback, want 1", kept.CPUs)
	}
}

// ---- (g) constructor ----

func TestNewRejectsInvalidShardCounts(t *testing.T) {
	if _, err := shard.New(nil); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("New(nil) error = %v, want InvalidArgument", err)
	}
	if _, err := shard.New([]store.Store{}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("New(empty) error = %v, want InvalidArgument", err)
	}
	if _, err := shard.New(asStores(memories(257))); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("New(257 shards) error = %v, want InvalidArgument", err)
	}
	if _, err := shard.New([]store.Store{store.NewMemory(), nil}); !errors.Is(err, errs.ErrInvalidArgument) {
		t.Errorf("New with nil shard error = %v, want InvalidArgument", err)
	}
	st, err := shard.New(asStores(memories(256)))
	must(t, err)
	if st.Shards() != 256 {
		t.Errorf("Shards() = %d, want 256", st.Shards())
	}
}
