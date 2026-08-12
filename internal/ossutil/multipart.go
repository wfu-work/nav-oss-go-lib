package ossutil

import (
	"errors"
	"strings"

	"github.com/wfu-work/nav-oss-go-lib/core"
)

const (
	MaxMultipartParts = 10_000
	MinS3PartSize     = 5 * 1024 * 1024
	MaxS3PartSize     = 5 * 1024 * 1024 * 1024
)

// ValidateCompletedParts enforces the portable S3 multipart constraints used
// by both provider implementations.
func ValidateCompletedParts(parts []core.CompletedPart) error {
	if len(parts) == 0 || len(parts) > MaxMultipartParts {
		return errors.New("one to 10000 parts are required")
	}
	previous := 0
	for index, part := range parts {
		if part.PartNumber <= previous || part.PartNumber > MaxMultipartParts || strings.TrimSpace(part.ETag) == "" || part.Size < 0 || part.Size > MaxS3PartSize {
			return errors.New("parts must be strictly ordered with valid numbers, ETags, and sizes")
		}
		if index < len(parts)-1 && part.Size < MinS3PartSize {
			return errors.New("every part except the last must be at least 5 MiB")
		}
		previous = part.PartNumber
	}
	return nil
}

// NormalizeETag removes optional HTTP quotes and surrounding whitespace.
func NormalizeETag(etag string) string {
	return strings.Trim(strings.TrimSpace(etag), "\"")
}
