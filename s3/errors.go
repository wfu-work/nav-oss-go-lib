package s3

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/minio/minio-go/v7"
)

func mapS3Error(op, key string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return newError(op, ProviderS3, key, ErrTemporary, err)
	}

	var response minio.ErrorResponse
	if errors.As(err, &response) {
		kind := s3ErrorKind(response.Code, response.StatusCode)
		return errorWithCode(op, ProviderS3, key, response.Code, kind, err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return newError(op, ProviderS3, key, ErrTemporary, err)
	}
	return newError(op, ProviderS3, key, nil, err)
}

func s3ErrorKind(code string, status int) error {
	switch code {
	case "NoSuchKey", "NoSuchBucket", "NoSuchUpload", "NoSuchVersion", "NotFound":
		return ErrNotFound
	case "XMinioInvalidObjectName", "InvalidObjectName":
		return ErrInvalidKey
	case "AccessDenied", "AllAccessDisabled", "InvalidAccessKeyId", "InvalidToken", "ExpiredToken", "SignatureDoesNotMatch", "RequestTimeTooSkewed":
		return ErrAccessDenied
	case "InvalidRange", "RequestedRangeNotSatisfiable":
		return ErrInvalidRange
	case "InvalidPart", "InvalidPartOrder", "EntityTooSmall", "EntityTooLarge", "MalformedXML":
		return ErrInvalidPart
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou", "Conflict", "OperationAborted", "PreconditionFailed":
		return ErrConflict
	case "SlowDown", "ServiceUnavailable", "InternalError", "InternalServerError", "RequestTimeout", "RequestTimeoutException":
		return ErrTemporary
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAccessDenied
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict, http.StatusPreconditionFailed:
		return ErrConflict
	case http.StatusRequestedRangeNotSatisfiable:
		return ErrInvalidRange
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return ErrTemporary
	default:
		return nil
	}
}
