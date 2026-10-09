package s3

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
)

// CreateMultipart starts an S3 multipart upload.
func (s *Store) CreateMultipart(ctx context.Context, key string, options PutOptions) (MultipartUpload, error) {
	object, err := s.objectKey(key)
	if err != nil {
		return MultipartUpload{}, err
	}
	uploadID, err := s.core.NewMultipartUpload(ctx, s.bucket, object, s3PutOptions(options, s.partSize))
	if err != nil {
		return MultipartUpload{}, mapS3Error("create-multipart", key, err)
	}
	return MultipartUpload{ID: uploadID, Key: key, CreatedAt: time.Now().UTC()}, nil
}

// UploadPart uploads or replaces one numbered S3 part.
func (s *Store) UploadPart(ctx context.Context, upload MultipartUpload, partNumber int, reader io.Reader, size int64) (CompletedPart, error) {
	object, err := s.s3MultipartObject(upload)
	if err != nil {
		return CompletedPart{}, err
	}
	if reader == nil || partNumber < 1 || partNumber > maxMultipartParts || size < 0 {
		return CompletedPart{}, newError("upload-part", ProviderS3, upload.Key, ErrInvalidPart, errors.New("reader, part number 1..10000, and non-negative size are required"))
	}
	part, err := s.core.PutObjectPart(ctx, s.bucket, object, upload.ID, partNumber, reader, size, minio.PutObjectPartOptions{})
	if err != nil {
		return CompletedPart{}, mapS3Error("upload-part", upload.Key, err)
	}
	return CompletedPart{PartNumber: part.PartNumber, ETag: normalizeETag(part.ETag), Size: part.Size}, nil
}

// ListParts returns every uploaded S3 part in ascending order.
func (s *Store) ListParts(ctx context.Context, upload MultipartUpload) ([]CompletedPart, error) {
	actual, err := s.listS3Parts(ctx, upload)
	if err != nil {
		return nil, err
	}
	parts := make([]CompletedPart, 0, len(actual))
	for _, part := range actual {
		parts = append(parts, CompletedPart{PartNumber: part.PartNumber, ETag: normalizeETag(part.ETag), Size: part.Size})
	}
	return parts, nil
}

func (s *Store) listS3Parts(ctx context.Context, upload MultipartUpload) ([]minio.ObjectPart, error) {
	object, err := s.s3MultipartObject(upload)
	if err != nil {
		return nil, err
	}
	parts := make([]minio.ObjectPart, 0)
	marker := 0
	for {
		result, listErr := s.core.ListObjectParts(ctx, s.bucket, object, upload.ID, marker, 1000)
		if listErr != nil {
			return nil, mapS3Error("list-parts", upload.Key, listErr)
		}
		parts = append(parts, result.ObjectParts...)
		if !result.IsTruncated {
			break
		}
		if result.NextPartNumberMarker <= marker {
			return nil, newError("list-parts", ProviderS3, upload.Key, ErrTemporary, errors.New("S3 returned a non-advancing part marker"))
		}
		marker = result.NextPartNumberMarker
	}
	return parts, nil
}

// CompleteMultipart validates the caller's part list against S3 and commits
// the upload.
func (s *Store) CompleteMultipart(ctx context.Context, upload MultipartUpload, parts []CompletedPart) (ObjectInfo, error) {
	object, err := s.s3MultipartObject(upload)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err = validateCompletedParts(parts); err != nil {
		return ObjectInfo{}, newError("complete-multipart", ProviderS3, upload.Key, ErrInvalidPart, err)
	}
	actual, err := s.ListParts(ctx, upload)
	if err != nil {
		return ObjectInfo{}, err
	}
	actualByNumber := make(map[int]CompletedPart, len(actual))
	for _, part := range actual {
		actualByNumber[part.PartNumber] = part
	}
	completed := make([]minio.CompletePart, 0, len(parts))
	for _, requested := range parts {
		uploaded, ok := actualByNumber[requested.PartNumber]
		if !ok || normalizeETag(requested.ETag) != normalizeETag(uploaded.ETag) || requested.Size != uploaded.Size {
			return ObjectInfo{}, newError("complete-multipart", ProviderS3, upload.Key, ErrInvalidPart, errors.New("part list does not match uploaded data"))
		}
		completed = append(completed, minio.CompletePart{PartNumber: requested.PartNumber, ETag: uploaded.ETag})
	}
	if _, err = s.core.CompleteMultipartUpload(ctx, s.bucket, object, upload.ID, completed, minio.PutObjectOptions{}); err != nil {
		return ObjectInfo{}, mapS3Error("complete-multipart", upload.Key, err)
	}
	return s.Stat(ctx, upload.Key)
}

// AbortMultipart aborts an S3 upload. Missing uploads are treated as already
// aborted, making this operation idempotent.
func (s *Store) AbortMultipart(ctx context.Context, upload MultipartUpload) error {
	object, err := s.s3MultipartObject(upload)
	if err != nil {
		return err
	}
	if err = s.core.AbortMultipartUpload(ctx, s.bucket, object, upload.ID); err != nil {
		mapped := mapS3Error("abort-multipart", upload.Key, err)
		if errors.Is(mapped, ErrNotFound) {
			return nil
		}
		return mapped
	}
	return nil
}

// PresignGet creates a temporary download URL.
func (s *Store) PresignGet(ctx context.Context, key string, options PresignGetOptions) (*url.URL, error) {
	object, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	if err = validatePresignExpiry(options.Expiry); err != nil {
		return nil, newError("presign-get", ProviderS3, key, ErrInvalidConfig, err)
	}
	parameters := make(url.Values)
	if options.Version != "" {
		parameters.Set("versionId", options.Version)
	}
	if options.ResponseContentType != "" {
		parameters.Set("response-content-type", options.ResponseContentType)
	}
	if options.ResponseContentDisposition != "" {
		parameters.Set("response-content-disposition", options.ResponseContentDisposition)
	}
	signed, err := s.client.PresignedGetObject(ctx, s.bucket, object, options.Expiry, parameters)
	if err != nil {
		return nil, mapS3Error("presign-get", key, err)
	}
	return signed, nil
}

// PresignPut creates a temporary upload URL.
func (s *Store) PresignPut(ctx context.Context, key string, options PresignPutOptions) (*url.URL, error) {
	object, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	if err = validatePresignExpiry(options.Expiry); err != nil {
		return nil, newError("presign-put", ProviderS3, key, ErrInvalidConfig, err)
	}
	signed, err := s.client.PresignedPutObject(ctx, s.bucket, object, options.Expiry)
	if err != nil {
		return nil, mapS3Error("presign-put", key, err)
	}
	return signed, nil
}

func (s *Store) s3MultipartObject(upload MultipartUpload) (string, error) {
	if strings.TrimSpace(upload.ID) == "" {
		return "", newError("validate-upload", ProviderS3, upload.Key, ErrInvalidPart, errors.New("upload ID is required"))
	}
	return s.objectKey(upload.Key)
}

func validatePresignExpiry(expiry time.Duration) error {
	if expiry <= 0 || expiry > maxPresignTTL {
		return errors.New("expiry must be greater than zero and no more than seven days")
	}
	return nil
}
