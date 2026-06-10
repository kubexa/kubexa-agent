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
)

const (
	segmentMaxBytes = 32 << 20 // 32 MiB per WAL segment
	walMagic        = "KXWQ"
	walVersion      = byte(1)

	recordTypeItem = byte(1)
	recordTypeAck  = byte(2)
)

var (
	errCorruptRecord = errors.New("corrupt WAL record")
	errInvalidMagic  = errors.New("invalid WAL magic")
	// ErrDiskFull indicates the spill directory has reached max_disk_bytes.
	ErrDiskFull = errors.New("disk spill limit exceeded")
)

// diskStore provides append-only WAL spill storage with segment rotation.
type diskStore struct {
	dir         string
	maxBytes    int64
	log         *logger.Logger
	metrics     *queueMetrics
	mu          sync.Mutex
	segment     *os.File
	segmentPath string
	segmentNum  int
	segmentSize int64
	totalBytes  int64
	closed      bool
}

// newDiskStore opens or creates spill storage under dir.
func newDiskStore(dir string, maxBytes int64, log *logger.Logger, metrics *queueMetrics) (*diskStore, error) {
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
		metrics.setDiskBytes(ds.totalBytes)
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

// appendItem writes an item record and fsyncs.
func (ds *diskStore) appendItem(item Item) error {
	body, err := encodeItemRecord(item)
	if err != nil {
		return err
	}
	return ds.appendRecord(recordTypeItem, body)
}

// appendAck writes an ack record and fsyncs.
func (ds *diskStore) appendAck(id string) error {
	body := make([]byte, 4+len(id))
	binary.BigEndian.PutUint32(body, uint32(len(id)))
	copy(body[4:], id)
	return ds.appendRecord(recordTypeAck, body)
}

func (ds *diskStore) appendRecord(recType byte, body []byte) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	if ds.closed {
		return errors.New("disk store closed")
	}
	if ds.maxBytes > 0 && ds.totalBytes+int64(5+len(body)) > ds.maxBytes {
		return fmt.Errorf("%w (%d bytes)", ErrDiskFull, ds.maxBytes)
	}

	if ds.segment == nil {
		return errors.New("no active WAL segment")
	}

	if ds.segmentSize+int64(5+len(body)) > segmentMaxBytes {
		if err := ds.createSegment(ds.segmentNum + 1); err != nil {
			return err
		}
	}

	header := make([]byte, 5)
	header[0] = recType
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))

	if _, err := ds.segment.Write(header); err != nil {
		return fmt.Errorf("write WAL header: %w", err)
	}
	if _, err := ds.segment.Write(body); err != nil {
		return fmt.Errorf("write WAL body: %w", err)
	}
	if err := ds.segment.Sync(); err != nil {
		return fmt.Errorf("fsync WAL segment: %w", err)
	}

	written := int64(len(header) + len(body))
	ds.segmentSize += written
	ds.totalBytes += written
	if ds.metrics != nil {
		ds.metrics.addDiskBytes(written)
	}
	return nil
}

// recover replays all segments and returns items not yet acknowledged.
func (ds *diskStore) recover() ([]Item, error) {
	entries, err := os.ReadDir(ds.dir)
	if err != nil {
		return nil, fmt.Errorf("read spill dir: %w", err)
	}

	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, ok := parseSegmentName(e.Name()); ok {
			paths = append(paths, filepath.Join(ds.dir, e.Name()))
		}
	}
	sort.Strings(paths)

	pending := make(map[string]Item)
	acked := make(map[string]struct{})

	for _, path := range paths {
		if err := replaySegment(path, pending, acked, ds.log); err != nil {
			ds.log.Error("skip corrupted WAL segment", logger.F("path", path), logger.F("error", err.Error()))
		}
	}

	out := make([]Item, 0, len(pending))
	for _, item := range pending {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].EnqueuedAt.Before(out[j].EnqueuedAt)
	})
	return out, nil
}

func replaySegment(path string, pending map[string]Item, acked map[string]struct{}, log *logger.Logger) error {
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

	for {
		recType, body, err := readRecord(r)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if log != nil {
				log.Error("corrupt WAL record, stopping segment replay",
					logger.F("path", path),
					logger.F("error", err.Error()),
				)
			}
			return nil
		}

		switch recType {
		case recordTypeItem:
			item, err := decodeItemRecord(body)
			if err != nil {
				if log != nil {
					log.Error("corrupt WAL item record", logger.F("path", path), logger.F("error", err.Error()))
				}
				continue
			}
			if _, ok := acked[item.ID]; ok {
				continue
			}
			pending[item.ID] = item
		case recordTypeAck:
			id, err := decodeAckRecord(body)
			if err != nil {
				if log != nil {
					log.Error("corrupt WAL ack record", logger.F("path", path), logger.F("error", err.Error()))
				}
				continue
			}
			acked[id] = struct{}{}
			delete(pending, id)
		}
	}
}

func readRecord(r *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	_, err := io.ReadFull(r, header)
	if err != nil {
		return 0, nil, err
	}
	recType := header[0]
	bodyLen := binary.BigEndian.Uint32(header[1:])
	if bodyLen > 16<<20 {
		return 0, nil, errCorruptRecord
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, fmt.Errorf("%w: %v", errCorruptRecord, err)
	}
	return recType, body, nil
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
