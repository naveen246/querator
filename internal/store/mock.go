package store

import (
	"bytes"
	"context"
	"errors"
	"github.com/kapetan-io/querator/internal/types"
)

type MockConfig struct {
	Methods map[string]func(args []any) error
}

type MockBackend struct {
	conf *MockConfig
}

var _ Backend = &MockBackend{}

func NewMockStorage(conf *MockConfig) *MockBackend {
	return &MockBackend{conf: conf}
}

func (m *MockBackend) GetQueueStore() (QueueStore, error) {
	return &MockQueuesStore{}, nil
}

func (m *MockBackend) GetPartition(info types.PartitionInfo) (Partition, error) {
	return &MockPartition{info: info.Queue, parent: m}, nil
}

func (m *MockBackend) ParseID(parse types.ItemID, id *StorageID) error {
	parts := bytes.Split(parse, []byte("~"))
	if len(parts) != 2 {
		return errors.New("expected format <queue_name>~<storage_id>")
	}
	id.Queue = string(parts[0])
	id.ID = parts[1]
	return nil
}

func (m *MockBackend) BuildStorageID(queue string, id []byte) types.ItemID {
	return append([]byte(queue+"~"), id...)
}

func (m *MockBackend) Close(_ context.Context) error {
	return nil
}

type MockPartition struct {
	info   types.QueueInfo
	parent *MockBackend
}

func (m *MockPartition) Produce(ctx context.Context, batch types.Batch[types.ProduceRequest]) error {
	f, ok := m.parent.conf.Methods["Partition.Produce"]
	if !ok {
		panic("no mock for method \"Partition.Produce\" defined")
	}
	return f([]any{ctx, batch})
}

func (m *MockPartition) Reserve(ctx context.Context, batch types.ReserveBatch, opts ReserveOptions) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Complete(ctx context.Context, batch types.Batch[types.CompleteRequest]) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) List(ctx context.Context, items *[]*types.Item, opts types.ListOptions) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Add(ctx context.Context, items []*types.Item) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Delete(ctx context.Context, ids []types.ItemID) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Clear(ctx context.Context, d bool) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Stats(ctx context.Context, stats *types.QueueStats) error {
	//TODO implement me
	panic("implement me")
}

func (m *MockPartition) Close(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

// ---------------------------------------------
// Partition Repository Implementation
// ---------------------------------------------

type MockQueuesStore struct{}

var _ QueueStore = &MockQueuesStore{}

func (r MockQueuesStore) Get(ctx context.Context, name string, queue *types.QueueInfo) error {
	//TODO implement me
	panic("implement me")
}

func (r MockQueuesStore) Add(ctx context.Context, opts types.QueueInfo) error {
	//TODO implement me
	panic("implement me")
}

func (r MockQueuesStore) Update(ctx context.Context, opts types.QueueInfo) error {
	//TODO implement me
	panic("implement me")
}

func (r MockQueuesStore) List(ctx context.Context, queues *[]types.QueueInfo, opts types.ListOptions) error {
	//TODO implement me
	panic("implement me")
}

func (r MockQueuesStore) Delete(ctx context.Context, queueName string) error {
	//TODO implement me
	panic("implement me")
}

func (r MockQueuesStore) Close(_ context.Context) error {
	return nil
}
