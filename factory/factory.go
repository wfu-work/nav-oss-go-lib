// Package factory constructs a configured object-storage provider.
package factory

import (
	"context"

	"github.com/wfu-work/nav-oss-go-lib/core"
	"github.com/wfu-work/nav-oss-go-lib/internal/ossutil"
	localprovider "github.com/wfu-work/nav-oss-go-lib/local"
	s3provider "github.com/wfu-work/nav-oss-go-lib/s3"
)

// New constructs and validates the provider selected by config.
func New(ctx context.Context, config core.Config) (core.Store, error) {
	switch config.Provider {
	case core.ProviderLocal:
		return localprovider.New(config.Local)
	case core.ProviderS3:
		return s3provider.New(ctx, config.S3)
	default:
		return nil, ossutil.NewError("new", config.Provider, "", core.ErrInvalidConfig, nil)
	}
}
