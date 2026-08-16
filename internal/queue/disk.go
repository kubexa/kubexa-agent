package queue

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubexa/kubexa-agent/internal/logger"
	agentmetrics "github.com/kubexa/kubexa-agent/internal/metrics"
)

const (
	segmentMaxBytes = 32 << 20 // 32 MiB per WAL segment
	walMagic        = "KXWQ"
	walVersion      = byte(1)

	recordTypeItem = byte(1)
	recordTypeAck  = byte(2)
)

var (
	errCorruptRecord   = errors.New("corrupt WAL record")
	errInvalidMagic    = errors.New("invalid WAL magic")
	errNotAnItemRecord = errors.New("record at offset is not an item record")
	// ErrDiskFull indicates the spill directory has reached max_disk_bytes.
	ErrDiskFull = errors.New("disk spill limit exceeded")
)

// diskStore provides append-only WAL spill storage with segment rotation.
type diskStore struct {
	dir         string
	maxBytes    int64
	log         *logger.Logger
	metrics     *agentmetrics.QueueMetrics
	mu          sync.Mutex
	segment     *os.File
	segmentPath string
	segmentNum  int
	segmentSize int64
	totalBytes  int64
	closed      bool
	// readHandles are per-segment read-only handles, opened lazily. All reads
	// use ReadAt, which does not touch the file offset, so they never race
	// with the append-only writer or with each other. At 32 MiB segments and
	// a 512 MiB cap there are at most 16 of these, so no eviction policy is
	// needed; compaction and close() are what remove them.
	readMu      sync.Mutex
	readClosed  bool
	readHandles map[int]*os.File
}

// newDiskStore opens or creates spill storage under dir.
func newDiskStore(dir string, maxBytes int64, log *logger.Logger, metrics *agentmetrics.QueueMetrics) (*diskStore, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create spill dir %q: %w", dir, err)
	}

	ds := &diskStore{
		dir:      dir,
		maxBytes: maxBytes,
		log:      log,
		metrics:  metrics,
	}

	if err := ds.openLatestSegment(); err != nil {
		return nil, err
	}
	ds.refreshTotalBytes()
	if metrics != nil {
		metrics.SetDiskBytes(float64(ds.totalBytes))
	}
	return ds, nil
}

func (ds *diskStore) openLatestSegment() error {
	entries, err := os.ReadDir(ds.dir)
	if err != nil {
		return fmt.Errorf("read spill dir: %w", err)
	}

	var nums []int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n, ok := parseSegmentName(e.Name())
		if !ok {
			continue
		}
		nums = append(nums, n)
	}
	sort.Ints(nums)

	if len(nums) == 0 {
		return ds.createSegment(0)
	}

	last := nums[len(nums)-1]
	path := filepath.Join(ds.dir, segmentFilename(last))
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat segment %q: %w", path, err)
	}

	if info.Size() >= segmentMaxBytes {
		return ds.createSegment(last + 1)
	}

	return ds.openSegment(last, path)
}

func (ds *diskStore) createSegment(num int) error {
	if ds.segment != nil {
		if err := ds.segment.Close(); err != nil {
			return fmt.Errorf("close segment: %w", err)
		}
		ds.segment = nil
	}

	path := filepath.Join(ds.dir, segmentFilename(num))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}

	if info, err := f.Stat(); err != nil {
		_ = f.Close()
		return fmt.Errorf("stat new segment: %w", err)
	} else if info.Size() == 0 {
		if err := writeSegmentHeader(f); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("fsync segment header: %w", err)
		}
	}

	ds.segment = f
	ds.segmentPath = path
	ds.segmentNum = num
	ds.segmentSize = 0
	if info, err := f.Stat(); err == nil {
		ds.segmentSize = info.Size()
	}
	return nil
}

func (ds *diskStore) openSegment(num int, path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat segment: %w", err)
	}

	ds.segment = f
	ds.segmentPath = path
	ds.segmentNum = num
	ds.segmentSize = info.Size()
	return nil
}

func writeSegmentHeader(w io.Writer) error {
	if _, err := io.WriteString(w, walMagic); err != nil {
		return fmt.Errorf("write WAL magic: %w", err)
	}
	if _, err := w.Write([]byte{walVersion}); err != nil {
		return fmt.Errorf("write WAL version: %w", err)
	}
	return nil
}

