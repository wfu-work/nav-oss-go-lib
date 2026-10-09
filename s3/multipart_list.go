package s3

import (
	"context"
	"errors"
	"sort"
	"strings"
)

// ListMultipart enumerates incomplete uploads within the configured namespace.
// S3 multipart and part-list permissions are needed to report upload progress.
func (s *Store) ListMultipart(ctx context.Context, prefix string) ([]MultipartInfo, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	items := make([]MultipartInfo, 0)
	keyMarker, idMarker := "", ""
	seen := make(map[[2]string]bool)
	for {
		if err := ctx.Err(); err != nil {
			return nil, mapS3Error("list-multipart", prefix, err)
		}
		marker := [2]string{keyMarker, idMarker}
		if seen[marker] {
			return nil, newError("list-multipart", ProviderS3, prefix, ErrTemporary, errors.New("S3 returned a non-advancing upload marker"))
		}
		seen[marker] = true
		// Some S3-compatible servers treat a multipart prefix as an exact key.
		// Walk bucket pages and filter logical keys below to avoid missing uploads.
		result, err := s.core.ListMultipartUploads(ctx, s.bucket, "", keyMarker, idMarker, "", 1000)
		if err != nil {
			return nil, mapS3Error("list-multipart", prefix, err)
		}
		for _, pending := range result.Uploads {
			key, ok := s.logicalKey(pending.Key)
			if !ok || key == "" || !strings.HasPrefix(key, prefix) {
				continue
			}
			// The unified Store cannot address keys outside its key contract.
			if validateObjectKey(key) != nil {
				continue
			}
			upload := MultipartUpload{ID: pending.UploadID, Key: key, CreatedAt: pending.Initiated.UTC()}
			parts, partErr := s.listS3Parts(ctx, upload)
			if errors.Is(partErr, ErrNotFound) {
				continue // Completed or aborted since the upload listing.
			}
			if partErr != nil {
				return nil, partErr
			}
			info := MultipartInfo{MultipartUpload: upload, PartCount: len(parts), UpdatedAt: upload.CreatedAt}
			for _, part := range parts {
				info.UploadedBytes += part.Size
				if part.LastModified.After(info.UpdatedAt) {
					info.UpdatedAt = part.LastModified.UTC()
				}
			}
			items = append(items, info)
		}
		if !result.IsTruncated {
			break
		}
		keyMarker, idMarker = result.NextKeyMarker, result.NextUploadIDMarker
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Key == items[j].Key {
			return items[i].ID < items[j].ID
		}
		return items[i].Key < items[j].Key
	})
	return items, nil
}
