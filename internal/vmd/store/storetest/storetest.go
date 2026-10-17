// Package storetest is a reusable conformance suite for store.Store. Every implementation (memory,
// postgres) runs it from its own tests via Run, so all implementations are held to the same contract
// documented in package store.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"example.com/homecloud/internal/errs"
	"example.com/homecloud/internal/vmd/domain"
	"example.com/homecloud/internal/vmd/store"
)

// fixed is the timestamp used for every object the suite builds.
var fixed = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// leaseTTL is the lease duration used by the ClaimVMs tests.
const leaseTTL = 30 * time.Second

// Run executes the conformance suite against stores produced by newStore. newStore must return a
// fresh, empty store on every call; each subtest calls it once so subtests never observe each
// other's writes.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"Nodes/UpsertAndGet", testNodesUpsertAndGet},
		{"Nodes/ListSorted", testNodesListSorted},

		{"Images/CreateAndGet", testImagesCreateAndGet},
		{"Images/DuplicateNameScopedToProject", testImagesDuplicateName},
		{"Images/ListSharedVisibility", testImagesListSharedVisibility},
		{"Images/PaginationKeyset", testImagesPagination},
		{"Images/UpdateCAS", testImagesUpdateCAS},
		{"Images/Delete", testImagesDelete},
		{"Images/EmptyProjectSelectsOnlySharedImages", testImagesEmptyProject},
		{"Images/ListByState", testImagesListByState},

		{"Disks/CreateAndGet", testDisksCreateAndGet},
		{"Disks/ListAndListByState", testDisksListAndListByState},
		{"Disks/UpdateCAS", testDisksUpdateCAS},
		{"Disks/Delete", testDisksDelete},
		{"Disks/CountUsingImage", testDisksCountUsingImage},

		{"VMs/CreateAndGet", testVMsCreateAndGet},
		{"VMs/DuplicateName", testVMsDuplicateName},
		{"VMs/IdempotencyKey", testVMsIdempotencyKey},
		{"VMs/ListLabelsAndPagination", testVMsListLabelsAndPagination},
		{"VMs/ListByNode", testVMsListByNode},
		{"VMs/UpdateCAS", testVMsUpdateCAS},
		{"VMs/Delete", testVMsDelete},

		{"ClaimVMs/DueOrderedAndLeased", testClaimDueOrderedAndLeased},
		{"ClaimVMs/ExclusiveWithinLease", testClaimExclusive},
		{"ClaimVMs/BumpsVersion", testClaimBumpsVersion},
		{"ClaimVMs/Limit", testClaimLimit},
		{"ClaimVMs/ExpiredLeaseReclaimable", testClaimExpiredLease},
		{"ClaimVMs/ZeroNextReconcileNeverClaimed", testClaimZeroNextReconcile},

		{"Isolation/VM", testIsolationVM},
		{"Isolation/Node", testIsolationNode},
		{"Isolation/JobExitCode", testIsolationJobExitCode},

		{"Jobs/CreateAndGet", testJobsCreateAndGet},
		{"Jobs/IdempotencyKey", testJobsIdempotencyKey},
		{"Jobs/UpdateCAS", testJobsUpdateCAS},
		{"Jobs/ListPaginated", testJobsListPaginated},
		{"Jobs/ListRunning", testJobsListRunning},

		{"Usage", testUsage},

		{"Tx/CommitMakesWritesVisible", testTxCommit},
		{"Tx/ErrorDiscardsAllWrites", testTxRollback},
		{"Tx/ReadsSeeOwnWrites", testTxReadsOwnWrites},

		{"Concurrency/UpdateVMSingleWinner", testConcurrentUpdateVM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, newStore(t))
		})
	}
}

// ---- builders ----

func newNode(id string) *domain.Node {
	return &domain.Node{
		ID: id, Address: id + ".example:9000", State: domain.NodeReady,
		CPUs: 8, MemoryMB: 16384, DiskBytesFree: 1 << 40, LastHeartbeat: fixed,
	}
}

func newImage(id, project, name string) *domain.Image {
	return &domain.Image{
		ID: id, ProjectID: project, Name: name, State: domain.ImageReady,
		SizeBytes: 1 << 30, CreatedAt: fixed, UpdatedAt: fixed,
	}
}

func newDisk(id, project, name string, sizeGB int) *domain.Disk {
	return &domain.Disk{
		ID: id, ProjectID: project, Name: name, SizeGB: sizeGB, State: domain.DiskReady,
		CreatedAt: fixed, UpdatedAt: fixed,
	}
}

func newVM(id, project, name string) *domain.VM {
	return &domain.VM{
		ID: id, ProjectID: project, Name: name, CPUs: 2, MemoryMB: 1024,
		Desired: domain.PowerStopped, State: domain.VMStopped, CreatedAt: fixed, UpdatedAt: fixed,
	}
}

func newJob(id, vmID, project string) *domain.Job {
	return &domain.Job{
		ID: id, VMID: vmID, ProjectID: project, Command: "echo hi", Workdir: "/",
		State: domain.JobRunning, TimeoutSeconds: 60, StartedAt: fixed,
	}
}

// ---- id accessors for wantIDs and pageIDs ----

func nodeID(n *domain.Node) string   { return n.ID }
func imageID(i *domain.Image) string { return i.ID }
func diskID(d *domain.Disk) string   { return d.ID }
func vmID(v *domain.VM) string       { return v.ID }
func jobID(j *domain.Job) string     { return j.ID }

// ---- assertion helpers ----

// must fails the test immediately if err is non-nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// wantErr fails the test immediately unless errors.Is(err, target).
func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want errors.Is(%v)", err, target)
	}
}

// wantIDs fails the test immediately unless got holds exactly the IDs in want, in that order.
func wantIDs[T any](t *testing.T, got []T, id func(T) string, want ...string) {
	t.Helper()
	gotIDs := make([]string, 0, len(got))
	for _, x := range got {
		gotIDs = append(gotIDs, id(x))
	}
	if !slices.Equal(gotIDs, want) {
		t.Fatalf("ids = %v, want %v", gotIDs, want)
	}
}

