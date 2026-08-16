package queue

import (
	"strings"
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
	if _, err := ds.readItem(0, int64(len(walMagic)+1)); err == nil {
		t.Fatal("readItem() on an ack record returned nil error, want failure")
	}
}

func TestReadItemMissingSegment(t *testing.T) {
	ds := newTestDiskStore(t, 64<<20)
	if _, err := ds.readItem(999, 5); err == nil {
		t.Fatal("readItem() on a missing segment returned nil error, want failure")
	}
}
