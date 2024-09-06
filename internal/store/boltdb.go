package store

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"github.com/duh-rpc/duh-go"
	"github.com/kapetan-io/errors"
	"github.com/kapetan-io/querator/internal/types"
	"github.com/kapetan-io/querator/transport"
	"github.com/kapetan-io/tackle/clock"
	"github.com/kapetan-io/tackle/random"
	"github.com/segmentio/ksuid"
	bolt "go.etcd.io/bbolt"
	"os"
	"path/filepath"
)

var bucketName = []byte("queue")

type BoltConfig struct {
	// StorageDir is the directory where bolt will store its data
	StorageDir string
	// Logger is used to log warnings and errors
	Logger duh.StandardLogger
	// Clock is a time provider used to preform time related calculations. It is configurable so that it can
	// be overridden for testing.
	Clock *clock.Provider
}

// TODO: Make BoltBackend non blocking, and obey the context provided. Perhaps we introduce a AsyncStorage
//   struct which takes a normal storage implementation and makes each call async and cancellable, making
//   a new call when the previous call failed, should be an error, a new call cannot be made until the
//   previous call completes.

//type BoltBackend struct {
//	conf BoltConfig
//}
//
//var _ Backend = &BoltBackend{}
//
//func (b *BoltBackend) ParseID(parse types.ItemID, id *StorageID) error {
//	parts := bytes.Split(parse, []byte("~"))
//	if len(parts) != 2 {
//		return errors.New("expected format <queue_name>~<storage_id>")
//	}
//	id.Queue = string(parts[0])
//	id.ID = parts[1]
//	return nil
//}
//
//// TODO: Remove this, no need to include the queue name in the id anymore.
//func (b *BoltBackend) BuildStorageID(queue string, id []byte) types.ItemID {
//	return append([]byte(queue+"~"), id...)
//}
//
//func (b *BoltBackend) Close(_ context.Context) error {
//	return nil
//}
//
//func NewBoltBackend(conf BoltConfig) *BoltBackend {
//	set.Default(&conf.Logger, slog.Default())
//	set.Default(&conf.StorageDir, ".")
//	set.Default(&conf.Clock, clock.NewProvider())
//	return &BoltBackend{conf: conf}
//}

// ---------------------------------------------
// PartitionStore Implementation
// ---------------------------------------------

type BoltPartitionStore struct {
	conf BoltConfig
}

var _ PartitionStore = &BoltPartitionStore{}

func NewBoltPartitionStore(conf BoltConfig) *BoltPartitionStore {
	return &BoltPartitionStore{conf: conf}
}

func (b BoltPartitionStore) Create(info types.PartitionInfo) error {
	// Does nothing as memory has nothing to create. Calls to Get() create the partition
	// when requested.
	return nil
}

func (b BoltPartitionStore) Get(info types.PartitionInfo) Partition {
	return &BoltPartition{
		uid:  ksuid.New(),
		conf: b.conf,
		info: info,
	}
}

// ---------------------------------------------
// Partition Implementation
// ---------------------------------------------

type BoltPartition struct {
	info types.PartitionInfo
	conf BoltConfig
	uid  ksuid.KSUID
	db   *bolt.DB
}

func (b *BoltPartition) Produce(_ context.Context, batch types.Batch[types.ProduceRequest]) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Produce"}
	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		for _, r := range batch.Requests {
			for _, item := range r.Items {
				b.uid = b.uid.Next()
				item.ID = []byte(b.uid.String())
				item.CreatedAt = b.conf.Clock.Now().UTC()

				// TODO: Get buffers from memory pool
				var buf bytes.Buffer
				if err := gob.NewEncoder(&buf).Encode(item); err != nil {
					return f.Errorf("during gob.Encode(): %w", err)
				}

				if err := bucket.Put(item.ID, buf.Bytes()); err != nil {
					return f.Errorf("during Put(): %w", err)
				}
			}
		}
		return nil
	})
}

func (b *BoltPartition) Reserve(_ context.Context, batch types.ReserveBatch, opts ReserveOptions) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Reserve"}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {

		b := tx.Bucket(bucketName)
		if b == nil {
			return f.Error("bucket does not exist in data file")
		}

		batchIter := batch.Iterator()
		c := b.Cursor()
		var count int

		// We preform a full scan of the entire bucket to find our reserved items.
		// I might entertain using an index for this if Bolt becomes a popular choice
		// in production.
		for k, v := c.First(); k != nil; k, v = c.Next() {
			if count >= batch.Total {
				break
			}

			item := new(types.Item) // TODO: memory pool
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(item); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}

			if item.IsReserved {
				continue
			}

			item.ReserveDeadline = opts.ReserveDeadline
			item.IsReserved = true
			count++

			// Assign the item to the next waiting reservation in the batch,
			// returns false if there are no more reservations available to fill
			if batchIter.Next(item) {
				// If assignment was a success, then we put the updated item into the db
				var buf bytes.Buffer // TODO: memory pool
				if err := gob.NewEncoder(&buf).Encode(item); err != nil {
					return f.Errorf("during gob.Encode(): %w", err)
				}

				if err := b.Put(item.ID, buf.Bytes()); err != nil {
					return f.Errorf("during Put(): %w", err)
				}
				continue
			}
			break
		}
		return nil
	})
}

