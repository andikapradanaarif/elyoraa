package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/job"
	bolt "go.etcd.io/bbolt"
)

const (
	metaBucket   = "meta"
	jobsBucket   = "jobs"
	doneBucket   = "done"
	tailKey      = "tail"
	headKey      = "head"
	maxIDKeyLen  = 20
)

// BoltDBBackend is a file-backed durable queue using BoltDB.
type BoltDBBackend struct {
	db       *bolt.DB
	maxSize  int
	mu       sync.Mutex
	notEmpty chan struct{}
	closed   bool
}

// OpenBoltDB opens (and creates if needed) a BoltDB queue file.
func OpenBoltDB(path string, maxSize int) (*BoltDBBackend, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir queue dir: %w", err)
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open boltdb: %w", err)
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(metaBucket)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(jobsBucket)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}

	b := &BoltDBBackend{db: db, maxSize: maxSize, notEmpty: make(chan struct{}, 1)}

	// Recover head/tail counters from existing data.
	if err := db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(jobsBucket))
		c := bkt.Cursor()
		k, _ := c.First()
		if k != nil {
			head, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return err
			}
			if err := putInt(tx.Bucket([]byte(metaBucket)), headKey, head); err != nil {
				return err
			}
		}
		k, _ = c.Last()
		if k != nil {
			tail, err := strconv.ParseInt(string(k), 10, 64)
			if err != nil {
				return err
			}
			if err := putInt(tx.Bucket([]byte(metaBucket)), tailKey, tail+1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}

	return b, nil
}

func (b *BoltDBBackend) Put(ctx context.Context, j job.Review) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return false
	}

	select {
	case <-ctx.Done():
		return false
	default:
	}

	data, err := json.Marshal(j)
	if err != nil {
		return false
	}

	err = b.db.Update(func(tx *bolt.Tx) error {
		if b.maxSize > 0 {
			if b.lenTx(tx) >= b.maxSize {
				return fmt.Errorf("queue full")
			}
		}
		tail, err := getInt(tx.Bucket([]byte(metaBucket)), tailKey)
		if err != nil {
			tail = 0
		}
		key := fmt.Sprintf("%0*d", maxIDKeyLen, tail)
		if err := tx.Bucket([]byte(jobsBucket)).Put([]byte(key), data); err != nil {
			return err
		}
		return putInt(tx.Bucket([]byte(metaBucket)), tailKey, tail+1)
	})
	if err != nil {
		return false
	}

	select {
	case b.notEmpty <- struct{}{}:
	default:
	}
	return true
}

func (b *BoltDBBackend) Next(ctx context.Context) (job.Review, bool) {
	for {
		select {
		case <-ctx.Done():
			return job.Review{}, false
		default:
		}

		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return job.Review{}, false
		}

		var j job.Review
		var ok bool
		err := b.db.Update(func(tx *bolt.Tx) error {
			bkt := tx.Bucket([]byte(jobsBucket))
			c := bkt.Cursor()
			k, v := c.First()
			if k == nil {
				return nil
			}
			if err := json.Unmarshal(v, &j); err != nil {
				return err
			}
			if err := bkt.Delete(k); err != nil {
				return err
			}
			if err := putInt(tx.Bucket([]byte(metaBucket)), headKey, idFromKey(k)+1); err != nil {
				return err
			}
			ok = true
			return nil
		})
		b.mu.Unlock()

		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if ok {
			return j, true
		}

		// Wait for a new job or context cancellation.
		select {
		case <-ctx.Done():
			return job.Review{}, false
		case <-b.notEmpty:
		}
	}
}

func (b *BoltDBBackend) MarkDone(ctx context.Context, j job.Review) {
	// The job is already removed from the pending queue by Next.
}

func (b *BoltDBBackend) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0
	}

	var n int
	_ = b.db.View(func(tx *bolt.Tx) error {
		n = b.lenTx(tx)
		return nil
	})
	return n
}

func (b *BoltDBBackend) lenTx(tx *bolt.Tx) int {
	return tx.Bucket([]byte(jobsBucket)).Stats().KeyN
}

func (b *BoltDBBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	close(b.notEmpty)
	return b.db.Close()
}

func putInt(bkt *bolt.Bucket, key string, n int64) error {
	return bkt.Put([]byte(key), []byte(strconv.FormatInt(n, 10)))
}

func getInt(bkt *bolt.Bucket, key string) (int64, error) {
	v := bkt.Get([]byte(key))
	if v == nil {
		return 0, fmt.Errorf("missing key %s", key)
	}
	return strconv.ParseInt(string(v), 10, 64)
}

func idFromKey(k []byte) int64 {
	n, _ := strconv.ParseInt(string(k), 10, 64)
	return n
}
