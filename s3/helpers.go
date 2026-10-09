package s3

import (
	"github.com/wfu-work/nav-oss-go-lib/core"
	"github.com/wfu-work/nav-oss-go-lib/internal/ossutil"
)

type (
	S3Config          = core.S3Config
	Provider          = core.Provider
	Capability        = core.Capability
	Capabilities      = core.Capabilities
	CredentialMode    = core.CredentialMode
	BucketLookup      = core.BucketLookup
	PutOptions        = core.PutOptions
	OpenOptions       = core.OpenOptions
	ListOptions       = core.ListOptions
	CopyOptions       = core.CopyOptions
	ObjectInfo        = core.ObjectInfo
	ListResult        = core.ListResult
	MultipartUpload   = core.MultipartUpload
	MultipartInfo     = core.MultipartInfo
	CompletedPart     = core.CompletedPart
	PresignGetOptions = core.PresignGetOptions
	PresignPutOptions = core.PresignPutOptions
)

const (
	ProviderS3              = core.ProviderS3
	CapabilityRangeRead     = core.CapabilityRangeRead
	CapabilityList          = core.CapabilityList
	CapabilityCopy          = core.CapabilityCopy
	CapabilityMultipart     = core.CapabilityMultipart
	CapabilityMultipartList = core.CapabilityMultipartList
	CapabilityPresignGet    = core.CapabilityPresignGet
	CapabilityPresignPut    = core.CapabilityPresignPut
	CredentialStatic        = core.CredentialStatic
	CredentialEnv           = core.CredentialEnv
	CredentialIAM           = core.CredentialIAM
	CredentialAnonymous     = core.CredentialAnonymous
	BucketLookupAuto        = core.BucketLookupAuto
	BucketLookupDNS         = core.BucketLookupDNS
	BucketLookupPath        = core.BucketLookupPath
	maxMultipartParts       = ossutil.MaxMultipartParts
	minS3PartSize           = ossutil.MinS3PartSize
	maxS3PartSize           = ossutil.MaxS3PartSize
	Version                 = core.Version
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

func newError(op string, provider Provider, key string, kind, cause error) error {
	return ossutil.NewError(op, provider, key, kind, cause)
}
func errorWithCode(op string, provider Provider, key, code string, kind, cause error) error {
	return ossutil.ErrorWithCode(op, provider, key, code, kind, cause)
}
func validateObjectKey(key string) error { return ossutil.ValidateObjectKey(key) }
func validatePrefix(prefix string) error { return ossutil.ValidatePrefix(prefix) }
func cloneMetadata(metadata map[string]string) map[string]string {
	return ossutil.CloneMetadata(metadata)
}
func normalizeListLimit(limit int) int { return ossutil.NormalizeListLimit(limit) }
func validateCompletedParts(parts []CompletedPart) error {
	return ossutil.ValidateCompletedParts(parts)
}
func normalizeETag(etag string) string { return ossutil.NormalizeETag(etag) }
