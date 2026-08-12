package ossutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/wfu-work/nav-oss-go-lib/core"
)

const (
	InternalDirName    = ".nav-oss"
	InternalTempPrefix = ".nav-oss-tmp-"
)

func ValidateObjectKey(key string) error {
	if key == "" || strings.TrimSpace(key) == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return NewError("validate-key", "", key, core.ErrInvalidKey, nil)
	}
	if strings.ContainsAny(key, "\\\x00") {
		return NewError("validate-key", "", key, core.ErrInvalidKey, nil)
	}
	parts := strings.Split(key, "/")
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return NewError("validate-key", "", key, core.ErrInvalidKey, nil)
		}
		if strings.HasPrefix(part, InternalTempPrefix) {
			return NewError("validate-key", "", key, core.ErrInvalidKey, nil)
		}
		if index == 0 && part == InternalDirName {
			return NewError("validate-key", "", key, core.ErrInvalidKey, nil)
		}
	}
	return nil
}

func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if strings.HasPrefix(prefix, "/") || strings.ContainsAny(prefix, "\\\x00") {
		return NewError("validate-prefix", "", prefix, core.ErrInvalidKey, nil)
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return NewError("validate-prefix", "", prefix, core.ErrInvalidKey, nil)
	}
	parts := strings.Split(trimmed, "/")
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return NewError("validate-prefix", "", prefix, core.ErrInvalidKey, nil)
		}
		if strings.HasPrefix(part, InternalTempPrefix) {
			return NewError("validate-prefix", "", prefix, core.ErrInvalidKey, nil)
		}
		if index == 0 && part == InternalDirName {
			return NewError("validate-prefix", "", prefix, core.ErrInvalidKey, nil)
		}
	}
	return nil
}

func CloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	copy := make(map[string]string, len(metadata))
	for key, value := range metadata {
		copy[key] = value
	}
	return copy
}

// EnsureNoSymlinkComponents prevents a lexical in-root path from escaping the
// root through an existing symbolic-link component. The local store root is
// expected to remain private to the application; this check also protects
// against accidentally introduced links.
func EnsureNoSymlinkComponents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return core.ErrInvalidKey
	}
	current := root
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return core.ErrInvalidKey
		}
	}
	return nil
}
