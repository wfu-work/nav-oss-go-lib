// Package s3 implements Amazon S3 and S3-compatible object storage using the
// minio-go protocol client.
package s3

import "github.com/wfu-work/nav-oss-go-lib/core"

var (
	_ core.Store          = (*Store)(nil)
	_ core.Copier         = (*Store)(nil)
	_ core.MultipartStore = (*Store)(nil)
	_ core.Presigner      = (*Store)(nil)
)