func (ds *diskStore) refreshTotalBytes() {
	var total int64
	entries, err := os.ReadDir(ds.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	ds.totalBytes = total
}

// appendItem writes an item record, fsyncs, and returns where it landed.
func (ds *diskStore) appendItem(item Item) (int, int64, error) {
	body, err := encodeItemRecord(item)
	if err != nil {
		return 0, 0, err
	}
	return ds.appendRecord(recordTypeItem, body)
}

// appendAck writes an ack record and fsyncs.
func (ds *diskStore) appendAck(id string) error {
	body := make([]byte, 4+len(id))
	binary.BigEndian.PutUint32(body, uint32(len(id)))
	copy(body[4:], id)
	_, _, err := ds.appendRecord(recordTypeAck, body)
	return err
}

// appendRecord writes one record and returns the segment number and byte
// offset it was written at, so callers can read it back without keeping the
// payload in memory.
func (ds *diskStore) appendRecord(recType byte, body []byte) (int, int64, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.closed {
		return 0, 0, errors.New("disk store closed")
	}
	if ds.maxBytes > 0 && ds.totalBytes+int64(5+len(body)) > ds.maxBytes {
		return 0, 0, fmt.Errorf("%w (%d bytes)", ErrDiskFull, ds.maxBytes)
	}

	if ds.segment == nil {
		return 0, 0, errors.New("no active WAL segment")
	}

	if ds.segmentSize+int64(5+len(body)) > segmentMaxBytes {
		if err := ds.createSegment(ds.segmentNum + 1); err != nil {
			return 0, 0, err
		}
	}

	// Captured after any rotation: this is the file and offset the record
	// actually lands in. Reading ds.segmentNum before the rotation above
	// would name the previous file.
	segment := ds.segmentNum
	offset := ds.segmentSize

	header := make([]byte, 5)
	header[0] = recType
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))

	if _, err := ds.segment.Write(header); err != nil {
		return 0, 0, fmt.Errorf("write WAL header: %w", err)
	}
	if _, err := ds.segment.Write(body); err != nil {
		return 0, 0, fmt.Errorf("write WAL body: %w", err)
	}
	if err := ds.segment.Sync(); err != nil {
		return 0, 0, fmt.Errorf("fsync WAL segment: %w", err)
	}

	written := int64(len(header) + len(body))
	ds.segmentSize += written
	ds.totalBytes += written
	if ds.metrics != nil {
		ds.metrics.AddDiskBytes(float64(written))
	}
	return segment, offset, nil
}

// readItem reads back the item record written at the given segment and offset.
//
// Reading from the active write segment is safe: appendRecord fsyncs before
// returning, and a location is only published to the queue after that call
// returns, so any offset a caller can name is already durable.
func (ds *diskStore) readItem(segment int, offset int64) (Item, error) {
	f, err := ds.readHandle(segment)
	if err != nil {
		return Item{}, err
	}

	header := make([]byte, 5)
	if _, err := f.ReadAt(header, offset); err != nil {
		return Item{}, fmt.Errorf("read record header at segment %d offset %d: %w", segment, offset, err)
	}
	if header[0] != recordTypeItem {
		return Item{}, fmt.Errorf("%w: segment %d offset %d type %d", errNotAnItemRecord, segment, offset, header[0])
	}
	bodyLen := binary.BigEndian.Uint32(header[1:])
	if bodyLen > 16<<20 {
		return Item{}, fmt.Errorf("%w: body length %d at segment %d offset %d", errCorruptRecord, bodyLen, segment, offset)
	}

	body := make([]byte, bodyLen)
	if _, err := f.ReadAt(body, offset+5); err != nil {
		return Item{}, fmt.Errorf("read record body at segment %d offset %d: %w", segment, offset, err)
	}
	return decodeItemRecord(body)
}

func (ds *diskStore) readHandle(segment int) (*os.File, error) {
	ds.readMu.Lock()
	defer ds.readMu.Unlock()

	if ds.readClosed {
		return nil, errors.New("disk store closed")
	}
	if f, ok := ds.readHandles[segment]; ok {
		return f, nil
	}
	path := filepath.Join(ds.dir, segmentFilename(segment))
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open segment for read: %w", err)
	}
	if ds.readHandles == nil {
		ds.readHandles = make(map[int]*os.File)
	}
	ds.readHandles[segment] = f
	return f, nil
}