func (b *BoltPartition) Complete(_ context.Context, batch types.Batch[types.CompleteRequest]) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Complete"}
	var done bool

	db, err := b.getDB()
	if err != nil {
		return err
	}

	tx, err := db.Begin(true)
	if err != nil {
		return f.Errorf("during Begin(): %w", err)
	}

	defer func() {
		if !done {
			if err := tx.Rollback(); err != nil {
				b.conf.Logger.Error("during Rollback()", "error", err)
			}
		}
	}()

	bucket := tx.Bucket(bucketName)
	if bucket == nil {
		return f.Error("bucket does not exist in data file")
	}

nextBatch:
	for i := range batch.Requests {
		for _, id := range batch.Requests[i].Ids {
			if err = b.validateID(id); err != nil {
				batch.Requests[i].Err = transport.NewInvalidOption("invalid storage id; '%s': %s", id, err)
				continue nextBatch
			}

			// TODO: Test complete with id's that do not exist in the database
			value := bucket.Get(id)
			if value == nil {
				batch.Requests[i].Err = transport.NewInvalidOption("invalid storage id; '%s' does not exist", id)
				continue nextBatch
			}

			item := new(types.Item) // TODO: memory pool
			if err = gob.NewDecoder(bytes.NewReader(value)).Decode(item); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}

			if !item.IsReserved {
				batch.Requests[i].Err = transport.NewConflict("item(s) cannot be completed; '%s' is not "+
					"marked as reserved", id)
				continue nextBatch
			}

			if err = bucket.Delete(id); err != nil {
				return f.Errorf("during Delete(%s): %w", id, err)
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return f.Errorf("during Commit(): %w", err)
	}

	done = true
	return nil
}

func (b *BoltPartition) List(_ context.Context, items *[]*types.Item, opts types.ListOptions) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.List"}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	if opts.Pivot != nil {
		if err := b.validateID(opts.Pivot); err != nil {
			return transport.NewInvalidOption("invalid storage id; '%s': %s", opts.Pivot, err)
		}
	}

	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return f.Error("bucket does not exist in data file")
		}

		c := b.Cursor()
		var count int
		var k, v []byte
		if opts.Pivot != nil {
			k, v = c.Seek(opts.Pivot)
			if k == nil {
				return transport.NewInvalidOption("invalid pivot; '%s' does not exist", opts.Pivot)
			}
		} else {
			k, v = c.First()
			if k == nil {
				// TODO: Add a test for this code path, attempt to list an empty queue
				// we get here if the bucket is empty
				return nil
			}
		}

		item := new(types.Item) // TODO: memory pool
		if err := gob.NewDecoder(bytes.NewReader(v)).Decode(item); err != nil {
			return f.Errorf("during Decode(): %w", err)
		}

		*items = append(*items, item)
		count++

		for k, v = c.Next(); k != nil; k, v = c.Next() {
			if count >= opts.Limit {
				return nil
			}

			item := new(types.Item) // TODO: memory pool
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(item); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}

			*items = append(*items, item)
			count++
		}
		return nil
	})
}

func (b *BoltPartition) Add(_ context.Context, items []*types.Item) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Add"}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		for _, item := range items {
			b.uid = b.uid.Next()
			item.ID = []byte(b.uid.String())
			item.CreatedAt = b.conf.Clock.Now().UTC()

			// TODO: Get buffers from memory pool
			var buf bytes.Buffer
			if err := gob.NewEncoder(&buf).Encode(item); err != nil {
				return f.Errorf("during gob.Encode(): %w", err)
			}

			if err := bucket.Put(item.ID, buf.Bytes()); err != nil {
				return f.Errorf("during Put(): %w", err)
			}
		}
		return nil
	})
}

func (b *BoltPartition) Delete(_ context.Context, ids []types.ItemID) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Delete"}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		for _, id := range ids {
			if err := b.validateID(id); err != nil {
				return transport.NewInvalidOption("invalid storage id; '%s': %s", id, err)
			}
			if err := bucket.Delete(id); err != nil {
				return fmt.Errorf("during delete: %w", err)
			}
		}
		return nil
	})
}

