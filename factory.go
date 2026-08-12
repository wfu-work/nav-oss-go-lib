package oss

import (
	"context"

	providerfactory "github.com/wfu-work/nav-oss-go-lib/factory"
	localprovider "github.com/wfu-work/nav-oss-go-lib/local"
	s3provider "github.com/wfu-work/nav-oss-go-lib/s3"
)

// New constructs a configured provider through the compatibility facade.
func New(ctx context.Context, config Config) (Store, error) {
	return providerfactory.New(ctx, config)
}

// NewLocal creates a filesystem-backed provider.
func NewLocal(config LocalConfig) (*LocalStore, error) {
	return localprovider.New(config)
}

// NewS3 creates an S3-compatible provider.
func NewS3(ctx context.Context, config S3Config) (*S3Store, error) {
	return s3provider.New(ctx, config)
}
