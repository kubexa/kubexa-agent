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