// closeReadHandle releases the read handle for one segment, if open. Used by
// compaction before deleting the file.
func (ds *diskStore) closeReadHandle(segment int) {
	ds.readMu.Lock()
	defer ds.readMu.Unlock()
	if f, ok := ds.readHandles[segment]; ok {
		_ = f.Close()
		delete(ds.readHandles, segment)
	}
}

// activeSegment returns the segment currently being appended to.
//
// segmentNum is guarded by ds.mu and the writer moves it on rotation, so the
// queue must not read the field directly — that is a data race the race
// detector will catch under `go test -race`.
func (ds *diskStore) activeSegment() int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.segmentNum
}

// removeSegment deletes one WAL segment file and reclaims its bytes. The caller
// is responsible for having established that nothing references it.
//
// The active write segment is refused outright. A segment carrying no live
// references is not by itself evidence that it is spent: the segment being
// appended to right now normally has no references at all in the moment between
// a rotation and the next spill, and unlinking it would delete the file the
// writer still holds open — on Linux the writes would keep succeeding into an
// unreachable inode and vanish at process exit, with no error anywhere.
//
// segmentNum only ever increases, so a number below it at check time can never
// become the active segment afterwards and the check does not need to stay held
// across the unlink. That matters because ds.mu and ds.readMu are never nested:
// the active-segment check and the byte accounting take ds.mu, closeReadHandle
// takes ds.readMu, and each is a separate critical section.
func (ds *diskStore) removeSegment(num int) error {
	ds.mu.Lock()
	if ds.closed {
		ds.mu.Unlock()
		return errors.New("disk store closed")
	}
	if num >= ds.segmentNum {
		active := ds.segmentNum
		ds.mu.Unlock()
		return fmt.Errorf("refusing to remove segment %d: the active write segment is %d", num, active)
	}
	ds.mu.Unlock()

	// Before the unlink, so no reader is left holding a handle to a deleted
	// inode and no later reader can reopen the path in between.
	ds.closeReadHandle(num)

	path := filepath.Join(ds.dir, segmentFilename(num))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat segment %q: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove segment %q: %w", path, err)
	}

	ds.mu.Lock()
	ds.totalBytes -= info.Size()
	if ds.totalBytes < 0 {
		ds.totalBytes = 0
	}
	total := ds.totalBytes
	ds.mu.Unlock()

	if ds.metrics != nil {
		ds.metrics.SetDiskBytes(float64(total))
	}
	return nil
}

// segmentCount returns how many WAL segment files exist on disk.
func (ds *diskStore) segmentCount() int {
	entries, err := os.ReadDir(ds.dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := parseSegmentName(e.Name()); ok {
			n++
		}
	}
	return n
}

// recover replays all segments and returns references to items not yet
// acknowledged, ordered by enqueue time. Payloads stay on disk.
func (ds *diskStore) recover() ([]diskRef, error) {
	entries, err := os.ReadDir(ds.dir)
	if err != nil {
		return nil, fmt.Errorf("read spill dir: %w", err)
	}

	type segFile struct {
		num  int
		path string
	}
	var segs []segFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n, ok := parseSegmentName(e.Name()); ok {
			segs = append(segs, segFile{num: n, path: filepath.Join(ds.dir, e.Name())})
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].num < segs[j].num })

	pending := make(map[string]diskRef)
	acked := make(map[string]struct{})

	for _, s := range segs {
		if err := replaySegment(s.num, s.path, pending, acked, ds.log); err != nil {
			ds.log.Error("skip corrupted WAL segment",
				logger.F("path", s.path),
				logger.F("error", err.Error()),
			)
		}
	}

	out := make([]diskRef, 0, len(pending))
	for _, ref := range pending {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].EnqueuedAt.Before(out[j].EnqueuedAt)
	})
	return out, nil
}

