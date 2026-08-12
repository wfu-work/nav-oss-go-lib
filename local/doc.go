// Package local implements durable filesystem-backed object storage.
package local

import "github.com/wfu-work/nav-oss-go-lib/core"

var (
	_ core.Store          = (*Store)(nil)
	_ core.Copier         = (*Store)(nil)
	_ core.MultipartStore = (*Store)(nil)
)
