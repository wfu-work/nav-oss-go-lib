package s3

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
)

const (
	maxPresignTTL = 7 * 24 * time.Hour
)

// Store stores objects in Amazon S3 or an S3-compatible service.
type Store struct {
	client    *minio.Client
	core      *minio.Core
	bucket    string
	prefix    string
	region    string
	partSize  uint64
	transport http.RoundTripper
}

// New creates an S3-compatible store and optionally verifies or creates its
// bucket. Protocol signing and wire behavior are delegated to minio-go.
func New(ctx context.Context, config S3Config) (*Store, error) {
	if ctx == nil {
		return nil, newError("new-s3", ProviderS3, "", ErrInvalidConfig, errors.New("context is required"))
	}
	endpoint, secure, err := normalizeS3Endpoint(config.Endpoint, config.Secure)
	if err != nil {
		return nil, newError("new-s3", ProviderS3, "", ErrInvalidConfig, err)
	}
	bucket := strings.TrimSpace(config.Bucket)
	if err = s3utils.CheckValidBucketName(bucket); err != nil {
		return nil, newError("new-s3", ProviderS3, "", ErrInvalidConfig, err)
	}
	prefix, err := normalizeS3Prefix(config.Prefix)
	if err != nil {
		return nil, err
	}
	if config.MaxRetries < 0 {
		return nil, newError("new-s3", ProviderS3, "", ErrInvalidConfig, errors.New("maxRetries cannot be negative"))
	}
	if config.PartSize != 0 && (config.PartSize < minS3PartSize || config.PartSize > maxS3PartSize) {
		return nil, newError("new-s3", ProviderS3, "", ErrInvalidConfig, errors.New("partSize must be between 5 MiB and 5 GiB"))
	}
	creds, err := s3Credentials(config)
	if err != nil {
		return nil, err
	}
	lookup, err := s3BucketLookup(config.BucketLookup)
	if err != nil {
		return nil, err
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Transport:    config.Transport,
		Region:       strings.TrimSpace(config.Region),
		BucketLookup: lookup,
		MaxRetries:   config.MaxRetries,
	})
	if err != nil {
		return nil, newError("new-s3/client", ProviderS3, "", ErrInvalidConfig, err)
	}
	client.SetAppInfo("nav-oss-go-lib", Version)
	store := &Store{
		client:    client,
		core:      &minio.Core{Client: client},
		bucket:    bucket,
		prefix:    prefix,
		region:    strings.TrimSpace(config.Region),
		partSize:  config.PartSize,
		transport: config.Transport,
	}
	if config.SkipBucketCheck {
		return store, nil
	}
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, mapS3Error("new-s3/check-bucket", "", err)
	}
	if exists {
		return store, nil
	}
	if !config.AutoCreateBucket {
		return nil, newError("new-s3/check-bucket", ProviderS3, "", ErrNotFound, errors.New("bucket does not exist"))
	}
	if err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: store.region}); err != nil {
		// Another process may have created the bucket between the check and
		// creation request.
		if exists, checkErr := client.BucketExists(ctx, bucket); checkErr == nil && exists {
			return store, nil
		}
		return nil, mapS3Error("new-s3/create-bucket", "", err)
	}
	return store, nil
}

func (s *Store) Provider() Provider {
	return ProviderS3
}

func (s *Store) Capabilities() Capabilities {
	return Capabilities(CapabilityRangeRead | CapabilityList | CapabilityCopy | CapabilityMultipart | CapabilityPresignGet | CapabilityPresignPut)
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	if reader == nil {
		return ObjectInfo{}, newError("put", ProviderS3, key, ErrInvalidConfig, errors.New("reader is required"))
	}
	if size < -1 {
		return ObjectInfo{}, newError("put", ProviderS3, key, ErrInvalidConfig, errors.New("size must be -1 or greater"))
	}
	if err := validateS3PartSize(options.PartSize); err != nil {
		return ObjectInfo{}, newError("put", ProviderS3, key, ErrInvalidConfig, err)
	}
	object, err := s.objectKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	putOptions := s3PutOptions(options, s.partSize)
	if _, err = s.client.PutObject(ctx, s.bucket, object, reader, size, putOptions); err != nil {
		return ObjectInfo{}, mapS3Error("put", key, err)
	}
	info, err := s.Stat(ctx, key)
	if err != nil {
		return ObjectInfo{}, newError("put/stat", ProviderS3, key, ErrPartialFailure, err)
	}
	return info, nil
}