func (b *BoltPartition) Clear(_ context.Context, destructive bool) error {
	f := errors.Fields{"category", "bolt", "func", "Partition.Delete"}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		if destructive {
			if err := tx.DeleteBucket(bucketName); err != nil {
				return f.Errorf("during destructive DeleteBucket(): %w", err)
			}
			if _, err := tx.CreateBucket(bucketName); err != nil {
				return f.Errorf("while re-creating with CreateBucket()): %w", err)
			}
			return nil
		}

		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			item := new(types.Item) // TODO: memory pool
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(item); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}

			// Skip reserved items
			if item.IsReserved {
				continue
			}

			if err := bucket.Delete(k); err != nil {
				return f.Errorf("during Delete(): %w", err)
			}
		}
		return nil
	})
}

func (b *BoltPartition) Stats(_ context.Context, stats *types.QueueStats) error {
	f := errors.Fields{"category", "bunt-db", "func", "Partition.Stats"}
	now := b.conf.Clock.Now().UTC()

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.View(func(tx *bolt.Tx) error {

		b := tx.Bucket(bucketName)
		if b == nil {
			return f.Error("bucket does not exist in data file")
		}

		c := b.Cursor()

		for k, v := c.First(); k != nil; k, v = c.Next() {
			item := new(types.Item) // TODO: memory pool
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(item); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}

			stats.Total++
			stats.AverageAge += now.Sub(item.CreatedAt)
			if item.IsReserved {
				stats.AverageReservedAge += item.ReserveDeadline.Sub(now)
				stats.TotalReserved++
			}
		}
		if stats.Total != 0 {
			stats.AverageAge = clock.Duration(int64(stats.AverageAge) / int64(stats.Total))
		}
		if stats.TotalReserved != 0 {
			stats.AverageReservedAge = clock.Duration(int64(stats.AverageReservedAge) / int64(stats.TotalReserved))
		}
		return nil
	})
}

func (b *BoltPartition) Close(_ context.Context) error {
	if b.db != nil {
		return b.db.Close()
	}
	return nil
}

func (b *BoltPartition) validateID(id []byte) error {
	_, err := ksuid.FromBytes(id)
	if err != nil {
		return errors.New("invalid storage id")
	}
	return nil
}

func (b *BoltPartition) getDB() (*bolt.DB, error) {
	if b.db != nil {
		return b.db, nil
	}

	f := errors.Fields{"category", "bolt", "func", "BoltPartition.getDB"}
	file := filepath.Join(b.conf.StorageDir, fmt.Sprintf("%s-%06d.db", b.info.QueueName, b.info.Partition))

	opts := &bolt.Options{
		FreelistType: bolt.FreelistArrayType,
		Timeout:      clock.Second,
		NoGrowSync:   false,
	}

	db, err := bolt.Open(file, 0600, opts)
	if err != nil {
		return nil, f.Errorf("while opening db '%s': %w", file, err)
	}

	// TODO: Test opening an existing partition.
	err = db.Update(func(tx *bolt.Tx) error {
		// TODO: This should open an bucket, not create one
		_, err := tx.CreateBucket(bucketName)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, f.Errorf("while creating bucket '%s': %w", file, err)
	}
	b.db = db
	return db, nil
}

func NewBoltQueueStore(conf BoltConfig) QueueStore {
	return &BoltQueueStore{
		db: db,
	}, nil
}

// ---------------------------------------------
// QueueStore Implementation
// ---------------------------------------------

type BoltQueueStore struct {
	QueuesValidation
	db   *bolt.DB
	conf BoltConfig
}

var _ QueueStore = &BoltQueueStore{}

func (b BoltQueueStore) getDB() (*bolt.DB, error) {
	if b.db != nil {
		return b.db, nil
	}

	f := errors.Fields{"category", "bolt", "func", "Storage.QueueStore"}
	// We store info about the queues in a single db file. We prefix it with `~` to make it
	// impossible for someone to create a queue with the same name.
	file := filepath.Join(b.conf.StorageDir, "~queue-storage.db")
	db, err := bolt.Open(file, 0600, bolt.DefaultOptions)
	if err != nil {
		return nil, f.Errorf("while opening db '%s': %w", file, err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(bucketName)
		if err != nil {
			if !errors.Is(err, bolt.ErrBucketExists) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, f.Errorf("while creating bucket '%s': %w", file, err)
	}
	b.db = db
	return db, nil
}

func (b BoltQueueStore) Get(_ context.Context, name string, queue *types.QueueInfo) error {
	f := errors.Fields{"category", "bolt", "func", "QueueStore.Get"}

	if err := b.validateGet(name); err != nil {
		return err
	}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b == nil {
			return f.Error("bucket does not exist in data file")
		}

		v := b.Get([]byte(name))
		if v == nil {
			return ErrQueueNotExist
		}

		if err := gob.NewDecoder(bytes.NewReader(v)).Decode(queue); err != nil {
			return f.Errorf("during Decode(): %w", err)
		}
		return nil
	})
}

func (b BoltQueueStore) Add(_ context.Context, info types.QueueInfo) error {
	f := errors.Fields{"category", "bolt", "func", "QueueStore.Add"}

	if err := b.validateAdd(info); err != nil {
		return err
	}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		// If the queue already exists in the store
		if bucket.Get([]byte(info.Name)) != nil {
			return transport.NewInvalidOption("invalid queue; '%b' already exists", info.Name)
		}

		var buf bytes.Buffer // TODO: memory pool
		if err := gob.NewEncoder(&buf).Encode(info); err != nil {
			return f.Errorf("during gob.Encode(): %w", err)
		}

		if err := bucket.Put([]byte(info.Name), buf.Bytes()); err != nil {
			return f.Errorf("during Put(): %w", err)
		}
		return nil
	})
}

