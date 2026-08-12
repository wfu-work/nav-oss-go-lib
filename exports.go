package oss

import (
	"context"

	"github.com/wfu-work/nav-oss-go-lib/core"
	localprovider "github.com/wfu-work/nav-oss-go-lib/local"
	s3provider "github.com/wfu-work/nav-oss-go-lib/s3"
)

// Public contracts remain available from the root package for compatibility.
// New code may import core and a provider package directly.
type (
	Provider          = core.Provider
	Capability        = core.Capability
	Capabilities      = core.Capabilities
	PutOptions        = core.PutOptions
	OpenOptions       = core.OpenOptions
	ListOptions       = core.ListOptions
	CopyOptions       = core.CopyOptions
	ObjectInfo        = core.ObjectInfo
	ListResult        = core.ListResult
	MultipartUpload   = core.MultipartUpload
	CompletedPart     = core.CompletedPart
	PresignGetOptions = core.PresignGetOptions
	PresignPutOptions = core.PresignPutOptions
	Config            = core.Config
	LocalConfig       = core.LocalConfig
	CredentialMode    = core.CredentialMode
	BucketLookup      = core.BucketLookup
	S3Config          = core.S3Config
	Store             = core.Store
	Copier            = core.Copier
	MultipartStore    = core.MultipartStore
	Presigner         = core.Presigner
	Error             = core.Error
	LocalStore        = localprovider.Store
	S3Store           = s3provider.Store
)

const (
	Version                    = core.Version
	ProviderLocal              = core.ProviderLocal
	ProviderS3                 = core.ProviderS3
	CapabilityRangeRead        = core.CapabilityRangeRead
	CapabilityList             = core.CapabilityList
	CapabilityCopy             = core.CapabilityCopy
	CapabilityMultipart        = core.CapabilityMultipart
	CapabilityPresignGet       = core.CapabilityPresignGet
	CapabilityPresignPut       = core.CapabilityPresignPut
	CapabilityConditionalWrite = core.CapabilityConditionalWrite
	CredentialStatic           = core.CredentialStatic
	CredentialEnv              = core.CredentialEnv
	CredentialIAM              = core.CredentialIAM
	CredentialAnonymous        = core.CredentialAnonymous
	BucketLookupAuto           = core.BucketLookupAuto
	BucketLookupDNS            = core.BucketLookupDNS
	BucketLookupPath           = core.BucketLookupPath
)

var (
	ErrNotFound       = core.ErrNotFound
	ErrAccessDenied   = core.ErrAccessDenied
	ErrConflict       = core.ErrConflict
	ErrInvalidConfig  = core.ErrInvalidConfig
	ErrInvalidKey     = core.ErrInvalidKey
	ErrInvalidRange   = core.ErrInvalidRange
	ErrInvalidPart    = core.ErrInvalidPart
	ErrUnsupported    = core.ErrUnsupported
	ErrTemporary      = core.ErrTemporary
	ErrPartialFailure = core.ErrPartialFailure
)

// Move delegates the provider-neutral copy-then-delete operation to core.
func Move(ctx context.Context, store Store, sourceKey, targetKey string, options CopyOptions) (ObjectInfo, error) {
	return core.Move(ctx, store, sourceKey, targetKey, options)
}