// pageIDs walks every page of the keyset-paginated list that fetch reads, returning the IDs in the
// order they were seen. It checks that every page respects limit and that each item sorts strictly
// after the cursor it was fetched with.
func pageIDs[T any](t *testing.T, limit int, id func(T) string, fetch func(after string) ([]T, error)) []string {
	t.Helper()
	var ids []string
	cursor := ""
	for range 1000 {
		page, err := fetch(cursor)
		must(t, err)
		if len(page) > limit {
			t.Fatalf("page has %d items, limit is %d", len(page), limit)
		}
		if len(page) == 0 {
			return ids
		}
		for _, x := range page {
			if cursor != "" && id(x) <= cursor {
				t.Fatalf("item %q returned after cursor %q", id(x), cursor)
			}
			ids = append(ids, id(x))
		}
		cursor = id(page[len(page)-1])
	}
	t.Fatal("pagination did not terminate")
	return nil
}

// ---- nodes ----

func testNodesUpsertAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()

	_, err := s.GetNode(ctx, "node-x")
	wantErr(t, err, errs.ErrNotFound)

	n := newNode("node-x")
	must(t, s.UpsertNode(ctx, n))
	got, err := s.GetNode(ctx, "node-x")
	must(t, err)
	if got.Address != n.Address || got.State != domain.NodeReady || got.CPUs != 8 ||
		got.MemoryMB != 16384 || got.DiskBytesFree != 1<<40 || !got.LastHeartbeat.Equal(fixed) {
		t.Fatalf("GetNode after insert = %+v", got)
	}

	// Upsert on an existing ID updates it rather than failing.
	n.State = domain.NodeDraining
	n.Address = "192.0.2.9:9000"
	must(t, s.UpsertNode(ctx, n))
	got, err = s.GetNode(ctx, "node-x")
	must(t, err)
	if got.State != domain.NodeDraining || got.Address != "192.0.2.9:9000" {
		t.Fatalf("GetNode after update = %+v", got)
	}

	// Isolation: changing the caller's object after upsert must not change the stored node.
	n.CPUs = 99
	got, err = s.GetNode(ctx, "node-x")
	must(t, err)
	if got.CPUs != 8 {
		t.Fatalf("stored CPUs = %d after mutating the upserted object, want 8", got.CPUs)
	}
}

func testNodesListSorted(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, id := range []string{"node-b", "node-c", "node-a"} {
		must(t, s.UpsertNode(ctx, newNode(id)))
	}
	must(t, s.UpsertNode(ctx, newNode("node-a"))) // re-upsert must not duplicate
	got, err := s.ListNodes(ctx)
	must(t, err)
	wantIDs(t, got, nodeID, "node-a", "node-b", "node-c")
}

// ---- images ----

func testImagesCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_1", "p1", "ubuntu")))

	got, err := s.GetImage(ctx, "img_1")
	must(t, err)
	if got.ProjectID != "p1" || got.Name != "ubuntu" || got.State != domain.ImageReady {
		t.Fatalf("GetImage = %+v", got)
	}

	err = s.CreateImage(ctx, newImage("img_1", "p1", "other"))
	wantErr(t, err, errs.ErrAlreadyExists)

	_, err = s.GetImage(ctx, "img_missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testImagesDuplicateName(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_1", "p1", "base")))

	wantErr(t, s.CreateImage(ctx, newImage("img_2", "p1", "base")), errs.ErrAlreadyExists)
	must(t, s.CreateImage(ctx, newImage("img_3", "p2", "base")))
}

func testImagesListSharedVisibility(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_a", "p1", "a")))
	must(t, s.CreateImage(ctx, newImage("img_b", "p1", "b")))
	must(t, s.CreateImage(ctx, newImage("img_s", "", "shared-base")))
	must(t, s.CreateImage(ctx, newImage("img_c", "p2", "c")))

	got, err := s.ListImages(ctx, "p1", false, "", 100)
	must(t, err)
	wantIDs(t, got, imageID, "img_a", "img_b")

	got, err = s.ListImages(ctx, "p1", true, "", 100)
	must(t, err)
	wantIDs(t, got, imageID, "img_a", "img_b", "img_s")

	got, err = s.ListImages(ctx, "p2", false, "", 100)
	must(t, err)
	wantIDs(t, got, imageID, "img_c")

	got, err = s.ListImages(ctx, "p2", true, "", 100)
	must(t, err)
	wantIDs(t, got, imageID, "img_c", "img_s")
}

func testImagesPagination(t *testing.T, s store.Store) {
	ctx := context.Background()
	var want []string
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("img_%02d", i)
		must(t, s.CreateImage(ctx, newImage(id, "p1", "name-"+id)))
		want = append(want, id)
	}
	must(t, s.CreateImage(ctx, newImage("img_zz", "p2", "other-project")))
	must(t, s.CreateImage(ctx, newImage("img_04a", "", "shared-1")))

	// Project only: 7 items, every one exactly once, in ID order.
	ids := pageIDs(t, 3, imageID, func(after string) ([]*domain.Image, error) {
		return s.ListImages(ctx, "p1", false, after, 3)
	})
	if !slices.Equal(ids, want) {
		t.Fatalf("paginated project images = %v, want %v", ids, want)
	}

	// Including shared: the shared image sorts into its ID position.
	want = []string{"img_01", "img_02", "img_03", "img_04", "img_04a", "img_05", "img_06", "img_07"}
	ids = pageIDs(t, 3, imageID, func(after string) ([]*domain.Image, error) {
		return s.ListImages(ctx, "p1", true, after, 3)
	})
	if !slices.Equal(ids, want) {
		t.Fatalf("paginated images with shared = %v, want %v", ids, want)
	}
}

func testImagesUpdateCAS(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_1", "p1", "u")))

	got, err := s.GetImage(ctx, "img_1")
	must(t, err)
	v0 := got.Version
	got.State = domain.ImageError
	got.StateReason = "boom"
	must(t, s.UpdateImage(ctx, got))
	if got.Version != v0+1 {
		t.Fatalf("Version after UpdateImage = %d, want %d (incremented in place)", got.Version, v0+1)
	}
	stored, err := s.GetImage(ctx, "img_1")
	must(t, err)
	if stored.State != domain.ImageError || stored.StateReason != "boom" || stored.Version != got.Version {
		t.Fatalf("stored image = %+v", stored)
	}

	// Two copies at the same version: the second write must conflict.
	a, err := s.GetImage(ctx, "img_1")
	must(t, err)
	b, err := s.GetImage(ctx, "img_1")
	must(t, err)
	a.StateReason = "from a"
	must(t, s.UpdateImage(ctx, a))
	b.StateReason = "from b"
	wantErr(t, s.UpdateImage(ctx, b), errs.ErrConflict)
	stored, err = s.GetImage(ctx, "img_1")
	must(t, err)
	if stored.StateReason != "from a" {
		t.Fatalf("StateReason = %q after conflicting write, want %q", stored.StateReason, "from a")
	}

	wantErr(t, s.UpdateImage(ctx, &domain.Image{ID: "img_nope", Version: 1}), errs.ErrNotFound)
}

func testImagesDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_1", "p1", "d")))
	must(t, s.DeleteImage(ctx, "img_1"))

	_, err := s.GetImage(ctx, "img_1")
	wantErr(t, err, errs.ErrNotFound)
	got, err := s.ListImages(ctx, "p1", false, "", 100)
	must(t, err)
	wantIDs(t, got, imageID)

	wantErr(t, s.DeleteImage(ctx, "img_1"), errs.ErrNotFound)
}

// ---- disks ----

func testDisksCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateDisk(ctx, newDisk("d_1", "p1", "data", 10)))

	got, err := s.GetDisk(ctx, "d_1")
	must(t, err)
	if got.ProjectID != "p1" || got.Name != "data" || got.SizeGB != 10 || got.State != domain.DiskReady {
		t.Fatalf("GetDisk = %+v", got)
	}

	wantErr(t, s.CreateDisk(ctx, newDisk("d_1", "p1", "other", 1)), errs.ErrAlreadyExists)
	wantErr(t, s.CreateDisk(ctx, newDisk("d_2", "p1", "data", 1)), errs.ErrAlreadyExists)
	must(t, s.CreateDisk(ctx, newDisk("d_3", "p2", "data", 1)))

	_, err = s.GetDisk(ctx, "d_missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testDisksListAndListByState(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, project string, st domain.DiskState) {
		d := newDisk(id, project, id, 5)
		d.State = st
		must(t, s.CreateDisk(ctx, d))
	}
	mk("d_1", "p1", domain.DiskReady)
	mk("d_2", "p1", domain.DiskCreating)
	mk("d_3", "p1", domain.DiskReady)
	mk("d_4", "p2", domain.DiskReady)
	mk("d_5", "p1", domain.DiskError)

	got, err := s.ListDisks(ctx, "p1", "", 100)
	must(t, err)
	wantIDs(t, got, diskID, "d_1", "d_2", "d_3", "d_5")

	got, err = s.ListDisks(ctx, "p1", "d_1", 100)
	must(t, err)
	wantIDs(t, got, diskID, "d_2", "d_3", "d_5")

	ids := pageIDs(t, 2, diskID, func(after string) ([]*domain.Disk, error) {
		return s.ListDisks(ctx, "p1", after, 2)
	})
	if !slices.Equal(ids, []string{"d_1", "d_2", "d_3", "d_5"}) {
		t.Fatalf("paginated disks = %v", ids)
	}

	got, err = s.ListDisksByState(ctx, domain.DiskReady, 100)
	must(t, err)
	wantIDs(t, got, diskID, "d_1", "d_3", "d_4")

	got, err = s.ListDisksByState(ctx, domain.DiskReady, 2)
	must(t, err)
	wantIDs(t, got, diskID, "d_1", "d_3")

	got, err = s.ListDisksByState(ctx, domain.DiskCreating, 100)
	must(t, err)
	wantIDs(t, got, diskID, "d_2")

	got, err = s.ListDisksByState(ctx, domain.DiskDeleting, 100)
	must(t, err)
	wantIDs(t, got, diskID)
}

func testDisksUpdateCAS(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateDisk(ctx, newDisk("d_1", "p1", "u", 10)))

	got, err := s.GetDisk(ctx, "d_1")
	must(t, err)
	v0 := got.Version
	got.State = domain.DiskError
	got.StateReason = "boom"
	must(t, s.UpdateDisk(ctx, got))
	if got.Version != v0+1 {
		t.Fatalf("Version after UpdateDisk = %d, want %d", got.Version, v0+1)
	}
	stored, err := s.GetDisk(ctx, "d_1")
	must(t, err)
	if stored.State != domain.DiskError || stored.StateReason != "boom" || stored.Version != got.Version {
		t.Fatalf("stored disk = %+v", stored)
	}

	a, err := s.GetDisk(ctx, "d_1")
	must(t, err)
	b, err := s.GetDisk(ctx, "d_1")
	must(t, err)
	a.AttachedVMID = "vm_a"
	must(t, s.UpdateDisk(ctx, a))
	b.AttachedVMID = "vm_b"
	wantErr(t, s.UpdateDisk(ctx, b), errs.ErrConflict)
	stored, err = s.GetDisk(ctx, "d_1")
	must(t, err)
	if stored.AttachedVMID != "vm_a" {
		t.Fatalf("AttachedVMID = %q after conflicting write, want vm_a", stored.AttachedVMID)
	}

	wantErr(t, s.UpdateDisk(ctx, &domain.Disk{ID: "d_nope", Version: 1}), errs.ErrNotFound)
}

func testDisksDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateDisk(ctx, newDisk("d_1", "p1", "d", 1)))
	must(t, s.DeleteDisk(ctx, "d_1"))

	_, err := s.GetDisk(ctx, "d_1")
	wantErr(t, err, errs.ErrNotFound)
	got, err := s.ListDisks(ctx, "p1", "", 100)
	must(t, err)
	wantIDs(t, got, diskID)

	wantErr(t, s.DeleteDisk(ctx, "d_1"), errs.ErrNotFound)
}

// ---- VMs ----

func testVMsCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	v := newVM("vm_1", "p1", "web")
	v.Labels = map[string]string{"env": "prod"}
	must(t, s.CreateVM(ctx, v))

	got, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	if got.ProjectID != "p1" || got.Name != "web" || got.CPUs != 2 || got.MemoryMB != 1024 {
		t.Fatalf("GetVM = %+v", got)
	}
	if got.Version != 1 {
		t.Fatalf("Version of a new VM = %d, want 1", got.Version)
	}
	if got.Labels["env"] != "prod" {
		t.Fatalf("Labels = %v, want env=prod", got.Labels)
	}

	wantErr(t, s.CreateVM(ctx, newVM("vm_1", "p1", "other")), errs.ErrAlreadyExists)
	_, err = s.GetVM(ctx, "vm_missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testVMsDuplicateName(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateVM(ctx, newVM("vm_1", "p1", "web")))

	wantErr(t, s.CreateVM(ctx, newVM("vm_2", "p1", "web")), errs.ErrAlreadyExists)
	must(t, s.CreateVM(ctx, newVM("vm_3", "p2", "web")))
}

func testVMsIdempotencyKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	withKey := func(id, project, name, key string) *domain.VM {
		v := newVM(id, project, name)
		v.IdempotencyKey = key
		return v
	}
	must(t, s.CreateVM(ctx, withKey("vm_1", "p1", "a", "k-1")))
	wantErr(t, s.CreateVM(ctx, withKey("vm_2", "p1", "b", "k-1")), errs.ErrAlreadyExists)
	must(t, s.CreateVM(ctx, withKey("vm_3", "p2", "a", "k-1")))
	must(t, s.CreateVM(ctx, withKey("vm_4", "p1", "c", "")))
	must(t, s.CreateVM(ctx, withKey("vm_5", "p1", "d", "")))

	got, err := s.GetVMByIdempotencyKey(ctx, "p1", "k-1")
	must(t, err)
	if got.ID != "vm_1" {
		t.Fatalf("GetVMByIdempotencyKey(p1, k-1).ID = %q, want vm_1", got.ID)
	}
	got, err = s.GetVMByIdempotencyKey(ctx, "p2", "k-1")
	must(t, err)
	if got.ID != "vm_3" {
		t.Fatalf("GetVMByIdempotencyKey(p2, k-1).ID = %q, want vm_3", got.ID)
	}
	_, err = s.GetVMByIdempotencyKey(ctx, "p1", "k-missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testVMsListLabelsAndPagination(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, project string, labels map[string]string) {
		v := newVM(id, project, id)
		v.Labels = labels
		must(t, s.CreateVM(ctx, v))
	}
	mk("vm_a", "p1", map[string]string{"env": "prod", "tier": "web"})
	mk("vm_b", "p1", map[string]string{"env": "prod", "tier": "db"})
	mk("vm_c", "p1", map[string]string{"env": "dev", "tier": "web"})
	mk("vm_d", "p2", map[string]string{"env": "prod", "tier": "web"})
	mk("vm_e", "p1", nil)

	list := func(f domain.VMFilter) ([]*domain.VM, error) {
		return s.ListVMs(ctx, f)
	}

	got, err := list(domain.VMFilter{ProjectID: "p1", Limit: 100})
	must(t, err)
	wantIDs(t, got, vmID, "vm_a", "vm_b", "vm_c", "vm_e")

	got, err = list(domain.VMFilter{ProjectID: "p1", Labels: map[string]string{"env": "prod"}, Limit: 100})
	must(t, err)
	wantIDs(t, got, vmID, "vm_a", "vm_b")

	got, err = list(domain.VMFilter{ProjectID: "p1", Labels: map[string]string{"env": "prod", "tier": "web"}, Limit: 100})
	must(t, err)
	wantIDs(t, got, vmID, "vm_a")

	got, err = list(domain.VMFilter{ProjectID: "p1", Labels: map[string]string{"env": "staging"}, Limit: 100})
	must(t, err)
	wantIDs(t, got, vmID)

	ids := pageIDs(t, 3, vmID, func(after string) ([]*domain.VM, error) {
		return list(domain.VMFilter{ProjectID: "p1", After: after, Limit: 3})
	})
	if !slices.Equal(ids, []string{"vm_a", "vm_b", "vm_c", "vm_e"}) {
		t.Fatalf("paginated VMs = %v", ids)
	}

	ids = pageIDs(t, 1, vmID, func(after string) ([]*domain.VM, error) {
		return list(domain.VMFilter{ProjectID: "p1", Labels: map[string]string{"env": "prod"}, After: after, Limit: 1})
	})
	if !slices.Equal(ids, []string{"vm_a", "vm_b"}) {
		t.Fatalf("paginated labelled VMs = %v", ids)
	}
}

func testVMsListByNode(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, node string) {
		v := newVM(id, "p1", id)
		v.NodeID = node
		must(t, s.CreateVM(ctx, v))
	}
	mk("vm_1", "node-1")
	mk("vm_2", "node-2")
	mk("vm_3", "node-1")
	mk("vm_4", "")

	got, err := s.ListVMsByNode(ctx, "node-1")
	must(t, err)
	wantIDs(t, got, vmID, "vm_1", "vm_3")

	got, err = s.ListVMsByNode(ctx, "node-9")
	must(t, err)
	wantIDs(t, got, vmID)
}

func testVMsUpdateCAS(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateVM(ctx, newVM("vm_1", "p1", "web")))

	got, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	v0 := got.Version
	got.CPUs = 4
	must(t, s.UpdateVM(ctx, got))
	if got.Version != v0+1 {
		t.Fatalf("Version after UpdateVM = %d, want %d (incremented in place)", got.Version, v0+1)
	}
	stored, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	if stored.CPUs != 4 || stored.Version != got.Version {
		t.Fatalf("stored VM = CPUs %d Version %d, want CPUs 4 Version %d", stored.CPUs, stored.Version, got.Version)
	}

	// Two copies at the same version: the first write wins, the second must conflict and leave no trace.
	a, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	b, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	a.CPUs = 6
	must(t, s.UpdateVM(ctx, a))
	b.CPUs = 8
	wantErr(t, s.UpdateVM(ctx, b), errs.ErrConflict)
	stored, err = s.GetVM(ctx, "vm_1")
	must(t, err)
	if stored.CPUs != 6 {
		t.Fatalf("CPUs = %d after conflicting write, want 6", stored.CPUs)
	}

	wantErr(t, s.UpdateVM(ctx, &domain.VM{ID: "vm_nope", Version: 1}), errs.ErrNotFound)
}

