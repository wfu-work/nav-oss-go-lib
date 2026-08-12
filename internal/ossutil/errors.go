// Package ossutil contains implementation helpers shared by storage providers.
// It is internal so provider-neutral public contracts remain in core.
package ossutil

import "github.com/wfu-work/nav-oss-go-lib/core"

// NewError creates a provider-neutral classified storage error.
func NewError(op string, provider core.Provider, key string, kind, cause error) error {
	return &core.Error{Op: op, Provider: provider, Key: key, Kind: kind, Err: cause}
}

// ErrorWithCode creates a classified error that retains a provider error code.
func ErrorWithCode(op string, provider core.Provider, key, code string, kind, cause error) error {
	return &core.Error{Op: op, Provider: provider, Key: key, Code: code, Kind: kind, Err: cause}
}
