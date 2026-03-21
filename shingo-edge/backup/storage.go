package backup

import (
	"context"
	"io"
)

type Storage interface {
	Test(ctx context.Context, stationID string) error
	Put(ctx context.Context, key string, body io.Reader, size int64, metadata map[string]string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	Delete(ctx context.Context, key string) error
}
