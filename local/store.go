package local

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
)

const localLockShards = 64

// Store stores objects below a private filesystem root.
type Store struct {
	root           string
	internalRoot   string
	metadataRoot   string
	multipartRoot  string
	dirMode        fs.FileMode
	fileMode       fs.FileMode
	locks          [localLockShards]sync.RWMutex
	multipartLocks [localLockShards]sync.Mutex
}

// New creates a filesystem-backed object store.
func New(config LocalConfig) (*Store, error) {
	if strings.TrimSpace(config.RootDir) == "" {
		return nil, newError("new-local", ProviderLocal, "", ErrInvalidConfig, errors.New("rootDir is required"))
	}
	root, err := filepath.Abs(config.RootDir)
	if err != nil {
		return nil, newError("new-local", ProviderLocal, "", ErrInvalidConfig, err)
	}
	dirMode := fs.FileMode(config.DirMode).Perm()
	if dirMode == 0 {
		dirMode = 0o750
	}
	fileMode := fs.FileMode(config.FileMode).Perm()
	if fileMode == 0 {
		fileMode = 0o640
	}
	if err = os.MkdirAll(root, dirMode); err != nil {
		return nil, mapLocalError("new-local", "", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	store := &Store{
		root:          root,
		internalRoot:  filepath.Join(root, internalDirName),
		metadataRoot:  filepath.Join(root, internalDirName, "metadata"),
		multipartRoot: filepath.Join(root, internalDirName, "multipart"),
		dirMode:       dirMode,
		fileMode:      fileMode,
	}
	for _, directory := range []string{store.internalRoot, store.metadataRoot, store.multipartRoot} {
		if err = ensureNoSymlinkComponents(store.root, directory); err != nil {
			return nil, newError("new-local", ProviderLocal, "", ErrInvalidConfig, err)
		}
		if err = os.MkdirAll(directory, dirMode); err != nil {
			return nil, mapLocalError("new-local", "", err)
		}
		if err = ensureNoSymlinkComponents(store.root, directory); err != nil {
			return nil, newError("new-local", ProviderLocal, "", ErrInvalidConfig, err)
		}
	}
	return store, nil
}

func (s *Store) Provider() Provider {
	return ProviderLocal
}

func (s *Store) Capabilities() Capabilities {
	return Capabilities(CapabilityRangeRead | CapabilityList | CapabilityCopy | CapabilityMultipart)
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, size int64, options PutOptions) (ObjectInfo, error) {
	if reader == nil {
		return ObjectInfo{}, newError("put", ProviderLocal, key, ErrInvalidConfig, errors.New("reader is required"))
	}
	if size < -1 {
		return ObjectInfo{}, newError("put", ProviderLocal, key, ErrInvalidConfig, errors.New("size must be -1 or greater"))
	}
	target, err := s.pathForKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	lock := s.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()

	if err = os.MkdirAll(filepath.Dir(target), s.dirMode); err != nil {
		return ObjectInfo{}, mapLocalError("put/mkdir", key, err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), internalTempPrefix+"write-*")
	if err != nil {
		return ObjectInfo{}, mapLocalError("put/temp", key, err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(s.fileMode); err != nil {
		return ObjectInfo{}, mapLocalError("put/chmod", key, err)
	}

	md5Hash := md5.New()
	sha256Hash := sha256.New()
	written, err := copyWithExpectedSize(ctx, io.MultiWriter(temporary, md5Hash, sha256Hash), reader, size)
	if err != nil {
		return ObjectInfo{}, mapLocalError("put/write", key, err)
	}
	if err = ctx.Err(); err != nil {
		return ObjectInfo{}, mapLocalError("put", key, err)
	}
	if err = temporary.Sync(); err != nil {
		return ObjectInfo{}, mapLocalError("put/sync", key, err)
	}
	if err = temporary.Close(); err != nil {
		return ObjectInfo{}, mapLocalError("put/close", key, err)
	}

	contentType := strings.TrimSpace(options.ContentType)
	if contentType == "" {
		contentType = detectFileContentType(temporaryPath)
	}
	if err = replaceFile(temporaryPath, target); err != nil {
		return ObjectInfo{}, mapLocalError("put/commit", key, err)
	}
	committed = true
	_ = syncDirectory(filepath.Dir(target))

	stat, err := os.Stat(target)
	if err != nil {
		return ObjectInfo{}, mapLocalError("put/stat", key, err)
	}
	metadata := localObjectMetadata{
		Key:                key,
		Size:               written,
		ETag:               hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256:             hex.EncodeToString(sha256Hash.Sum(nil)),
		ContentType:        contentType,
		ContentEncoding:    options.ContentEncoding,
		ContentDisposition: options.ContentDisposition,
		CacheControl:       options.CacheControl,
		StorageClass:       options.StorageClass,
		Metadata:           cloneMetadata(options.Metadata),
		LastModified:       stat.ModTime().UTC(),
	}
	if err = s.writeMetadata(metadata); err != nil {
		return metadata.objectInfo(), newError("put/metadata", ProviderLocal, key, ErrPartialFailure, err)
	}
	return metadata.objectInfo(), nil
}

func (s *Store) Open(ctx context.Context, key string, options OpenOptions) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapLocalError("open", key, err)
	}
	if options.Offset < 0 || options.Length < -1 {
		return nil, newError("open", ProviderLocal, key, ErrInvalidRange, nil)
	}
	if options.Version != "" {
		return nil, newError("open", ProviderLocal, key, ErrUnsupported, errors.New("local object versions are not supported"))
	}
	path, err := s.pathForKey(key)
	if err != nil {
		return nil, err
	}
	lock := s.lockForKey(key)
	lock.RLock()
	file, err := os.Open(path)
	if err != nil {
		lock.RUnlock()
		return nil, mapLocalError("open", key, err)
	}
	stat, err := file.Stat()
	lock.RUnlock()
	if err != nil {
		_ = file.Close()
		return nil, mapLocalError("open/stat", key, err)
	}
	if stat.IsDir() {
		_ = file.Close()
		return nil, newError("open", ProviderLocal, key, ErrNotFound, nil)
	}
	if options.Offset > stat.Size() {
		_ = file.Close()
		return nil, newError("open", ProviderLocal, key, ErrInvalidRange, nil)
	}
	length := stat.Size() - options.Offset
	if options.Length > 0 && options.Length < length {
		length = options.Length
	}
	section := io.NewSectionReader(file, options.Offset, length)
	return &sectionReadCloser{reader: &contextReader{ctx: ctx, reader: section}, closer: file}, nil
}

func (s *Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return ObjectInfo{}, mapLocalError("stat", key, err)
	}
	path, err := s.pathForKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	lock := s.lockForKey(key)
	lock.RLock()
	defer lock.RUnlock()
	return s.statUnlocked(key, path)
}

func (s *Store) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return mapLocalError("delete", key, err)
	}
	path, err := s.pathForKey(key)
	if err != nil {
		return err
	}
	lock := s.lockForKey(key)
	lock.Lock()
	defer lock.Unlock()
	if err = os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return mapLocalError("delete", key, err)
	}
	if err = os.Remove(s.metadataPath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return mapLocalError("delete/metadata", key, err)
	}
	s.pruneEmptyDirectories(filepath.Dir(path))
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
	itemsByKey := make(map[string]ObjectInfo)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == s.internalRoot {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if strings.HasPrefix(entry.Name(), internalTempPrefix) {
			return nil
		}
		relative, relErr := filepath.Rel(s.root, path)
		if relErr != nil {
			return relErr
		}
		key := filepath.ToSlash(relative)
		if !strings.HasPrefix(key, options.Prefix) {
			return nil
		}
		if !options.Recursive {
			remainder := strings.TrimPrefix(key, options.Prefix)
			if index := strings.IndexByte(remainder, '/'); index >= 0 {
				prefix := options.Prefix + remainder[:index+1]
				itemsByKey[prefix] = ObjectInfo{Key: prefix, IsPrefix: true}
				return nil
			}
		}
		info, statErr := s.Stat(ctx, key)
		if statErr != nil {
			if errors.Is(statErr, ErrNotFound) {
				return nil
			}
			return statErr
		}
		itemsByKey[key] = info
		return nil
	})
	if err != nil {
		return ListResult{}, mapLocalError("list", options.Prefix, err)
	}
	keys := make([]string, 0, len(itemsByKey))
	for key := range itemsByKey {
		if options.Cursor == "" || key > options.Cursor {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := ListResult{Items: make([]ObjectInfo, 0, min(limit, len(keys)))}
	if len(keys) > limit {
		result.HasMore = true
		keys = keys[:limit]
	}
	for _, key := range keys {
		result.Items = append(result.Items, itemsByKey[key])
	}
	if result.HasMore && len(result.Items) > 0 {
		result.NextCursor = result.Items[len(result.Items)-1].Key
	}
	return result, nil
}

func (s *Store) Copy(ctx context.Context, sourceKey, targetKey string, options CopyOptions) (ObjectInfo, error) {
	sourceInfo, err := s.Stat(ctx, sourceKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	reader, err := s.Open(ctx, sourceKey, OpenOptions{})
	if err != nil {
		return ObjectInfo{}, err
	}
	defer reader.Close()
	putOptions := PutOptions{
		ContentType:        sourceInfo.ContentType,
		ContentEncoding:    sourceInfo.ContentEncoding,
		ContentDisposition: sourceInfo.ContentDisposition,
		CacheControl:       sourceInfo.CacheControl,
		StorageClass:       sourceInfo.StorageClass,
		Metadata:           cloneMetadata(sourceInfo.Metadata),
	}
	if options.ReplaceMetadata {
		putOptions.Metadata = cloneMetadata(options.Metadata)
	}
	if options.ContentType != "" {
		putOptions.ContentType = options.ContentType
	}
	return s.Put(ctx, targetKey, reader, sourceInfo.Size, putOptions)
}

func (s *Store) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return mapLocalError("health", "", err)
	}
	file, err := os.CreateTemp(s.internalRoot, internalTempPrefix+"health-*")
	if err != nil {
		return mapLocalError("health", "", err)
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err = file.Write([]byte("ok")); err != nil {
		_ = file.Close()
		return mapLocalError("health/write", "", err)
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return mapLocalError("health/sync", "", err)
	}
	if err = file.Close(); err != nil {
		return mapLocalError("health/close", "", err)
	}
	if err = os.Remove(path); err != nil {
		return mapLocalError("health/remove", "", err)
	}
	return nil
}

func (s *Store) Close() error {
	return nil
}

func (s *Store) pathForKey(key string) (string, error) {
	if err := validateObjectKey(key); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, filepath.FromSlash(key))
	relative, err := filepath.Rel(s.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", newError("resolve-key", ProviderLocal, key, ErrInvalidKey, err)
	}
	if err = ensureNoSymlinkComponents(s.root, path); err != nil {
		return "", newError("resolve-key", ProviderLocal, key, ErrInvalidKey, err)
	}
	return path, nil
}