func (b BoltQueueStore) Update(_ context.Context, info types.QueueInfo) error {
	f := errors.Fields{"category", "bolt", "func", "QueueStore.Update"}

	if err := b.validateUpdate(info); err != nil {
		return err
	}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		v := bucket.Get([]byte(info.Name))
		if v == nil {
			return ErrQueueNotExist
		}

		var found types.QueueInfo
		if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&found); err != nil {
			return f.Errorf("during Decode(): %w", err)
		}

		found.Update(info)

		if found.ReserveTimeout > found.DeadTimeout {
			return transport.NewInvalidOption("reserve timeout is too long; %b cannot be greater than the "+
				"dead timeout %b", info.ReserveTimeout.String(), found.DeadTimeout.String())
		}

		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(found); err != nil {
			return f.Errorf("during gob.Encode(): %w", err)
		}

		if err := bucket.Put([]byte(info.Name), buf.Bytes()); err != nil {
			return f.Errorf("during Put(): %w", err)
		}
		return nil
	})
}

func (b BoltQueueStore) List(_ context.Context, queues *[]types.QueueInfo, opts types.ListOptions) error {
	f := errors.Fields{"category", "bolt", "func", "QueueStore.List"}

	if err := b.validateList(opts); err != nil {
		return err
	}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		c := bucket.Cursor()
		var count int
		var k, v []byte
		if opts.Pivot != nil {
			k, v = c.Seek(opts.Pivot)
			if k == nil {
				return transport.NewInvalidOption("invalid pivot; '%b' does not exist", opts.Pivot)
			}

		} else {
			k, v = c.First()
			if k == nil {
				// TODO: Add a test for this code path, attempt to list an empty queue
				// we get here if the bucket is empty
				return nil
			}
		}

		var info types.QueueInfo
		if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&info); err != nil {
			return f.Errorf("during Decode(): %w", err)
		}
		*queues = append(*queues, info)
		count++

		for k, v = c.Next(); k != nil; k, v = c.Next() {
			if count >= opts.Limit {
				return nil
			}

			var info types.QueueInfo
			if err := gob.NewDecoder(bytes.NewReader(v)).Decode(&info); err != nil {
				return f.Errorf("during Decode(): %w", err)
			}
			*queues = append(*queues, info)
			count++
		}
		return nil
	})
}

func (b BoltQueueStore) Delete(_ context.Context, name string) error {
	f := errors.Fields{"category", "bolt", "func", "QueueStore.Delete"}

	if err := b.validateDelete(name); err != nil {
		return err
	}

	db, err := b.getDB()
	if err != nil {
		return err
	}

	return db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return f.Error("bucket does not exist in data file")
		}

		if err := bucket.Delete([]byte(name)); err != nil {
			return f.Errorf("during Delete(%b): %w", name, err)
		}
		return nil
	})
}

func (b BoltQueueStore) Close(_ context.Context) error {
	return b.db.Close()
}

// ---------------------------------------------
// Test Helper
// ---------------------------------------------

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		return false
	}
	return info.IsDir()
}

type BoltDBTesting struct {
	Dir string
}

func (b *BoltDBTesting) TestSetup(conf BoltConfig) *StorageConfig {
	if !dirExists(b.Dir) {
		if err := os.Mkdir(b.Dir, 0777); err != nil {
			panic(err)
		}
	}
	b.Dir = filepath.Join(b.Dir, random.String("test-data-", 10))
	if err := os.Mkdir(b.Dir, 0777); err != nil {
		panic(err)
	}
	conf.StorageDir = b.Dir

	//backend := NewBoltBackend(conf)
	//s, err := NewStorage(StorageConfig{
	//	QueueStore: backend,
	//	PartitionBackends: []PartitionBackend{
	//		{
	//			Name:    "bolt-0",
	//			Backend: backend,
	//		},
	//	},
	//})
	//if err != nil {
	//	panic(err)
	//}
	//return s
}

func (b *BoltDBTesting) Teardown() {
	if err := os.RemoveAll(b.Dir); err != nil {
		panic(err)
	}
}
