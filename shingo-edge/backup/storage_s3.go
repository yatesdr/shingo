package backup

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shingoedge/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type S3Storage struct {
	client *minio.Client
	bucket string
}

func NewS3Storage(cfg config.BackupS3Config) (*S3Storage, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("backup endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, fmt.Errorf("backup bucket is required")
	}
	if strings.TrimSpace(cfg.AccessKey) == "" || strings.TrimSpace(cfg.SecretKey) == "" {
		return nil, fmt.Errorf("backup access key and secret key are required")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}

	rawEndpoint := strings.TrimSpace(cfg.Endpoint)
	secure := true
	endpoint := rawEndpoint
	if strings.Contains(rawEndpoint, "://") {
		u, err := url.Parse(rawEndpoint)
		if err != nil {
			return nil, fmt.Errorf("parse backup endpoint: %w", err)
		}
		secure = strings.EqualFold(u.Scheme, "https")
		endpoint = u.Host
		if endpoint == "" {
			return nil, fmt.Errorf("backup endpoint host is required")
		}
	}

	httpClient := &http.Client{Timeout: 30 * time.Second}
	if cfg.InsecureSkipTLSVerify {
		httpClient.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		}
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		Transport:    httpClient.Transport,
		BucketLookup: minio.BucketLookupAuto,
	})
	if err != nil {
		return nil, fmt.Errorf("create s3 client: %w", err)
	}
	if cfg.UsePathStyle {
		client, err = minio.New(endpoint, &minio.Options{
			Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure:       secure,
			Region:       region,
			Transport:    httpClient.Transport,
			BucketLookup: minio.BucketLookupPath,
		})
		if err != nil {
			return nil, fmt.Errorf("create path-style s3 client: %w", err)
		}
	}
	return &S3Storage{client: client, bucket: strings.TrimSpace(cfg.Bucket)}, nil
}

func (s *S3Storage) Test(ctx context.Context, stationID string) error {
	key := objectPrefix(stationID) + ".healthcheck-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".txt"
	body := []byte("ok")
	if err := s.Put(ctx, key, bytes.NewReader(body), int64(len(body)), map[string]string{"station-id": stationID}); err != nil {
		return err
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		_ = s.Delete(ctx, key)
		return err
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		_ = s.Delete(ctx, key)
		return fmt.Errorf("read test object: %w", err)
	}
	if string(got) != "ok" {
		_ = s.Delete(ctx, key)
		return fmt.Errorf("unexpected test object contents")
	}
	return s.Delete(ctx, key)
}

func (s *S3Storage) Put(ctx context.Context, key string, body io.Reader, size int64, metadata map[string]string) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, body, size, minio.PutObjectOptions{
		ContentType:  "application/gzip",
		UserMetadata: metadata,
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}

func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	if _, err := out.Stat(); err != nil {
		return nil, fmt.Errorf("stat object %s: %w", key, err)
	}
	return out, nil
}

func (s *S3Storage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for item := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if item.Err != nil {
			return nil, fmt.Errorf("list objects for %s: %w", prefix, item.Err)
		}
		if item.Key == "" || strings.HasSuffix(item.Key, "/") {
			continue
		}
		lastModified := item.LastModified
		out = append(out, ObjectInfo{
			Key:          item.Key,
			Size:         item.Size,
			LastModified: &lastModified,
		})
	}
	return out, nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete object %s: %w", key, err)
	}
	return nil
}