func (s *Store) Open(ctx context.Context, key string, options OpenOptions) (io.ReadCloser, error) {
	if options.Offset < 0 || options.Length < -1 {
		return nil, newError("open", ProviderS3, key, ErrInvalidRange, nil)
	}
	object, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	getOptions := minio.GetObjectOptions{VersionID: options.Version}
	if options.Offset > 0 || options.Length > 0 {
		stat, statErr := s.client.StatObject(ctx, s.bucket, object, minio.StatObjectOptions{VersionID: options.Version})
		if statErr != nil {
			return nil, mapS3Error("open/stat", key, statErr)
		}
		if options.Offset > stat.Size {
			return nil, newError("open", ProviderS3, key, ErrInvalidRange, nil)
		}
		if options.Offset == stat.Size {
			return io.NopCloser(strings.NewReader("")), nil
		}
		if options.Length > 0 {
			if options.Offset > math.MaxInt64-(options.Length-1) {
				return nil, newError("open", ProviderS3, key, ErrInvalidRange, nil)
			}
			end := options.Offset + options.Length - 1
			if end >= stat.Size {
				end = stat.Size - 1
			}
			if err = getOptions.SetRange(options.Offset, end); err != nil {
				return nil, newError("open", ProviderS3, key, ErrInvalidRange, err)
			}
		} else if err = getOptions.SetRange(options.Offset, 0); err != nil {
			return nil, newError("open", ProviderS3, key, ErrInvalidRange, err)
		}
	}
	reader, _, _, err := s.core.GetObject(ctx, s.bucket, object, getOptions)
	if err != nil {
		return nil, mapS3Error("open", key, err)
	}
	return reader, nil
}

func (s *Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	object, err := s.objectKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	info, err := s.client.StatObject(ctx, s.bucket, object, minio.StatObjectOptions{})
	if err != nil {
		return ObjectInfo{}, mapS3Error("stat", key, err)
	}
	return objectInfoFromS3(key, info), nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	object, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if err = s.client.RemoveObject(ctx, s.bucket, object, minio.RemoveObjectOptions{}); err != nil {
		return mapS3Error("delete", key, err)
	}
	return nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

func (s *Store) List(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := validatePrefix(options.Prefix); err != nil {
		return ListResult{}, err
	}
	if options.Cursor != "" {
		if err := validatePrefix(options.Cursor); err != nil {
			return ListResult{}, err
		}
	}
	limit := normalizeListLimit(options.Limit)
	listContext, cancel := context.WithCancel(ctx)
	defer cancel()
	channel := s.client.ListObjects(listContext, s.bucket, minio.ListObjectsOptions{
		Prefix:       s.prefix + options.Prefix,
		Recursive:    options.Recursive,
		MaxKeys:      limit + 1,
		StartAfter:   s.prefix + options.Cursor,
		WithMetadata: true,
	})
	items := make([]ObjectInfo, 0, limit+1)
	stoppedEarly := false
	for object := range channel {
		if object.Err != nil {
			if stoppedEarly && errors.Is(object.Err, context.Canceled) {
				continue
			}
			return ListResult{}, mapS3Error("list", options.Prefix, object.Err)
		}
		key, ok := s.logicalKey(object.Key)
		if !ok || key == "" {
			continue
		}
		isPrefix := !options.Recursive && strings.HasSuffix(key, "/") && object.LastModified.IsZero()
		items = append(items, objectInfoFromS3(key, object))
		items[len(items)-1].IsPrefix = isPrefix
		if len(items) > limit && !stoppedEarly {
			stoppedEarly = true
			cancel()
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key < items[j].Key })
	result := ListResult{Items: items}
	if len(result.Items) > limit {
		result.Items = result.Items[:limit]
		result.HasMore = true
		result.NextCursor = result.Items[len(result.Items)-1].Key
	}
	return result, nil
}

func (s *Store) Copy(ctx context.Context, sourceKey, targetKey string, options CopyOptions) (ObjectInfo, error) {
	sourceObject, err := s.objectKey(sourceKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	targetObject, err := s.objectKey(targetKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	sourceInfo, err := s.Stat(ctx, sourceKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	metadata := cloneMetadata(sourceInfo.Metadata)
	if options.ReplaceMetadata {
		metadata = cloneMetadata(options.Metadata)
	}
	contentType := sourceInfo.ContentType
	if options.ContentType != "" {
		contentType = options.ContentType
	}
	destination := minio.CopyDestOptions{
		Bucket:             s.bucket,
		Object:             targetObject,
		UserMetadata:       metadata,
		ReplaceMetadata:    true,
		ContentType:        contentType,
		ContentEncoding:    sourceInfo.ContentEncoding,
		ContentDisposition: sourceInfo.ContentDisposition,
		CacheControl:       sourceInfo.CacheControl,
		PartSize:           s.partSize,
	}
	source := minio.CopySrcOptions{Bucket: s.bucket, Object: sourceObject, Start: -1, End: -1}
	if _, err = s.client.ComposeObject(ctx, destination, source); err != nil {
		return ObjectInfo{}, mapS3Error("copy", sourceKey, err)
	}
	return s.Stat(ctx, targetKey)
}

func (s *Store) Health(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return mapS3Error("health", "", err)
	}
	if !exists {
		return newError("health", ProviderS3, "", ErrNotFound, errors.New("bucket does not exist"))
	}
	return nil
}

func (s *Store) Close() error {
	if closer, ok := s.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	return nil
}

func (s *Store) objectKey(key string) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	return s.prefix + key, nil
}

func (s *Store) logicalKey(object string) (string, bool) {
	if !strings.HasPrefix(object, s.prefix) {
		return "", false
	}
	return strings.TrimPrefix(object, s.prefix), true
}

func normalizeS3Endpoint(endpoint string, secure bool) (string, bool, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", false, errors.New("endpoint is required")
	}
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return "", false, err
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", false, errors.New("endpoint scheme must be http or https")
		}
		if parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", false, errors.New("endpoint must not contain credentials, a path, query, or fragment")
		}
		return parsed.Host, parsed.Scheme == "https", nil
	}
	parsed, err := url.Parse("//" + endpoint)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false, errors.New("invalid endpoint")
	}
	return parsed.Host, secure, nil
}