func (s *Store) statUnlocked(key, path string) (ObjectInfo, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return ObjectInfo{}, mapLocalError("stat", key, err)
	}
	if stat.IsDir() {
		return ObjectInfo{}, newError("stat", ProviderLocal, key, ErrNotFound, nil)
	}
	metadata, err := s.readMetadata(key)
	if err == nil && metadata.Size == stat.Size() && metadata.LastModified.Equal(stat.ModTime().UTC()) {
		return metadata.objectInfo(), nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, mapLocalError("stat/metadata", key, err)
	}
	return buildMetadataFromFile(key, path, stat)
}

func buildMetadataFromFile(key, path string, stat fs.FileInfo) (ObjectInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return ObjectInfo{}, mapLocalError("stat/open", key, err)
	}
	defer file.Close()
	md5Hash := md5.New()
	sha256Hash := sha256.New()
	if _, err = io.Copy(io.MultiWriter(md5Hash, sha256Hash), file); err != nil {
		return ObjectInfo{}, mapLocalError("stat/hash", key, err)
	}
	return ObjectInfo{
		Key:          key,
		Size:         stat.Size(),
		ETag:         hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256:       hex.EncodeToString(sha256Hash.Sum(nil)),
		ContentType:  detectFileContentType(path),
		LastModified: stat.ModTime().UTC(),
	}, nil
}