func replaySegment(segNum int, path string, pending map[string]diskRef, acked map[string]struct{}, log *logger.Logger) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open segment %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	magic := make([]byte, len(walMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return fmt.Errorf("read segment header: %w", err)
	}
	if string(magic) != walMagic {
		return errInvalidMagic
	}
	if _, err := r.ReadByte(); err != nil {
		return fmt.Errorf("read WAL version: %w", err)
	}

	// Byte position of the next record, mirroring what appendRecord returned
	// when it wrote it. The header is the 4-byte magic plus a 1-byte version.
	offset := int64(len(walMagic) + 1)

	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if log != nil {
				log.Error("corrupt WAL record, stopping segment replay",
					logger.F("path", path),
					logger.F("error", err.Error()),
				)
			}
			return nil
		}
		recType := header[0]
		bodyLen := binary.BigEndian.Uint32(header[1:])
		if bodyLen > 16<<20 {
			if log != nil {
				log.Error("corrupt WAL record, stopping segment replay",
					logger.F("path", path),
					logger.F("error", errCorruptRecord.Error()),
				)
			}
			return nil
		}
		recordOffset := offset
		offset += int64(5) + int64(bodyLen)

		switch recType {
		case recordTypeItem:
			// readItemHeader streams the record off r and discards the
			// payload through bufio's fixed buffer, so on success the reader
			// sits exactly at the next record's header. On error, though, the
			// reader may be stopped mid-record at an unknown position, so we
			// cannot safely continue to the next iteration — stop replaying
			// this segment entirely, same as any other corrupt record.
			id, enqueuedAt, attempts, err := readItemHeader(r, bodyLen)
			if err != nil {
				if log != nil {
					log.Error("corrupt WAL item record, stopping segment replay",
						logger.F("path", path), logger.F("error", err.Error()))
				}
				return nil
			}
			if _, ok := acked[id]; ok {
				continue
			}
			pending[id] = diskRef{
				ID:         id,
				Segment:    segNum,
				Offset:     recordOffset,
				EnqueuedAt: enqueuedAt,
				Attempts:   attempts,
			}
		case recordTypeAck:
			// Ack bodies are just an id, so reading the whole thing is fine.
			body := make([]byte, bodyLen)
			if _, err := io.ReadFull(r, body); err != nil {
				if log != nil {
					log.Error("corrupt WAL ack record, stopping segment replay",
						logger.F("path", path), logger.F("error", err.Error()))
				}
				return nil
			}
			id, err := decodeAckRecord(body)
			if err != nil {
				if log != nil {
					log.Error("corrupt WAL ack record", logger.F("path", path), logger.F("error", err.Error()))
				}
				continue
			}
			acked[id] = struct{}{}
			delete(pending, id)
		default:
			// Unknown record type: skip its body through the fixed buffer so
			// the reader stays aligned on the next record's header.
			if _, err := r.Discard(int(bodyLen)); err != nil {
				if log != nil {
					log.Error("corrupt WAL record, stopping segment replay",
						logger.F("path", path), logger.F("error", err.Error()))
				}
				return nil
			}
		}
	}
}

func encodeItemRecord(item Item) ([]byte, error) {
	id := []byte(item.ID)
	if len(id) > 1<<20 {
		return nil, fmt.Errorf("item id too long")
	}
	if len(item.Payload) > 16<<20 {
		return nil, fmt.Errorf("item payload too large")
	}

	body := make([]byte, 4+len(id)+4+len(item.Payload)+8+4)
	off := 0
	binary.BigEndian.PutUint32(body[off:], uint32(len(id)))
	off += 4
	copy(body[off:], id)
	off += len(id)
	binary.BigEndian.PutUint32(body[off:], uint32(len(item.Payload)))
	off += 4
	copy(body[off:], item.Payload)
	off += len(item.Payload)
	binary.BigEndian.PutUint64(body[off:], uint64(item.EnqueuedAt.UnixNano()))
	off += 8
	binary.BigEndian.PutUint32(body[off:], uint32(item.Attempts))
	return body, nil
}

