package queue

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
)

func newTestDiskStore(t *testing.T, maxBytes int64) *diskStore {
	t.Helper()
	ds, err := newDiskStore(t.TempDir(), maxBytes, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}
	t.Cleanup(func() { _ = ds.close() })
	return ds
}

func testItem(id string, payloadLen int) Item {
	return Item{
		ID:         id,
		Payload:    []byte(strings.Repeat("x", payloadLen)),
		EnqueuedAt: time.Unix(1755000000, 0).UTC(),
	}
}

func TestAppendItemReturnsLocation(t *testing.T) {
	ds := newTestDiskStore(t, 64<<20)

	seg0, off0, err := ds.appendItem(testItem("a", 16))
	if err != nil {
		t.Fatalf("appendItem() error = %v", err)
	}
	// The first record sits immediately after the 5-byte segment header
	// (4-byte magic + 1-byte version).
	if seg0 != 0 || off0 != int64(len(walMagic)+1) {
		t.Errorf("first record at (seg %d, off %d), want (0, %d)", seg0, off0, len(walMagic)+1)
	}

	seg1, off1, err := ds.appendItem(testItem("b", 16))
	if err != nil {
		t.Fatalf("appendItem() error = %v", err)
	}
	if seg1 != 0 {
		t.Errorf("second record in segment %d, want 0", seg1)
	}
	if off1 <= off0 {
		t.Errorf("second record at offset %d, want > %d", off1, off0)
	}
}

// A record that forces rotation must report the NEW segment and an offset
// inside it. Reading ds.segmentNum before rotation is the trap this guards.
func TestAppendItemAcrossRotation(t *testing.T) {
	ds := newTestDiskStore(t, 512<<20)

	big := segmentMaxBytes / 4
	var lastSeg int
	var lastOff int64
	for i := 0; i < 6; i++ {
		seg, off, err := ds.appendItem(testItem("item", big))
		if err != nil {
			t.Fatalf("appendItem(%d) error = %v", i, err)
		}
		if seg > lastSeg {
			// First record of a fresh segment sits right after its header.
			if off != int64(len(walMagic)+1) {
				t.Fatalf("first record of segment %d at offset %d, want %d",
					seg, off, len(walMagic)+1)
			}
		}
		lastSeg, lastOff = seg, off
	}
	if lastSeg == 0 {
		t.Fatal("no rotation happened; test cannot verify the rotation case")
	}
	if lastOff < 0 {
		t.Fatalf("negative offset %d", lastOff)
	}
}

func TestReadItemRoundTrip(t *testing.T) {
	ds := newTestDiskStore(t, 64<<20)

	want := testItem("round-trip", 4096)
	want.Attempts = 3
	seg, off, err := ds.appendItem(want)
	if err != nil {
		t.Fatalf("appendItem() error = %v", err)
	}

	got, err := ds.readItem(seg, off)
	if err != nil {
		t.Fatalf("readItem() error = %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("payload mismatch: got %d bytes, want %d", len(got.Payload), len(want.Payload))
	}
	if got.Attempts != want.Attempts {
		t.Errorf("Attempts = %d, want %d", got.Attempts, want.Attempts)
	}
	if !got.EnqueuedAt.Equal(want.EnqueuedAt) {
		t.Errorf("EnqueuedAt = %v, want %v", got.EnqueuedAt, want.EnqueuedAt)
	}
}

func TestReadItemAcrossSegments(t *testing.T) {
	ds := newTestDiskStore(t, 512<<20)

	type loc struct {
		seg int
		off int64
		id  string
	}
	var locs []loc
	big := segmentMaxBytes / 4
	for i := 0; i < 6; i++ {
		it := testItem("id-"+string(rune('a'+i)), big)
		seg, off, err := ds.appendItem(it)
		if err != nil {
			t.Fatalf("appendItem(%d) error = %v", i, err)
		}
		locs = append(locs, loc{seg: seg, off: off, id: it.ID})
	}
	if locs[len(locs)-1].seg == 0 {
		t.Fatal("no rotation happened; test cannot verify cross-segment reads")
	}

	// Read in reverse so handles are opened out of write order.
	for i := len(locs) - 1; i >= 0; i-- {
		got, err := ds.readItem(locs[i].seg, locs[i].off)
		if err != nil {
			t.Fatalf("readItem(%d, %d) error = %v", locs[i].seg, locs[i].off, err)
		}
		if got.ID != locs[i].id {
			t.Errorf("read id = %q, want %q", got.ID, locs[i].id)
		}
	}
}

func TestReadItemRejectsAnAckRecord(t *testing.T) {
	ds := newTestDiskStore(t, 64<<20)

	// The only offset we know for sure is the first record's.
	if err := ds.appendAck("some-id"); err != nil {
		t.Fatalf("appendAck() error = %v", err)
	}
	_, err := ds.readItem(0, int64(len(walMagic)+1))
	if !errors.Is(err, errNotAnItemRecord) {
		t.Fatalf("readItem() on an ack record: got error %v, want errNotAnItemRecord", err)
	}
}

func TestReadItemMissingSegment(t *testing.T) {
	ds := newTestDiskStore(t, 64<<20)
	_, err := ds.readItem(999, 5)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readItem() on a missing segment: got error %v, want fs.ErrNotExist", err)
	}
}

