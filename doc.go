// Package oss provides a small, provider-neutral object-storage API with
// filesystem and S3-compatible implementations.
//
// Authentication, authorization, database entities, and business-level file
// metadata intentionally remain the responsibility of the application using
// this package.
package oss

var (
	_ Store           = (*LocalStore)(nil)
	_ Copier          = (*LocalStore)(nil)
	_ MultipartStore  = (*LocalStore)(nil)
	_ MultipartLister = (*LocalStore)(nil)
	_ Store           = (*S3Store)(nil)
	_ Copier          = (*S3Store)(nil)
	_ MultipartStore  = (*S3Store)(nil)
	_ MultipartLister = (*S3Store)(nil)
	_ Presigner       = (*S3Store)(nil)
)
