package local

import (
	"github.com/wfu-work/nav-oss-go-lib/core"
	"github.com/wfu-work/nav-oss-go-lib/internal/ossutil"
)

type (
	LocalConfig     = core.LocalConfig
	Provider        = core.Provider
	Capability      = core.Capability
	Capabilities    = core.Capabilities
	PutOptions      = core.PutOptions
	OpenOptions     = core.OpenOptions
	ListOptions     = core.ListOptions
	CopyOptions     = core.CopyOptions
	ObjectInfo      = core.ObjectInfo
	ListResult      = core.ListResult
	MultipartUpload = core.MultipartUpload
	MultipartInfo   = core.MultipartInfo
	CompletedPart   = core.CompletedPart
)

const (
	ProviderLocal           = core.ProviderLocal
	CapabilityRangeRead     = core.CapabilityRangeRead
	CapabilityList          = core.CapabilityList
	CapabilityCopy          = core.CapabilityCopy
	CapabilityMultipart     = core.CapabilityMultipart
	CapabilityMultipartList = core.CapabilityMultipartList

	internalDirName    = ossutil.InternalDirName
	internalTempPrefix = ossutil.InternalTempPrefix
	maxMultipartParts  = ossutil.MaxMultipartParts
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

func validateObjectKey(key string) error { return ossutil.ValidateObjectKey(key) }
func validatePrefix(prefix string) error { return ossutil.ValidatePrefix(prefix) }
func cloneMetadata(metadata map[string]string) map[string]string {
	return ossutil.CloneMetadata(metadata)
}
func normalizeListLimit(limit int) int { return ossutil.NormalizeListLimit(limit) }
func ensureNoSymlinkComponents(root, path string) error {
	return ossutil.EnsureNoSymlinkComponents(root, path)
}
func validateCompletedParts(parts []CompletedPart) error {
	return ossutil.ValidateCompletedParts(parts)
}
func normalizeETag(etag string) string { return ossutil.NormalizeETag(etag) }
