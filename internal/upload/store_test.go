package upload

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func testStore(t *testing.T, cfg config.FileStoreConfig) *Store {
	t.Helper()
	if cfg.TTL == "" {
		cfg.TTL = "1h"
	}
	if cfg.MaxUploadMB == 0 {
		cfg.MaxUploadMB = 1
	}
	if cfg.MaxTotalGB == 0 {
		cfg.MaxTotalGB = 1
	}
	store, err := Open(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestStorePutGetTenantIsolation(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{})
	meta, err := store.Put("alice", strings.NewReader("hello-video"), "video/mp4", "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidFileID(meta.ID) {
		t.Fatalf("id = %q", meta.ID)
	}
	if meta.Bytes != 11 || meta.MIME != "video/mp4" || meta.Filename != "clip.mp4" {
		t.Fatalf("meta = %+v", meta)
	}

	data, got, err := store.ReadAll("alice", meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello-video" || got.ID != meta.ID {
		t.Fatalf("read = %q %+v", data, got)
	}
	if _, _, err = store.ReadAll("bob", meta.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant err = %v", err)
	}
	if _, err = store.Stat("bob", meta.ID); err != ErrNotFound {
		t.Fatalf("stat cross-tenant err = %v", err)
	}
}

func TestStoreRejectsTooLarge(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{MaxUploadMB: 1})
	payload := bytes.Repeat([]byte("a"), int(store.maxFile)+1)
	if _, err := store.Put("alice", bytes.NewReader(payload), "video/mp4", "big.mp4"); err != ErrTooLarge {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreTTLExpiry(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{TTL: "1h"})
	meta, err := store.Put("alice", strings.NewReader("clip"), "video/mp4", "clip.mp4")
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	entry := store.index[meta.ID]
	entry.ExpiresAt = time.Now().Add(-time.Second).Unix()
	store.index[meta.ID] = entry
	store.mu.Unlock()
	store.cleanup()
	if _, err = store.Stat("alice", meta.ID); err != ErrNotFound {
		t.Fatalf("expired file still visible: %v", err)
	}
}

func TestStoreCapacityEviction(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, config.FileStoreConfig{TTL: "1h", MaxUploadMB: 1, MaxTotalGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	store.mu.Lock()
	store.maxAll = 20
	store.mu.Unlock()

	first, err := store.Put("alice", strings.NewReader(strings.Repeat("a", 12)), "video/mp4", "a.mp4")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put("alice", strings.NewReader(strings.Repeat("b", 12)), "video/mp4", "b.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Stat("alice", first.ID); err != ErrNotFound {
		t.Fatal("oldest file should have been evicted")
	}
	if _, err = store.Stat("alice", second.ID); err != nil {
		t.Fatalf("newest file missing: %v", err)
	}
}

func TestStoreRebuildFromDisk(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir, config.FileStoreConfig{TTL: "1h", MaxUploadMB: 1, MaxTotalGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	meta, err := first.Put("alice", strings.NewReader("persist-me"), "video/webm", "keep.webm")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(dir, config.FileStoreConfig{TTL: "1h", MaxUploadMB: 1, MaxTotalGB: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	data, got, err := second.ReadAll("alice", meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "persist-me" || got.MIME != "video/webm" {
		t.Fatalf("rebuilt = %q %+v", data, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "alice", meta.ID)); err != nil {
		t.Fatal(err)
	}
}

func TestStoreDelete(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{})
	meta, err := store.Put("alice", strings.NewReader("x"), "video/mp4", "x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Delete("bob", meta.ID); err != ErrNotFound {
		t.Fatalf("delete other tenant = %v", err)
	}
	if err = store.Delete("alice", meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Stat("alice", meta.ID); err != ErrNotFound {
		t.Fatal("deleted file still visible")
	}
}

func TestStoreRejectsEmpty(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{})
	if _, err := store.Put("alice", strings.NewReader(""), "video/mp4", "empty.mp4"); err == nil {
		t.Fatal("expected empty upload to fail")
	}
}

func TestValidFileID(t *testing.T) {
	if ValidFileID("file-0123456789abcdef") != true {
		t.Fatal("expected valid id")
	}
	if ValidFileID("../etc/passwd") || ValidFileID("file-") || ValidFileID("abc") {
		t.Fatal("expected invalid ids to be rejected")
	}
}

func TestStoreOpenReader(t *testing.T) {
	store := testStore(t, config.FileStoreConfig{})
	meta, err := store.Put("alice", strings.NewReader("stream"), "video/mp4", "s.mp4")
	if err != nil {
		t.Fatal(err)
	}
	rc, got, err := store.Open("alice", meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "stream" || got.ID != meta.ID {
		t.Fatalf("open = %q %+v", data, got)
	}
}