func TestReadItemConcurrentWithWrites(t *testing.T) {
	ds := newTestDiskStore(t, 512<<20)

	// Pre-write some items and capture their locations.
	type location struct {
		seg     int
		off     int64
		id      string
		payload []byte
	}
	var locations []location
	for i := 0; i < 10; i++ {
		it := testItem("pre-"+string(rune('a'+i)), 1024)
		seg, off, err := ds.appendItem(it)
		if err != nil {
			t.Fatalf("appendItem(%d) error = %v", i, err)
		}
		locations = append(locations, location{
			seg:     seg,
			off:     off,
			id:      it.ID,
			payload: it.Payload,
		})
	}

	var wg sync.WaitGroup

	// Goroutine 1: Keep appending new items.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			it := testItem("append-"+string(rune('a'+i)), 2048)
			if _, _, err := ds.appendItem(it); err != nil {
				t.Errorf("appendItem(%d) error = %v", i, err)
			}
		}
	}()

	// Goroutines 2-5: Concurrently read the pre-written items.
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			// Each reader checks all pre-written locations multiple times.
			for attempt := 0; attempt < 5; attempt++ {
				for _, loc := range locations {
					got, err := ds.readItem(loc.seg, loc.off)
					if err != nil {
						t.Errorf("reader %d attempt %d: readItem(%d, %d) error = %v", r, attempt, loc.seg, loc.off, err)
						continue
					}
					if got.ID != loc.id {
						t.Errorf("reader %d attempt %d: ID = %q, want %q", r, attempt, got.ID, loc.id)
					}
					if string(got.Payload) != string(loc.payload) {
						t.Errorf("reader %d attempt %d: payload mismatch", r, attempt)
					}
				}
			}
		}(reader)
	}

	wg.Wait()
}

func TestRecoverReturnsRefsInEnqueueOrder(t *testing.T) {
	dir := t.TempDir()
	ds, err := newDiskStore(dir, 64<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}

	first := testItem("first", 64)
	first.EnqueuedAt = time.Unix(1755000000, 0).UTC()
	second := testItem("second", 64)
	second.EnqueuedAt = time.Unix(1755000100, 0).UTC()
	third := testItem("third", 64)
	third.EnqueuedAt = time.Unix(1755000200, 0).UTC()

	for _, it := range []Item{third, first, second} {
		if _, _, err := ds.appendItem(it); err != nil {
			t.Fatalf("appendItem(%s) error = %v", it.ID, err)
		}
	}
	if err := ds.appendAck("second"); err != nil {
		t.Fatalf("appendAck() error = %v", err)
	}
	if err := ds.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	reopened, err := newDiskStore(dir, 64<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.close() })

	refs, err := reopened.recover()
	if err != nil {
		t.Fatalf("recover() error = %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("recovered %d refs, want 2 (the acked one must be gone)", len(refs))
	}
	if refs[0].ID != "first" || refs[1].ID != "third" {
		t.Errorf("refs in order %q,%q; want first,third (sorted by EnqueuedAt)",
			refs[0].ID, refs[1].ID)
	}
	// A recovered ref must be readable through the location it carries.
	got, err := reopened.readItem(refs[0].Segment, refs[0].Offset)
	if err != nil {
		t.Fatalf("readItem(recovered ref) error = %v", err)
	}
	if got.ID != "first" {
		t.Errorf("ref location points at %q, want %q", got.ID, "first")
	}
}

// The regression guard for the bug this whole change exists to fix: replay
// must not copy payloads into the heap. 32 items of 1 MiB is 32 MiB of
// payload; a 4 MiB ceiling gives an 8x margin over ref overhead while still
// failing loudly if payloads come back.
func TestRecoverDoesNotMaterializePayloads(t *testing.T) {
	dir := t.TempDir()
	ds, err := newDiskStore(dir, 512<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}
	const (
		count      = 32
		payloadLen = 1 << 20
	)
	for i := 0; i < count; i++ {
		if _, _, err := ds.appendItem(testItem("big-"+string(rune('a'+i)), payloadLen)); err != nil {
			t.Fatalf("appendItem(%d) error = %v", i, err)
		}
	}
	if err := ds.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	reopened, err := newDiskStore(dir, 512<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.close() })

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	refs, err := reopened.recover()
	if err != nil {
		t.Fatalf("recover() error = %v", err)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(refs)

	if len(refs) != count {
		t.Fatalf("recovered %d refs, want %d", len(refs), count)
	}
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	const ceiling = 4 << 20
	if growth > ceiling {
		t.Errorf("recover() grew the live heap by %d bytes for %d MiB of payload; "+
			"want under %d. Payloads are being retained again.",
			growth, (count*payloadLen)>>20, ceiling)
	}
}

func TestRecoverSkipsSegmentDeletedUnderneath(t *testing.T) {
	dir := t.TempDir()
	ds, err := newDiskStore(dir, 64<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("newDiskStore() error = %v", err)
	}
	if _, _, err := ds.appendItem(testItem("only", 64)); err != nil {
		t.Fatalf("appendItem() error = %v", err)
	}
	if err := ds.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if err := os.Remove(dir + "/" + segmentFilename(0)); err != nil {
		t.Fatalf("remove segment: %v", err)
	}

	reopened, err := newDiskStore(dir, 64<<20, logger.New("disk-test"), nil)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.close() })
	refs, err := reopened.recover()
	if err != nil {
		t.Fatalf("recover() error = %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("recovered %d refs from an empty spill dir, want 0", len(refs))
	}
}