func normalizeS3Prefix(prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", nil
	}
	if err := validatePrefix(prefix); err != nil {
		return "", err
	}
	return strings.TrimSuffix(prefix, "/") + "/", nil
}

func s3Credentials(config S3Config) (*credentials.Credentials, error) {
	mode := config.CredentialMode
	if mode == "" {
		if config.AccessKeyID != "" || config.SecretAccessKey != "" || config.SessionToken != "" {
			mode = CredentialStatic
		} else {
			mode = CredentialEnv
		}
	}
	switch mode {
	case CredentialStatic:
		if config.AccessKeyID == "" || config.SecretAccessKey == "" {
			return nil, newError("new-s3/credentials", ProviderS3, "", ErrInvalidConfig, errors.New("static credentials require accessKeyId and secretAccessKey"))
		}
		return credentials.NewStaticV4(config.AccessKeyID, config.SecretAccessKey, config.SessionToken), nil
	case CredentialEnv:
		environment := credentials.NewEnvAWS()
		value, err := environment.Get()
		if err != nil {
			return nil, newError("new-s3/credentials", ProviderS3, "", ErrInvalidConfig, err)
		}
		if value.SignerType.IsAnonymous() {
			return nil, newError("new-s3/credentials", ProviderS3, "", ErrInvalidConfig, errors.New("AWS environment credentials are not set"))
		}
		return environment, nil
	case CredentialIAM:
		return credentials.NewIAM(strings.TrimSpace(config.IAMEndpoint)), nil
	case CredentialAnonymous:
		return credentials.NewStaticV4("", "", ""), nil
	default:
		return nil, newError("new-s3/credentials", ProviderS3, "", ErrInvalidConfig, errors.New("unsupported credential mode"))
	}
}

func s3BucketLookup(lookup BucketLookup) (minio.BucketLookupType, error) {
	switch lookup {
	case "", BucketLookupAuto:
		return minio.BucketLookupAuto, nil
	case BucketLookupDNS:
		return minio.BucketLookupDNS, nil
	case BucketLookupPath:
		return minio.BucketLookupPath, nil
	default:
		return minio.BucketLookupAuto, newError("new-s3/bucket-lookup", ProviderS3, "", ErrInvalidConfig, errors.New("unsupported bucket lookup mode"))
	}
}

func s3PutOptions(options PutOptions, defaultPartSize uint64) minio.PutObjectOptions {
	partSize := options.PartSize
	if partSize == 0 {
		partSize = defaultPartSize
	}
	return minio.PutObjectOptions{
		UserMetadata:       cloneMetadata(options.Metadata),
		ContentType:        options.ContentType,
		ContentEncoding:    options.ContentEncoding,
		ContentDisposition: options.ContentDisposition,
		CacheControl:       options.CacheControl,
		StorageClass:       options.StorageClass,
		PartSize:           partSize,
		DisableMultipart:   options.DisableMultipart,
		SendContentMd5:     options.SendContentMD5,
	}
}

func validateS3PartSize(partSize uint64) error {
	if partSize != 0 && (partSize < minS3PartSize || partSize > maxS3PartSize) {
		return errors.New("partSize must be between 5 MiB and 5 GiB")
	}
	return nil
}

func objectInfoFromS3(key string, info minio.ObjectInfo) ObjectInfo {
	contentType := info.ContentType
	if contentType == "" {
		contentType = info.Metadata.Get("Content-Type")
	}
	metadata := make(map[string]string, len(info.UserMetadata))
	for name, value := range info.UserMetadata {
		metadata[name] = value
	}
	for name, values := range info.Metadata {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-meta-") && len(values) > 0 {
			metadata[name[len("x-amz-meta-"):]] = values[0]
		}
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	return ObjectInfo{
		Key:                key,
		Size:               info.Size,
		ETag:               normalizeETag(info.ETag),
		ContentType:        contentType,
		ContentEncoding:    info.Metadata.Get("Content-Encoding"),
		ContentDisposition: info.Metadata.Get("Content-Disposition"),
		CacheControl:       info.Metadata.Get("Cache-Control"),
		LastModified:       info.LastModified.UTC(),
		Metadata:           metadata,
		StorageClass:       info.StorageClass,
		Version:            info.VersionID,
	}
}