func testVMsDelete(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateVM(ctx, newVM("vm_1", "p1", "web")))
	must(t, s.DeleteVM(ctx, "vm_1"))

	_, err := s.GetVM(ctx, "vm_1")
	wantErr(t, err, errs.ErrNotFound)
	got, err := s.ListVMs(ctx, domain.VMFilter{ProjectID: "p1", Limit: 100})
	must(t, err)
	wantIDs(t, got, vmID)

	wantErr(t, s.DeleteVM(ctx, "vm_1"), errs.ErrNotFound)
}

// seedClaimable creates six VMs in project p1 covering every claim case. Relative to fixed:
//
//	vm_a: due now, no lease                     -> claimable
//	vm_b: due a minute earlier, no lease        -> claimable, and first because it is earliest
//	vm_c: NextReconcile zero ("nothing to do")  -> never claimable
//	vm_d: due in an hour                        -> not yet claimable
//	vm_e: due now, lease active until +1m       -> not claimable until the lease expires
//	vm_f: due now, lease expired a second ago   -> claimable
func seedClaimable(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	mk := func(id string, next, leaseUntil time.Time) {
		v := newVM(id, "p1", id)
		v.NextReconcile = next
		v.LeaseUntil = leaseUntil
		must(t, s.CreateVM(ctx, v))
	}
	mk("vm_a", fixed, time.Time{})
	mk("vm_b", fixed.Add(-time.Minute), time.Time{})
	mk("vm_c", time.Time{}, time.Time{})
	mk("vm_d", fixed.Add(time.Hour), time.Time{})
	mk("vm_e", fixed, fixed.Add(time.Minute))
	mk("vm_f", fixed, fixed.Add(-time.Second))
}

func testClaimDueOrderedAndLeased(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedClaimable(t, s)

	got, err := s.ClaimVMs(ctx, "r1", fixed, leaseTTL, 10)
	must(t, err)
	wantIDs(t, got, vmID, "vm_b", "vm_a", "vm_f")
	for _, v := range got {
		if v.LeaseOwner != "r1" || !v.LeaseUntil.Equal(fixed.Add(leaseTTL)) {
			t.Errorf("%s: returned lease = (%q, %v), want (r1, %v)", v.ID, v.LeaseOwner, v.LeaseUntil, fixed.Add(leaseTTL))
		}
	}

	stored, err := s.GetVM(ctx, "vm_a")
	must(t, err)
	if stored.LeaseOwner != "r1" || !stored.LeaseUntil.Equal(fixed.Add(leaseTTL)) {
		t.Fatalf("stored lease of vm_a = (%q, %v), want (r1, %v)", stored.LeaseOwner, stored.LeaseUntil, fixed.Add(leaseTTL))
	}
}

func testClaimExclusive(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedClaimable(t, s)

	first, err := s.ClaimVMs(ctx, "r1", fixed, leaseTTL, 10)
	must(t, err)
	wantIDs(t, first, vmID, "vm_b", "vm_a", "vm_f")

	second, err := s.ClaimVMs(ctx, "r2", fixed, leaseTTL, 10)
	must(t, err)
	wantIDs(t, second, vmID)

	third, err := s.ClaimVMs(ctx, "r3", fixed.Add(leaseTTL/2), leaseTTL, 10)
	must(t, err)
	wantIDs(t, third, vmID)
}

func testClaimLimit(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedClaimable(t, s)

	c1, err := s.ClaimVMs(ctx, "r1", fixed, leaseTTL, 2)
	must(t, err)
	wantIDs(t, c1, vmID, "vm_b", "vm_a")

	c2, err := s.ClaimVMs(ctx, "r2", fixed, leaseTTL, 2)
	must(t, err)
	wantIDs(t, c2, vmID, "vm_f")

	c3, err := s.ClaimVMs(ctx, "r3", fixed, leaseTTL, 2)
	must(t, err)
	wantIDs(t, c3, vmID)
}

func testClaimExpiredLease(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedClaimable(t, s)

	c1, err := s.ClaimVMs(ctx, "r1", fixed, leaseTTL, 10)
	must(t, err)
	wantIDs(t, c1, vmID, "vm_b", "vm_a", "vm_f")

	// r1's leases ran out at fixed+30s; vm_e's lease (until fixed+1m) is still active.
	c2, err := s.ClaimVMs(ctx, "r2", fixed.Add(leaseTTL+time.Second), leaseTTL, 10)
	must(t, err)
	wantIDs(t, c2, vmID, "vm_b", "vm_a", "vm_f")
	for _, v := range c2 {
		if v.LeaseOwner != "r2" {
			t.Errorf("%s: LeaseOwner = %q after reclaim, want r2", v.ID, v.LeaseOwner)
		}
	}

	// By fixed+2m every lease has expired, so vm_e becomes claimable too; order is NextReconcile then ID.
	c3, err := s.ClaimVMs(ctx, "r3", fixed.Add(2*time.Minute), leaseTTL, 10)
	must(t, err)
	wantIDs(t, c3, vmID, "vm_b", "vm_a", "vm_e", "vm_f")
}

func testClaimZeroNextReconcile(t *testing.T, s store.Store) {
	ctx := context.Background()
	seedClaimable(t, s)

	// Even far in the future, vm_c (NextReconcile zero) must never be handed out.
	got, err := s.ClaimVMs(ctx, "r1", fixed.Add(1000*time.Hour), leaseTTL, 100)
	must(t, err)
	wantIDs(t, got, vmID, "vm_b", "vm_a", "vm_e", "vm_f", "vm_d")
}

// ---- isolation ----

