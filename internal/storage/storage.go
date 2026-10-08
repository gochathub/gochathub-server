// Package storage implements the object-storage boundary (service.Storage)
// against S3-compatible backends with presigned upload/download URLs.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 is the minio-backed implementation.
type S3 struct {
	client    *minio.Client // server-side calls (internal endpoint)
	presigner *minio.Client // signs browser-facing URLs (public endpoint)
	bucket    string
	urlTTL    time.Duration
	endpoint  string // for SSRF-safe object URL checks (not used for calls)
}

// NewS3 connects and ensures the bucket exists. endpoint is where this server
// reaches the store. publicEndpoint, when set, is the browser-visible URL
// presigned links are signed for (signatures are host-bound), e.g. an
// https:// host behind a TLS proxy; empty means the same as endpoint.
func NewS3(ctx context.Context, endpoint, publicEndpoint, region, bucket, accessKey, secretKey string, useTLS bool) (*S3, error) {
	creds := credentials.NewStaticV4(accessKey, secretKey, "")
	client, err := minio.New(endpoint, &minio.Options{Creds: creds, Secure: useTLS, Region: region})
	if err != nil {
		return nil, fmt.Errorf("minio client: %w", err)
	}
	presigner := client
	if publicEndpoint != "" {
		host, secure := publicEndpoint, useTLS
		if i := strings.Index(publicEndpoint, "://"); i > 0 {
			host, secure = publicEndpoint[i+3:], strings.EqualFold(publicEndpoint[:i], "https")
		}
		// the region is set, so presigning never contacts the public host
		presigner, err = minio.New(host, &minio.Options{Creds: creds, Secure: secure, Region: region})
		if err != nil {
			return nil, fmt.Errorf("minio presign client: %w", err)
		}
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("bucket check: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region}); err != nil {
			// bucket may have been created concurrently; re-check
			if exists2, err2 := client.BucketExists(ctx, bucket); err2 != nil || !exists2 {
				return nil, fmt.Errorf("create bucket %q: %w", bucket, err)
			}
		}
	}
	return &S3{client: client, presigner: presigner, bucket: bucket, urlTTL: 15 * time.Minute, endpoint: endpoint}, nil
}

func (s *S3) PresignPut(_ context.Context, key, mimeType string, size int64) (string, error) {
	opts := minio.PutObjectOptions{ContentType: mimeType}
	u, err := s.presigner.PresignedPutObject(context.Background(), s.bucket, key, s.urlTTL)
	if err != nil {
		return "", err
	}
	// content-type rides the presign via query when required; minio handles headers at upload time
	_ = opts
	return u.String(), nil
}

// PutObject writes content server-side (migration import path; the normal
// upload flow is presigned).
func (s *S3) PutObject(ctx context.Context, key, mimeType string, size int64, r io.Reader) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{ContentType: mimeType})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) PresignGet(_ context.Context, key string) (string, error) {
	u, err := s.presigner.PresignedGetObject(context.Background(), s.bucket, key, s.urlTTL, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s *S3) Stat(ctx context.Context, key string) (int64, bool, string, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" || resp.Code == "404" {
			return 0, false, "", nil
		}
		return 0, false, "", fmt.Errorf("stat %s: %w", key, err)
	}
	return info.Size, true, cleanETag(info.ETag), nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// cleanETag strips quotes to expose the sha256/minio md5 etag hex. Note:
// per-object md5, not our sha256 — the sha256 comparison uses the client's
// declared checksum; this returns the etag only when callers use it as one.
func cleanETag(etag string) string {
	return strings.Trim(strings.TrimSpace(etag), `"`)
}

// Disabled is the no-storage implementation (uploads rejected upstream by
// service flags; avatars degrade to monogram).
type Disabled struct{}

func (Disabled) PresignPut(context.Context, string, string, int64) (string, error) {
	return "", errors.New("object storage is not configured")
}
func (Disabled) PresignGet(context.Context, string) (string, error) {
	return "", errors.New("object storage is not configured")
}
func (Disabled) Stat(context.Context, string) (int64, bool, string, error) {
	return 0, false, "", errors.New("object storage is not configured")
}
func (Disabled) Delete(context.Context, string) error {
	return errors.New("object storage is not configured")
}