func decodeItemRecord(body []byte) (Item, error) {
	if len(body) < 16 {
		return Item{}, errCorruptRecord
	}
	off := 0
	idLen := binary.BigEndian.Uint32(body[off:])
	off += 4
	if int(idLen) > len(body)-off {
		return Item{}, errCorruptRecord
	}
	id := string(body[off : off+int(idLen)])
	off += int(idLen)
	if len(body) < off+4 {
		return Item{}, errCorruptRecord
	}
	payloadLen := binary.BigEndian.Uint32(body[off:])
	off += 4
	if int(payloadLen) > len(body)-off {
		return Item{}, errCorruptRecord
	}
	payload := make([]byte, payloadLen)
	copy(payload, body[off:off+int(payloadLen)])
	off += int(payloadLen)
	if len(body) < off+12 {
		return Item{}, errCorruptRecord
	}
	nanos := int64(binary.BigEndian.Uint64(body[off:]))
	off += 8
	attempts := int(binary.BigEndian.Uint32(body[off:]))

	return Item{
		ID:         id,
		Payload:    payload,
		EnqueuedAt: time.Unix(0, nanos),
		Attempts:   attempts,
	}, nil
}

// readItemHeader parses an item record's identity, timestamp and attempt
// count directly off r, discarding the payload through bufio's fixed buffer
// instead of ever allocating one the payload's size.
//
// An earlier version of this fix read the whole record body into a
// `make([]byte, bodyLen)` buffer first and only then walked past the payload
// by index — decodeItemRecord's `payload := make([]byte, payloadLen)` moved
// one layer up, not removed. That still allocated and copied every payload
// byte on every startup; the buffer was merely garbage by the next loop
// iteration, invisible to a live-heap check but not to allocation counters.
// Only r.Discard, which advances the reader through its existing internal
// buffer without allocating, actually avoids the allocation. The record
// layout is
//
//	uint32 idLen | id | uint32 payloadLen | payload | int64 nanos | uint32 attempts
//
// and the timestamp/attempts sit after the payload, so they cannot be read
// from a prefix — the payload must genuinely be consumed, just without a copy.
//
// bodyLen is the caller's already-validated record length (see the
// bodyLen > 16<<20 check in replaySegment); the check against it below is an
// integrity check that the field lengths inside the body actually sum to it,
// which the old whole-body decode never verified.
func readItemHeader(r *bufio.Reader, bodyLen uint32) (string, time.Time, int, error) {
	if bodyLen < 20 {
		return "", time.Time{}, 0, errCorruptRecord
	}
	var num [4]byte
	if _, err := io.ReadFull(r, num[:]); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	idLen := binary.BigEndian.Uint32(num[:])
	if idLen > 1<<20 || uint64(idLen)+20 > uint64(bodyLen) {
		return "", time.Time{}, 0, errCorruptRecord
	}
	idBuf := make([]byte, idLen)
	if _, err := io.ReadFull(r, idBuf); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	if _, err := io.ReadFull(r, num[:]); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	payloadLen := binary.BigEndian.Uint32(num[:])
	if 4+uint64(idLen)+4+uint64(payloadLen)+12 != uint64(bodyLen) {
		return "", time.Time{}, 0, errCorruptRecord
	}
	if _, err := r.Discard(int(payloadLen)); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	var tail [12]byte
	if _, err := io.ReadFull(r, tail[:]); err != nil {
		return "", time.Time{}, 0, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	nanos := int64(binary.BigEndian.Uint64(tail[:8]))
	attempts := int(binary.BigEndian.Uint32(tail[8:]))
	return string(idBuf), time.Unix(0, nanos), attempts, nil
}

func decodeAckRecord(body []byte) (string, error) {
	if len(body) < 4 {
		return "", errCorruptRecord
	}
	idLen := binary.BigEndian.Uint32(body[0:4])
	if int(idLen) > len(body)-4 {
		return "", errCorruptRecord
	}
	return string(body[4 : 4+idLen]), nil
}

func (ds *diskStore) close() error {
	ds.readMu.Lock()
	ds.readClosed = true
	for segment, f := range ds.readHandles {
		_ = f.Close()
		delete(ds.readHandles, segment)
	}
	ds.readMu.Unlock()

	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.closed = true
	if ds.segment != nil {
		err := ds.segment.Close()
		ds.segment = nil
		return err
	}
	return nil
}

func segmentFilename(num int) string {
	return fmt.Sprintf("segment-%06d.wal", num)
}

func parseSegmentName(name string) (int, bool) {
	if !strings.HasPrefix(name, "segment-") || !strings.HasSuffix(name, ".wal") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "segment-"), ".wal")
	n, err := strconv.Atoi(mid)
	if err != nil {
		return 0, false
	}
	return n, true
}