func (s *Store) lockForKey(key string) *sync.RWMutex {
	hash := sha256.Sum256([]byte(key))
	return &s.locks[int(hash[0])%len(s.locks)]
}

func (s *Store) multipartLockForID(uploadID string) *sync.Mutex {
	hash := sha256.Sum256([]byte(uploadID))
	return &s.multipartLocks[int(hash[0])%len(s.multipartLocks)]
}

func (s *Store) pruneEmptyDirectories(directory string) {
	for directory != s.root && directory != s.internalRoot {
		if err := os.Remove(directory); err != nil {
			return
		}
		directory = filepath.Dir(directory)
	}
}

func copyWithExpectedSize(ctx context.Context, destination io.Writer, source io.Reader, expected int64) (int64, error) {
	reader := &contextReader{ctx: ctx, reader: source}
	if expected < 0 {
		return io.Copy(destination, reader)
	}
	written, err := io.Copy(destination, io.LimitReader(reader, expected+1))
	if err != nil {
		return written, err
	}
	if written != expected {
		return written, fmt.Errorf("content size mismatch: expected %d, received %d", expected, written)
	}
	return written, nil
}

func detectFileContentType(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer file.Close()
	buffer := make([]byte, 512)
	count, _ := file.Read(buffer)
	if count == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(buffer[:count])
}

func replaceFile(source, target string) error {
	if runtime.GOOS == "windows" {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.Rename(source, target)
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func mapLocalError(op, key string, err error) error {
	if err == nil {
		return nil
	}
	kind := error(nil)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		kind = ErrNotFound
	case errors.Is(err, fs.ErrPermission):
		kind = ErrAccessDenied
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		kind = ErrTemporary
	}
	return newError(op, ProviderLocal, key, kind, err)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

type sectionReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (r *sectionReadCloser) Read(buffer []byte) (int, error) {
	return r.reader.Read(buffer)
}

func (r *sectionReadCloser) Close() error {
	return r.closer.Close()
}
