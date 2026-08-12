package core

import (
	"context"
	"io"
	"net/url"
)

// Store is the small common denominator shared by all providers.
type Store interface {
	Provider() Provider
	Capabilities() Capabilities
	Put(ctx context.Context, key string, reader io.Reader, size int64, options PutOptions) (ObjectInfo, error)
	Open(ctx context.Context, key string, options OpenOptions) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	List(ctx context.Context, options ListOptions) (ListResult, error)
	Health(ctx context.Context) error
	Close() error
}

// Copier is implemented when the provider can copy objects without exposing
// object data to the caller.
type Copier interface {
	Copy(ctx context.Context, sourceKey, targetKey string, options CopyOptions) (ObjectInfo, error)
}

// MultipartStore is implemented by providers with resumable multipart upload.
type MultipartStore interface {
	CreateMultipart(ctx context.Context, key string, options PutOptions) (MultipartUpload, error)
	UploadPart(ctx context.Context, upload MultipartUpload, partNumber int, reader io.Reader, size int64) (CompletedPart, error)
	ListParts(ctx context.Context, upload MultipartUpload) ([]CompletedPart, error)
	CompleteMultipart(ctx context.Context, upload MultipartUpload, parts []CompletedPart) (ObjectInfo, error)
	AbortMultipart(ctx context.Context, upload MultipartUpload) error
}

// Presigner is implemented by providers that can create temporary HTTP URLs.
type Presigner interface {
	PresignGet(ctx context.Context, key string, options PresignGetOptions) (*url.URL, error)
	PresignPut(ctx context.Context, key string, options PresignPutOptions) (*url.URL, error)
}

// Move copies then deletes an object. It is intentionally not presented as an
// atomic operation because object stores generally cannot guarantee that.
func Move(ctx context.Context, store Store, sourceKey, targetKey string, options CopyOptions) (ObjectInfo, error) {
	copier, ok := store.(Copier)
	if !ok {
		return ObjectInfo{}, newError("move", store.Provider(), sourceKey, ErrUnsupported, nil)
	}
	info, err := copier.Copy(ctx, sourceKey, targetKey, options)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err = store.Delete(ctx, sourceKey); err != nil {
		return info, newError("move/delete-source", store.Provider(), sourceKey, ErrPartialFailure, err)
	}
	return info, nil
}