func testIsolationVM(t *testing.T, s store.Store) {
	ctx := context.Background()

	v := newVM("vm_1", "p1", "web")
	v.DiskIDs = []string{"d1"}
	v.Holds = []string{"h1"}
	v.Labels = map[string]string{"env": "prod"}
	must(t, s.CreateVM(ctx, v))

	// Mutating the object passed to Create must not reach the store.
	v.Name = "changed"
	v.DiskIDs[0] = "changed"
	v.Holds[0] = "changed"
	v.Labels["env"] = "changed"

	check := func(stage string, got *domain.VM) {
		t.Helper()
		if got.Name != "web" || len(got.DiskIDs) != 1 || got.DiskIDs[0] != "d1" ||
			len(got.Holds) != 1 || got.Holds[0] != "h1" || got.Labels["env"] != "prod" {
			t.Fatalf("%s: stored VM = name %q disks %v holds %v labels %v", stage, got.Name, got.DiskIDs, got.Holds, got.Labels)
		}
	}

	got, err := s.GetVM(ctx, "vm_1")
	must(t, err)
	check("after Create mutation", got)

	// Mutating an object returned from GetVM (slices and map included) must not reach the store.
	got.Name = "changed"
	got.DiskIDs[0] = "changed"
	got.Holds[0] = "changed"
	got.Labels["env"] = "changed"
	got.Labels["extra"] = "x"
	got.DiskIDs = append(got.DiskIDs, "more")
	check("after GetVM mutation", mustGet(t, s, "vm_1"))

	// The same holds for objects returned from ListVMs.
	list, err := s.ListVMs(ctx, domain.VMFilter{ProjectID: "p1", Limit: 10})
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("ListVMs returned %d VMs, want 1", len(list))
	}
	list[0].Labels["env"] = "changed"
	list[0].DiskIDs[0] = "changed"
	check("after ListVMs mutation", mustGet(t, s, "vm_1"))
}

// mustGet is GetVM that fails the test on error.
func mustGet(t *testing.T, s store.Store, id string) *domain.VM {
	t.Helper()
	v, err := s.GetVM(context.Background(), id)
	must(t, err)
	return v
}

func testIsolationNode(t *testing.T, s store.Store) {
	ctx := context.Background()
	n := newNode("node-iso")
	must(t, s.UpsertNode(ctx, n))
	n.Address = "changed"
	got, err := s.GetNode(ctx, "node-iso")
	must(t, err)
	if got.Address == "changed" {
		t.Fatalf("stored node address changed after mutating the upserted object")
	}
	got.Address = "changed"
	got, err = s.GetNode(ctx, "node-iso")
	must(t, err)
	if got.Address == "changed" {
		t.Fatalf("stored node address changed after mutating a GetNode result")
	}
}

func testIsolationJobExitCode(t *testing.T, s store.Store) {
	ctx := context.Background()
	j := newJob("j_1", "vm_1", "p1")
	code := 7
	j.ExitCode = &code
	must(t, s.CreateJob(ctx, j))

	// The caller's pointer must not alias the stored value.
	code = 9
	*j.ExitCode = 9
	got, err := s.GetJob(ctx, "j_1")
	must(t, err)
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("ExitCode = %v after mutating the created object, want 7", got.ExitCode)
	}

	*got.ExitCode = 11
	got, err = s.GetJob(ctx, "j_1")
	must(t, err)
	if got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("ExitCode = %v after mutating a GetJob result, want 7", got.ExitCode)
	}
}

// ---- jobs ----

func testJobsCreateAndGet(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateJob(ctx, newJob("j_1", "vm_1", "p1")))

	got, err := s.GetJob(ctx, "j_1")
	must(t, err)
	if got.VMID != "vm_1" || got.ProjectID != "p1" || got.Command != "echo hi" || got.State != domain.JobRunning {
		t.Fatalf("GetJob = %+v", got)
	}
	if got.ExitCode != nil {
		t.Fatalf("ExitCode of a new job = %d, want nil", *got.ExitCode)
	}

	wantErr(t, s.CreateJob(ctx, newJob("j_1", "vm_1", "p1")), errs.ErrAlreadyExists)
	_, err = s.GetJob(ctx, "j_missing")
	wantErr(t, err, errs.ErrNotFound)
}

func testJobsIdempotencyKey(t *testing.T, s store.Store) {
	ctx := context.Background()
	withKey := func(id, project, key string) *domain.Job {
		j := newJob(id, "vm_1", project)
		j.IdempotencyKey = key
		return j
	}
	must(t, s.CreateJob(ctx, withKey("j_1", "p1", "jk")))
	wantErr(t, s.CreateJob(ctx, withKey("j_2", "p1", "jk")), errs.ErrAlreadyExists)
	must(t, s.CreateJob(ctx, withKey("j_3", "p2", "jk")))
	must(t, s.CreateJob(ctx, withKey("j_4", "p1", "")))
	must(t, s.CreateJob(ctx, withKey("j_5", "p1", "")))

	got, err := s.GetJobByIdempotencyKey(ctx, "p1", "jk")
	must(t, err)
	if got.ID != "j_1" {
		t.Fatalf("GetJobByIdempotencyKey(p1, jk).ID = %q, want j_1", got.ID)
	}
	got, err = s.GetJobByIdempotencyKey(ctx, "p2", "jk")
	must(t, err)
	if got.ID != "j_3" {
		t.Fatalf("GetJobByIdempotencyKey(p2, jk).ID = %q, want j_3", got.ID)
	}
	_, err = s.GetJobByIdempotencyKey(ctx, "p1", "nope")
	wantErr(t, err, errs.ErrNotFound)
}

func testJobsUpdateCAS(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateJob(ctx, newJob("j_1", "vm_1", "p1")))

	got, err := s.GetJob(ctx, "j_1")
	must(t, err)
	v0 := got.Version
	zero := 0
	got.State = domain.JobSucceeded
	got.ExitCode = &zero
	got.FinishedAt = fixed.Add(time.Second)
	must(t, s.UpdateJob(ctx, got))
	if got.Version != v0+1 {
		t.Fatalf("Version after UpdateJob = %d, want %d", got.Version, v0+1)
	}
	stored, err := s.GetJob(ctx, "j_1")
	must(t, err)
	if stored.State != domain.JobSucceeded || stored.ExitCode == nil || *stored.ExitCode != 0 ||
		!stored.FinishedAt.Equal(fixed.Add(time.Second)) || stored.Version != got.Version {
		t.Fatalf("stored job = %+v", stored)
	}

	// ExitCode pointer round trip: set a value, then clear it back to nil.
	code := 137
	got.ExitCode = &code
	must(t, s.UpdateJob(ctx, got))
	stored, err = s.GetJob(ctx, "j_1")
	must(t, err)
	if stored.ExitCode == nil || *stored.ExitCode != 137 {
		t.Fatalf("ExitCode = %v, want 137", stored.ExitCode)
	}
	got.ExitCode = nil
	must(t, s.UpdateJob(ctx, got))
	stored, err = s.GetJob(ctx, "j_1")
	must(t, err)
	if stored.ExitCode != nil {
		t.Fatalf("ExitCode = %d, want nil after clearing", *stored.ExitCode)
	}

	// Stale copies conflict.
	a, err := s.GetJob(ctx, "j_1")
	must(t, err)
	b, err := s.GetJob(ctx, "j_1")
	must(t, err)
	a.KilledReason = "by user"
	must(t, s.UpdateJob(ctx, a))
	b.KilledReason = "by reaper"
	wantErr(t, s.UpdateJob(ctx, b), errs.ErrConflict)
	stored, err = s.GetJob(ctx, "j_1")
	must(t, err)
	if stored.KilledReason != "by user" {
		t.Fatalf("KilledReason = %q after conflicting write, want %q", stored.KilledReason, "by user")
	}

	wantErr(t, s.UpdateJob(ctx, &domain.Job{ID: "j_nope", Version: 1}), errs.ErrNotFound)
}

