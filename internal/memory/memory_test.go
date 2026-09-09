package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/holihur/agent/internal/agent"
)

func TestFileStorePutGetRoundtrip(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "memory"))
	ctx := context.Background()
	if err := s.Put(ctx, "user-fav", "likes rust"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	v, err := s.Get(ctx, "user-fav")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "likes rust" {
		t.Fatalf("Get = %q, want %q", v, "likes rust")
	}
	if err := s.Put(ctx, "user-fav", "likes go"); err != nil {
		t.Fatalf("overwrite Put: %v", err)
	}
	if v, _ := s.Get(ctx, "user-fav"); v != "likes go" {
		t.Fatalf("overwrite = %q, want %q", v, "likes go")
	}
}

func TestFileStoreNotFound(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "memory"))
	ctx := context.Background()
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, agent.ErrMemoryNotFound) {
		t.Fatalf("Get err = %v, want ErrMemoryNotFound", err)
	}
	if err := s.Delete(ctx, "nope"); !errors.Is(err, agent.ErrMemoryNotFound) {
		t.Fatalf("Delete err = %v, want ErrMemoryNotFound", err)
	}
}

func TestFileStoreSearchAndKeys(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "memory"))
	ctx := context.Background()
	for k, v := range map[string]string{"lang-go": "Go is great", "lang-rs": "Rust too", "pet": "cat"} {
		if err := s.Put(ctx, k, v); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := s.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if len(keys) != 3 || keys[0] != "lang-go" {
		t.Fatalf("Keys = %v", keys)
	}
	// 值命中(大小写不敏感)
	hits, err := s.Search(ctx, "rust")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0] != "lang-rs" {
		t.Fatalf("Search(rust) = %v", hits)
	}
	// 键命中
	if hits, _ = s.Search(ctx, "lang"); len(hits) != 2 {
		t.Fatalf("Search(lang) = %v", hits)
	}
	// 空查询 = 列全部
	if hits, _ = s.Search(ctx, ""); len(hits) != 3 {
		t.Fatalf("Search(empty) = %v", hits)
	}
}

func TestFileStoreValidation(t *testing.T) {
	s := NewFileStore(filepath.Join(t.TempDir(), "memory"))
	ctx := context.Background()
	if err := s.Put(ctx, "bad/key", "v"); err == nil {
		t.Fatal("Put bad key: want error")
	}
	if err := s.Put(ctx, "k", "  "); err == nil {
		t.Fatal("Put empty value: want error")
	}
	if err := s.Delete(ctx, "bad/key"); err == nil {
		t.Fatal("Delete bad key: want error")
	}
}
