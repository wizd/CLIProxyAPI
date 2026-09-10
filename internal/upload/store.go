package upload

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

const (
	fileIDPrefix      = "file-"
	metaSuffix        = ".json"
	anonymousScope    = "_anonymous"
	fileIDRandomBytes = 16
)

var (
	// ErrNotFound is returned when a handle is missing or belongs to another caller.
	ErrNotFound = errors.New("file not found")
	// ErrTooLarge is returned when an upload exceeds the per-file cap.
	ErrTooLarge = errors.New("file exceeds upload size limit")
	// ErrInvalidID is returned when a file id is malformed.
	ErrInvalidID = errors.New("invalid file id")
	// ErrUnavailable is returned when the store is not initialized.
	ErrUnavailable = errors.New("file store unavailable")
)

var fileIDPattern = regexp.MustCompile(`^file-[A-Za-z0-9_-]{8,64}$`)

// Meta is the persisted sidecar for one uploaded file.
type Meta struct {
	ID          string `json:"id"`
	CallerScope string `json:"caller_scope"`
	MIME        string `json:"mime"`
	Filename    string `json:"filename"`
	Bytes       int64  `json:"bytes"`
	CreatedAt   int64  `json:"created_at"`
	CreatedNano int64  `json:"created_nano,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
}

// Store is a disk-backed file handle store with TTL and capacity eviction.
type Store struct {
	dir     string
	ttl     time.Duration
	maxFile int64
	maxAll  int64

	mu    sync.RWMutex
	index map[string]Meta

	stopCh   chan struct{}
	stopOnce sync.Once
}

var currentStore atomic.Pointer[Store]

// SetCurrent registers the process-wide store used by executors.
func SetCurrent(s *Store) {
	currentStore.Store(s)
}

// Current returns the process-wide store, or nil.
func Current() *Store {
	return currentStore.Load()
}

// Open creates a store at dir, rebuilds the in-memory index, and starts GC.
func Open(dir string, cfg config.FileStoreConfig) (*Store, error) {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "." {
		return nil, fmt.Errorf("upload store: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upload store: mkdir: %w", err)
	}
	s := &Store{
		dir:     dir,
		ttl:     cfg.TTLDuration(),
		maxFile: cfg.MaxUploadBytes(),
		maxAll:  cfg.MaxTotalBytes(),
		index:   make(map[string]Meta),
		stopCh:  make(chan struct{}),
	}
	if err := s.rebuild(); err != nil {
		return nil, err
	}
	go s.cleanupLoop()
	return s, nil
}

// Dir returns the store root.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// ApplyConfig updates runtime limits without changing the directory.
func (s *Store) ApplyConfig(cfg config.FileStoreConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ttl = cfg.TTLDuration()
	s.maxFile = cfg.MaxUploadBytes()
	s.maxAll = cfg.MaxTotalBytes()
	s.mu.Unlock()
}

// Close stops the GC goroutine.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
}

// Put streams r to disk and returns a new handle owned by callerScope.
func (s *Store) Put(callerScope string, r io.Reader, mimeType, filename string) (Meta, error) {
	if s == nil {
		return Meta{}, ErrUnavailable
	}
	if r == nil {
		return Meta{}, fmt.Errorf("upload store: empty body")
	}
	callerScope = normalizeScope(callerScope)
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	filename = filepath.Base(strings.TrimSpace(filename))
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		filename = "upload.bin"
	}

	id, err := newFileID()
	if err != nil {
		return Meta{}, err
	}
	now := time.Now()
	s.mu.RLock()
	ttl := s.ttl
	limit := s.maxFile
	s.mu.RUnlock()
	if ttl <= 0 {
		ttl, _ = time.ParseDuration(config.DefaultFileStoreTTL)
	}

	ownerDir := filepath.Join(s.dir, callerScope)
	if err = os.MkdirAll(ownerDir, 0o700); err != nil {
		return Meta{}, fmt.Errorf("upload store: mkdir caller: %w", err)
	}

	tmp, err := os.CreateTemp(ownerDir, id+".tmp-*")
	if err != nil {
		return Meta{}, fmt.Errorf("upload store: create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		if errClose := tmp.Close(); errClose != nil && !errors.Is(errClose, os.ErrClosed) {
			log.Errorf("upload store: close temp file: %v", errClose)
		}
		if errRemove := os.Remove(tmpName); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.Errorf("upload store: remove temp file: %v", errRemove)
		}
	}

	written, err := io.Copy(tmp, io.LimitReader(r, limit+1))
	if err != nil {
		cleanup()
		return Meta{}, fmt.Errorf("upload store: write: %w", err)
	}
	if written > limit {
		cleanup()
		return Meta{}, ErrTooLarge
	}
	if written == 0 {
		cleanup()
		return Meta{}, fmt.Errorf("upload store: empty file")
	}
	if err = tmp.Sync(); err != nil {
		cleanup()
		return Meta{}, fmt.Errorf("upload store: sync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Meta{}, fmt.Errorf("upload store: close: %w", err)
	}

	finalPath := filepath.Join(ownerDir, id)
	if err = os.Rename(tmpName, finalPath); err != nil {
		_ = os.Remove(tmpName)
		return Meta{}, fmt.Errorf("upload store: rename: %w", err)
	}

	meta := Meta{
		ID:          id,
		CallerScope: callerScope,
		MIME:        mimeType,
		Filename:    filename,
		Bytes:       written,
		CreatedAt:   now.Unix(),
		CreatedNano: now.UnixNano(),
		ExpiresAt:   now.Add(ttl).Unix(),
	}
	if err = s.writeMeta(meta); err != nil {
		_ = os.Remove(finalPath)
		return Meta{}, err
	}

	s.mu.Lock()
	s.index[id] = meta
	s.mu.Unlock()
	s.cleanup()
	return meta, nil
}

// Stat returns metadata after verifying caller ownership.
func (s *Store) Stat(callerScope, fileID string) (Meta, error) {
	if s == nil {
		return Meta{}, ErrUnavailable
	}
	fileID = strings.TrimSpace(fileID)
	if !ValidFileID(fileID) {
		return Meta{}, ErrInvalidID
	}
	callerScope = normalizeScope(callerScope)
	s.mu.RLock()
	meta, ok := s.index[fileID]
	s.mu.RUnlock()
	if !ok || meta.CallerScope != callerScope || isExpired(meta, time.Now()) {
		return Meta{}, ErrNotFound
	}
	return meta, nil
}

// Open returns a reader for the blob after verifying caller ownership.
func (s *Store) Open(callerScope, fileID string) (io.ReadCloser, Meta, error) {
	meta, err := s.Stat(callerScope, fileID)
	if err != nil {
		return nil, Meta{}, err
	}
	f, err := os.Open(s.blobPath(meta))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Meta{}, ErrNotFound
		}
		return nil, Meta{}, err
	}
	return f, meta, nil
}

// ReadAll loads the blob into memory after verifying caller ownership.
func (s *Store) ReadAll(callerScope, fileID string) ([]byte, Meta, error) {
	rc, meta, err := s.Open(callerScope, fileID)
	if err != nil {
		return nil, Meta{}, err
	}
	defer func() {
		if errClose := rc.Close(); errClose != nil {
			log.Errorf("upload store: close blob: %v", errClose)
		}
	}()
	s.mu.RLock()
	limit := s.maxFile
	s.mu.RUnlock()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, Meta{}, err
	}
	if int64(len(data)) > limit {
		return nil, Meta{}, ErrTooLarge
	}
	return data, meta, nil
}

// Delete removes a handle owned by callerScope.
func (s *Store) Delete(callerScope, fileID string) error {
	if s == nil {
		return ErrUnavailable
	}
	fileID = strings.TrimSpace(fileID)
	if !ValidFileID(fileID) {
		return ErrInvalidID
	}
	callerScope = normalizeScope(callerScope)
	s.mu.Lock()
	meta, ok := s.index[fileID]
	if !ok || meta.CallerScope != callerScope {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.index, fileID)
	s.mu.Unlock()
	s.removeFiles(meta)
	return nil
}

// ValidFileID reports whether id matches the gateway file-id alphabet.
func ValidFileID(id string) bool {
	return fileIDPattern.MatchString(strings.TrimSpace(id))
}

func newFileID() (string, error) {
	var raw [fileIDRandomBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("upload store: generate id: %w", err)
	}
	return fileIDPrefix + hex.EncodeToString(raw[:]), nil
}

func normalizeScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope == "" || strings.ContainsAny(scope, `/\`) || strings.Contains(scope, "..") {
		return anonymousScope
	}
	if len(scope) > 128 {
		return scope[:128]
	}
	return scope
}

func (s *Store) blobPath(meta Meta) string {
	return filepath.Join(s.dir, meta.CallerScope, meta.ID)
}

func (s *Store) metaPath(meta Meta) string {
	return filepath.Join(s.dir, meta.CallerScope, meta.ID+metaSuffix)
}

func (s *Store) writeMeta(meta Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("upload store: encode meta: %w", err)
	}
	path := s.metaPath(meta)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("upload store: write meta: %w", err)
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("upload store: rename meta: %w", err)
	}
	return nil
}

func (s *Store) rebuild() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("upload store: read dir: %w", err)
	}
	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		owner := entry.Name()
		ownerDir := filepath.Join(s.dir, owner)
		files, errRead := os.ReadDir(ownerDir)
		if errRead != nil {
			log.Errorf("upload store: read caller dir: %v", errRead)
			continue
		}
		for _, file := range files {
			name := file.Name()
			if file.IsDir() || !strings.HasSuffix(name, metaSuffix) {
				continue
			}
			raw, errReadMeta := os.ReadFile(filepath.Join(ownerDir, name))
			if errReadMeta != nil {
				continue
			}
			var meta Meta
			if errJSON := json.Unmarshal(raw, &meta); errJSON != nil || !ValidFileID(meta.ID) {
				continue
			}
			if meta.CallerScope == "" {
				meta.CallerScope = owner
			}
			if _, errStat := os.Stat(s.blobPath(meta)); errStat != nil {
				_ = os.Remove(filepath.Join(ownerDir, name))
				continue
			}
			if isExpired(meta, now) {
				s.removeFiles(meta)
				continue
			}
			s.index[meta.ID] = meta
		}
	}
	return nil
}

func (s *Store) cleanupLoop() {
	ticker := time.NewTicker(s.ttl / 2)
	if s.ttl/2 < time.Minute {
		ticker.Reset(time.Minute)
	}
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.cleanup()
		}
	}
}

func (s *Store) cleanup() {
	if s == nil {
		return
	}
	now := time.Now()
	s.mu.Lock()
	expired := make([]Meta, 0)
	alive := make([]Meta, 0, len(s.index))
	var total int64
	for id, meta := range s.index {
		if isExpired(meta, now) {
			expired = append(expired, meta)
			delete(s.index, id)
			continue
		}
		alive = append(alive, meta)
		total += meta.Bytes
	}
	maxAll := s.maxAll
	s.mu.Unlock()

	for _, meta := range expired {
		s.removeFiles(meta)
	}
	if maxAll <= 0 || total <= maxAll {
		return
	}
	sort.Slice(alive, func(i, j int) bool {
		left := alive[i].CreatedNano
		right := alive[j].CreatedNano
		if left == 0 {
			left = alive[i].CreatedAt * int64(time.Second)
		}
		if right == 0 {
			right = alive[j].CreatedAt * int64(time.Second)
		}
		if left == right {
			return alive[i].ID < alive[j].ID
		}
		return left < right
	})
	for _, meta := range alive {
		if total <= maxAll {
			break
		}
		s.mu.Lock()
		delete(s.index, meta.ID)
		s.mu.Unlock()
		s.removeFiles(meta)
		total -= meta.Bytes
	}
}

func (s *Store) removeFiles(meta Meta) {
	if err := os.Remove(s.blobPath(meta)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Errorf("upload store: remove blob: %v", err)
	}
	if err := os.Remove(s.metaPath(meta)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Errorf("upload store: remove meta: %v", err)
	}
}

func isExpired(meta Meta, now time.Time) bool {
	return meta.ExpiresAt > 0 && now.Unix() >= meta.ExpiresAt
}