func testJobsListPaginated(t *testing.T, s store.Store) {
	ctx := context.Background()
	var want []string
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("j_%02d", i)
		must(t, s.CreateJob(ctx, newJob(id, "vm_1", "p1")))
		want = append(want, id)
	}
	must(t, s.CreateJob(ctx, newJob("j_99", "vm_2", "p1")))

	ids := pageIDs(t, 2, jobID, func(after string) ([]*domain.Job, error) {
		return s.ListJobs(ctx, "vm_1", after, 2)
	})
	if !slices.Equal(ids, want) {
		t.Fatalf("paginated jobs for vm_1 = %v, want %v", ids, want)
	}

	got, err := s.ListJobs(ctx, "vm_2", "", 10)
	must(t, err)
	wantIDs(t, got, jobID, "j_99")
}

func testJobsListRunning(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, vm string, st domain.JobState) {
		j := newJob(id, vm, "p1")
		j.State = st
		must(t, s.CreateJob(ctx, j))
	}
	mk("j_1", "vm_1", domain.JobRunning)
	mk("j_2", "vm_1", domain.JobSucceeded)
	mk("j_3", "vm_1", domain.JobRunning)
	mk("j_4", "vm_1", domain.JobFailed)
	mk("j_5", "vm_2", domain.JobRunning)
	mk("j_6", "vm_1", domain.JobLost)

	got, err := s.ListRunningJobs(ctx, "vm_1")
	must(t, err)
	wantIDs(t, got, jobID, "j_1", "j_3")
	for _, j := range got {
		if j.State != domain.JobRunning {
			t.Errorf("%s: State = %q from ListRunningJobs, want running", j.ID, j.State)
		}
	}

	got, err = s.ListRunningJobs(ctx, "vm_9")
	must(t, err)
	wantIDs(t, got, jobID)
}

// ---- usage ----

func testUsage(t *testing.T, s store.Store) {
	ctx := context.Background()
	mk := func(id, project string, cpus int, mem int64, desired domain.PowerState, state domain.VMState) {
		v := newVM(id, project, id)
		v.CPUs, v.MemoryMB, v.Desired, v.State = cpus, mem, desired, state
		must(t, s.CreateVM(ctx, v))
	}
	mk("vm_1", "p1", 2, 1024, domain.PowerRunning, domain.VMRunning)
	mk("vm_2", "p1", 4, 2048, domain.PowerStopped, domain.VMStopped)
	mk("vm_3", "p1", 1, 512, domain.PowerRunning, domain.VMStopped) // desired running counts even if not yet up
	mk("vm_4", "p2", 8, 4096, domain.PowerRunning, domain.VMRunning)

	disk := func(id, project string, sizeGB int) {
		must(t, s.CreateDisk(ctx, newDisk(id, project, id, sizeGB)))
	}
	disk("d_1", "p1", 10)
	disk("d_2", "p1", 20)
	disk("d_3", "p2", 100)

	u, err := s.Usage(ctx, "p1")
	must(t, err)
	if want := (domain.Usage{VMs: 3, CPUs: 3, MemoryMB: 1536, DiskGB: 30}); u != want {
		t.Fatalf("Usage(p1) = %+v, want %+v", u, want)
	}

	u, err = s.Usage(ctx, "p2")
	must(t, err)
	if want := (domain.Usage{VMs: 1, CPUs: 8, MemoryMB: 4096, DiskGB: 100}); u != want {
		t.Fatalf("Usage(p2) = %+v, want %+v", u, want)
	}

	u, err = s.Usage(ctx, "p-empty")
	must(t, err)
	if u != (domain.Usage{}) {
		t.Fatalf("Usage(p-empty) = %+v, want zero", u)
	}
}

// ---- transactions ----

func testTxCommit(t *testing.T, s store.Store) {
	ctx := context.Background()
	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.UpsertNode(ctx, newNode("node-tx")); err != nil {
			return err
		}
		if err := tx.CreateImage(ctx, newImage("img_tx", "p1", "tx")); err != nil {
			return err
		}
		return tx.CreateVM(ctx, newVM("vm_tx", "p1", "tx"))
	})
	must(t, err)

	_, err = s.GetNode(ctx, "node-tx")
	must(t, err)
	_, err = s.GetImage(ctx, "img_tx")
	must(t, err)
	_, err = s.GetVM(ctx, "vm_tx")
	must(t, err)
}

func testTxRollback(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateVM(ctx, newVM("vm_1", "p1", "web")))
	v0 := mustGet(t, s, "vm_1").Version

	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.UpsertNode(ctx, newNode("node-rb")); err != nil {
			return err
		}
		if err := tx.CreateImage(ctx, newImage("img_rb", "p1", "rb")); err != nil {
			return err
		}
		got, err := tx.GetVM(ctx, "vm_1")
		if err != nil {
			return err
		}
		got.CPUs = 16
		if err := tx.UpdateVM(ctx, got); err != nil {
			return err
		}
		if err := tx.CreateVM(ctx, newVM("vm_rb", "p1", "rb")); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Tx returned %v, want the callback's error %v unchanged", err, boom)
	}

	_, err = s.GetNode(ctx, "node-rb")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetImage(ctx, "img_rb")
	wantErr(t, err, errs.ErrNotFound)
	_, err = s.GetVM(ctx, "vm_rb")
	wantErr(t, err, errs.ErrNotFound)

	got := mustGet(t, s, "vm_1")
	if got.CPUs != 2 || got.Version != v0 {
		t.Fatalf("vm_1 after rolled-back update = CPUs %d Version %d, want CPUs 2 Version %d", got.CPUs, got.Version, v0)
	}
}

