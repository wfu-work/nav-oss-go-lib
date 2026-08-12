package core

import (
	"net/http"
	"time"
)

// Provider identifies a storage implementation.
type Provider string

const (
	ProviderLocal Provider = "local"
	ProviderS3    Provider = "s3"
)

// Capability describes an optional storage feature.
type Capability uint64

const (
	CapabilityRangeRead Capability = 1 << iota
	CapabilityList
	CapabilityCopy
	CapabilityMultipart
	CapabilityPresignGet
	CapabilityPresignPut
	CapabilityConditionalWrite
)

// Capabilities is a bit set of supported features.
type Capabilities uint64

// Has reports whether a capability is supported.
func (c Capabilities) Has(capability Capability) bool {
	return uint64(c)&uint64(capability) != 0
}

// PutOptions controls an object write.
type PutOptions struct {
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	PartSize           uint64            `json:"partSize,omitempty"`
	DisableMultipart   bool              `json:"disableMultipart,omitempty"`
	SendContentMD5     bool              `json:"sendContentMd5,omitempty"`
}

// OpenOptions selects a byte range. Length 0 or -1 means read to EOF.
type OpenOptions struct {
	Offset  int64  `json:"offset"`
	Length  int64  `json:"length"`
	Version string `json:"version,omitempty"`
}

// ListOptions controls a page of object listings. Cursor is the last key from
// the previous page.
type ListOptions struct {
	Prefix    string `json:"prefix,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Recursive bool   `json:"recursive"`
}

// CopyOptions controls an object copy.
type CopyOptions struct {
	ReplaceMetadata bool              `json:"replaceMetadata"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	ContentType     string            `json:"contentType,omitempty"`
}

// ObjectInfo is provider-neutral object metadata.
type ObjectInfo struct {
	Key                string            `json:"key"`
	Size               int64             `json:"size"`
	ETag               string            `json:"etag,omitempty"`
	SHA256             string            `json:"sha256,omitempty"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	LastModified       time.Time         `json:"lastModified"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	StorageClass       string            `json:"storageClass,omitempty"`
	Version            string            `json:"version,omitempty"`
	IsPrefix           bool              `json:"isPrefix"`
}

// ListResult is one deterministic page of objects and common prefixes.
type ListResult struct {
	Items      []ObjectInfo `json:"items"`
	NextCursor string       `json:"nextCursor,omitempty"`
	HasMore    bool         `json:"hasMore"`
}

// MultipartUpload identifies a resumable upload.
type MultipartUpload struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"createdAt"`
}

// CompletedPart identifies one uploaded part.
type CompletedPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

// PresignGetOptions controls a temporary download URL.
type PresignGetOptions struct {
	Expiry                     time.Duration `json:"expiry"`
	Version                    string        `json:"version,omitempty"`
	ResponseContentType        string        `json:"responseContentType,omitempty"`
	ResponseContentDisposition string        `json:"responseContentDisposition,omitempty"`
}

// PresignPutOptions controls a temporary upload URL.
type PresignPutOptions struct {
	Expiry time.Duration `json:"expiry"`
}

// Config creates a storage implementation.
type Config struct {
	Provider Provider    `json:"provider"`
	Local    LocalConfig `json:"local,omitempty"`
	S3       S3Config    `json:"s3,omitempty"`
}

// LocalConfig configures filesystem-backed object storage.
type LocalConfig struct {
	RootDir  string `json:"rootDir"`
	DirMode  uint32 `json:"dirMode,omitempty"`
	FileMode uint32 `json:"fileMode,omitempty"`
}

// CredentialMode selects how S3 credentials are loaded.
type CredentialMode string

const (
	CredentialStatic    CredentialMode = "static"
	CredentialEnv       CredentialMode = "env"
	CredentialIAM       CredentialMode = "iam"
	CredentialAnonymous CredentialMode = "anonymous"
)

// BucketLookup controls path-style versus virtual-host-style requests.
type BucketLookup string

const (
	BucketLookupAuto BucketLookup = "auto"
	BucketLookupDNS  BucketLookup = "dns"
	BucketLookupPath BucketLookup = "path"
)

// S3Config configures an Amazon S3 compatible endpoint. Endpoint can be a
// host:port combined with Secure, or an explicit http(s) URL.
type S3Config struct {
	Endpoint         string            `json:"endpoint"`
	Secure           bool              `json:"secure"`
	Region           string            `json:"region,omitempty"`
	Bucket           string            `json:"bucket"`
	Prefix           string            `json:"prefix,omitempty"`
	AccessKeyID      string            `json:"accessKeyId,omitempty"`
	SecretAccessKey  string            `json:"secretAccessKey,omitempty"`
	SessionToken     string            `json:"sessionToken,omitempty"`
	CredentialMode   CredentialMode    `json:"credentialMode,omitempty"`
	IAMEndpoint      string            `json:"iamEndpoint,omitempty"`
	BucketLookup     BucketLookup      `json:"bucketLookup,omitempty"`
	AutoCreateBucket bool              `json:"autoCreateBucket"`
	SkipBucketCheck  bool              `json:"skipBucketCheck"`
	PartSize         uint64            `json:"partSize,omitempty"`
	MaxRetries       int               `json:"maxRetries,omitempty"`
	Transport        http.RoundTripper `json:"-"`
}
