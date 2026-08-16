// Package storage is the object-store gateway (MinIO / any S3-compatible
// backend). Clients never receive long-lived storage credentials: they get
// presigned URLs scoped to a single object and a short expiry (§19).
package storage

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/sobh/messenger/backend/internal/config"
)

type Client struct {
	mc  *minio.Client
	cfg config.Storage
}

// Part identifies one uploaded chunk of a multipart upload.
type Part struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

func Connect(ctx context.Context, cfg config.Storage) (*Client, error) {
	mc, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage: client: %w", err)
	}

	client := &Client{mc: mc, cfg: cfg}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := mc.ListBuckets(checkCtx); err != nil {
		return nil, fmt.Errorf("storage: connectivity check: %w", err)
	}
	return client, nil
}

// EnsureBuckets creates the buckets the platform needs. Media and exports stay
// private; only the public bucket is readable without a presigned URL.
func (c *Client) EnsureBuckets(ctx context.Context) error {
	for _, bucket := range []string{c.cfg.MediaBucket, c.cfg.PublicBucket, c.cfg.ExportBucket} {
		exists, err := c.mc.BucketExists(ctx, bucket)
		if err != nil {
			return fmt.Errorf("storage: check bucket %q: %w", bucket, err)
		}
		if exists {
			continue
		}
		if err := c.mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: c.cfg.Region}); err != nil {
			return fmt.Errorf("storage: create bucket %q: %w", bucket, err)
		}
	}
	return nil
}

func (c *Client) Healthy(ctx context.Context) error {
	_, err := c.mc.BucketExists(ctx, c.cfg.MediaBucket)
	return err
}

func (c *Client) MediaBucket() string  { return c.cfg.MediaBucket }
func (c *Client) PublicBucket() string { return c.cfg.PublicBucket }
func (c *Client) ExportBucket() string { return c.cfg.ExportBucket }

// PresignPut returns a URL the client can PUT a whole object to.
func (c *Client) PresignPut(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	u, err := c.mc.PresignedPutObject(ctx, bucket, key, ttl)
	if err != nil {
		return "", fmt.Errorf("storage: presign put: %w", err)
	}
	return u.String(), nil
}

// PresignGet returns a download URL. When a CDN is configured the object is
// served through it instead, keeping origin traffic off the media gateway (§73).
func (c *Client) PresignGet(ctx context.Context, bucket, key, downloadName string, ttl time.Duration) (string, error) {
	if c.cfg.CDNBaseURL != "" && bucket == c.cfg.PublicBucket {
		return strings.TrimRight(c.cfg.CDNBaseURL, "/") + "/" + key, nil
	}

	params := url.Values{}
	if downloadName != "" {
		params.Set("response-content-disposition",
			fmt.Sprintf(`attachment; filename="%s"`, sanitizeFilename(downloadName)))
	}
	u, err := c.mc.PresignedGetObject(ctx, bucket, key, ttl, params)
	if err != nil {
		return "", fmt.Errorf("storage: presign get: %w", err)
	}
	return u.String(), nil
}

// NewMultipartUpload starts a multipart upload for a large file (§19).
func (c *Client) NewMultipartUpload(ctx context.Context, bucket, key, contentType string) (string, error) {
	core := minio.Core{Client: c.mc}
	uploadID, err := core.NewMultipartUpload(ctx, bucket, key, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", fmt.Errorf("storage: start multipart upload: %w", err)
	}
	return uploadID, nil
}

// PresignPart returns a URL for one part of a multipart upload.
func (c *Client) PresignPart(ctx context.Context, bucket, key, uploadID string, partNumber int, ttl time.Duration) (string, error) {
	params := url.Values{}
	params.Set("uploadId", uploadID)
	params.Set("partNumber", fmt.Sprintf("%d", partNumber))

	u, err := c.mc.Presign(ctx, "PUT", bucket, key, ttl, params)
	if err != nil {
		return "", fmt.Errorf("storage: presign part: %w", err)
	}
	return u.String(), nil
}

// CompleteMultipartUpload assembles the parts into the final object.
func (c *Client) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []Part) error {
	core := minio.Core{Client: c.mc}
	complete := make([]minio.CompletePart, 0, len(parts))
	for _, part := range parts {
		complete = append(complete, minio.CompletePart{
			PartNumber: part.PartNumber,
			ETag:       strings.Trim(part.ETag, `"`),
		})
	}
	_, err := core.CompleteMultipartUpload(ctx, bucket, key, uploadID, complete, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("storage: complete multipart upload: %w", err)
	}
	return nil
}

func (c *Client) AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error {
	core := minio.Core{Client: c.mc}
	if err := core.AbortMultipartUpload(ctx, bucket, key, uploadID); err != nil {
		return fmt.Errorf("storage: abort multipart upload: %w", err)
	}
	return nil
}

// Put stores an object directly. Used by workers writing derived variants and
// export archives, never by the request path.
func (c *Client) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	_, err := c.mc.PutObject(ctx, bucket, key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return fmt.Errorf("storage: put object: %w", err)
	}
	return nil
}

func (c *Client) Get(ctx context.Context, bucket, key string) (io.ReadCloser, error) {
	obj, err := c.mc.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("storage: get object: %w", err)
	}
	return obj, nil
}

// GetRange reads a byte range, which is how the media pipeline sniffs magic
// bytes without downloading a multi-gigabyte object (§72).
func (c *Client) GetRange(ctx context.Context, bucket, key string, offset, length int64) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, offset+length-1); err != nil {
		return nil, fmt.Errorf("storage: set range: %w", err)
	}
	obj, err := c.mc.GetObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, fmt.Errorf("storage: get object range: %w", err)
	}
	return obj, nil
}

type ObjectInfo struct {
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

func (c *Client) Stat(ctx context.Context, bucket, key string) (ObjectInfo, error) {
	info, err := c.mc.StatObject(ctx, bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("storage: stat object: %w", err)
	}
	return ObjectInfo{
		Size:         info.Size,
		ContentType:  info.ContentType,
		ETag:         info.ETag,
		LastModified: info.LastModified,
	}, nil
}

func (c *Client) Remove(ctx context.Context, bucket, key string) error {
	if err := c.mc.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("storage: remove object: %w", err)
	}
	return nil
}

// sanitizeFilename strips characters that would let a filename break out of
// the Content-Disposition header.
func sanitizeFilename(name string) string {
	replacer := strings.NewReplacer(`"`, "", `\`, "", "\r", "", "\n", "", ";", "")
	cleaned := replacer.Replace(name)
	if len(cleaned) > 200 {
		cleaned = cleaned[:200]
	}
	if cleaned == "" {
		return "download"
	}
	return cleaned
}