func testTxReadsOwnWrites(t *testing.T, s store.Store) {
	ctx := context.Background()
	err := s.Tx(ctx, func(tx store.Store) error {
		if err := tx.CreateVM(ctx, newVM("vm_rw", "p1", "rw")); err != nil {
			return err
		}
		got, err := tx.GetVM(ctx, "vm_rw")
		if err != nil {
			return fmt.Errorf("GetVM inside Tx after CreateVM: %w", err)
		}
		if got.Name != "rw" {
			t.Errorf("GetVM inside Tx: Name = %q, want rw", got.Name)
		}
		list, err := tx.ListVMs(ctx, domain.VMFilter{ProjectID: "p1", Limit: 10})
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].ID != "vm_rw" {
			t.Errorf("ListVMs inside Tx = %d VMs, want [vm_rw]", len(list))
		}
		got.CPUs = 6
		if err := tx.UpdateVM(ctx, got); err != nil {
			return fmt.Errorf("UpdateVM inside Tx: %w", err)
		}
		again, err := tx.GetVM(ctx, "vm_rw")
		if err != nil {
			return err
		}
		if again.CPUs != 6 || again.Version != got.Version {
			t.Errorf("GetVM inside Tx after UpdateVM = CPUs %d Version %d, want 6 and %d", again.CPUs, again.Version, got.Version)
		}
		return nil
	})
	must(t, err)

	got := mustGet(t, s, "vm_rw")
	if got.CPUs != 6 {
		t.Fatalf("CPUs after committed Tx = %d, want 6", got.CPUs)
	}
}

// ---- concurrency ----

func testConcurrentUpdateVM(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateVM(ctx, newVM("vm_race", "p1", "race")))
	v0 := mustGet(t, s, "vm_race").Version

	const n = 16
	var (
		readers sync.WaitGroup // all reads finish before any write starts
		done    sync.WaitGroup
		start   = make(chan struct{})
		results = make([]error, n)
	)
	for i := range n {
		readers.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			v, err := s.GetVM(ctx, "vm_race")
			readers.Done()
			if err != nil {
				results[i] = err
				return
			}
			<-start
			v.CPUs = 10 + i
			results[i] = s.UpdateVM(ctx, v)
		}()
	}
	readers.Wait()
	close(start)
	done.Wait()

	var ok, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errs.ErrConflict):
			conflicts++
		default:
			t.Errorf("goroutine %d: UpdateVM error = %v, want nil or ErrConflict", i, err)
		}
	}
	if ok != 1 || conflicts != n-1 {
		t.Fatalf("successes = %d, conflicts = %d; want 1 and %d", ok, conflicts, n-1)
	}
	if got := mustGet(t, s, "vm_race"); got.Version != v0+1 {
		t.Fatalf("Version after race = %d, want %d", got.Version, v0+1)
	}
}

func testImagesEmptyProject(t *testing.T, s store.Store) {
	ctx := context.Background()
	must(t, s.CreateImage(ctx, newImage("img_shared", "", "base")))
	must(t, s.CreateImage(ctx, newImage("img_p1", "p1", "mine")))
	got, err := s.ListImages(ctx, "", false, "", 0)
	must(t, err)
	wantIDs(t, got, imageID) // an empty project selects no project images and, without includeShared, nothing
	got, err = s.ListImages(ctx, "", true, "", 0)
	must(t, err)
	wantIDs(t, got, imageID, "img_shared")
}

func testImagesListByState(t *testing.T, s store.Store) {
	ctx := context.Background()
	a, b, c := newImage("img_a", "p1", "a"), newImage("img_b", "p1", "b"), newImage("img_c", "p2", "c")
	a.State, b.State, c.State = domain.ImageCreating, domain.ImageReady, domain.ImageCreating
	for _, i := range []*domain.Image{a, b, c} {
		must(t, s.CreateImage(ctx, i))
	}
	got, err := s.ListImagesByState(ctx, domain.ImageCreating, 0)
	must(t, err)
	wantIDs(t, got, imageID, "img_a", "img_c")
	got, err = s.ListImagesByState(ctx, domain.ImageCreating, 1)
	must(t, err)
	wantIDs(t, got, imageID, "img_a")
}

func testDisksCountUsingImage(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i, img := range []string{"img_a", "img_a", "img_b", ""} {
		d := newDisk(fmt.Sprintf("disk_%d", i), "p1", fmt.Sprintf("d%d", i), 10)
		d.ImageID = img
		must(t, s.CreateDisk(ctx, d))
	}
	for img, want := range map[string]int{"img_a": 2, "img_b": 1, "img_none": 0} {
		n, err := s.CountDisksUsingImage(ctx, img)
		must(t, err)
		if n != want {
			t.Errorf("CountDisksUsingImage(%s) = %d, want %d", img, n, want)
		}
	}
}

// A claim is a write: it bumps the row version, so a copy read before the claim can no longer overwrite
// the claimed row, while the copy the claim returns can be updated and release the lease.
func testClaimBumpsVersion(t *testing.T, s store.Store) {
	ctx := context.Background()
	v := newVM("vm_x", "p1", "x")
	v.NextReconcile = fixed
	must(t, s.CreateVM(ctx, v))
	stale, err := s.GetVM(ctx, "vm_x")
	must(t, err)

	claimed, err := s.ClaimVMs(ctx, "owner-1", fixed, time.Minute, 10)
	must(t, err)
	wantIDs(t, claimed, vmID, "vm_x")
	if claimed[0].Version != stale.Version+1 {
		t.Fatalf("claimed Version = %d, want %d", claimed[0].Version, stale.Version+1)
	}
	stale.StateReason = "from a copy read before the claim"
	wantErr(t, s.UpdateVM(ctx, stale), errs.ErrConflict)

	mine := claimed[0]
	mine.LeaseOwner, mine.LeaseUntil = "", time.Time{}
	must(t, s.UpdateVM(ctx, mine))
}
