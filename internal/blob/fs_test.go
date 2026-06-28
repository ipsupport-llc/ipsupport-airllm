package blob

import (
	"context"
	"testing"
)

func TestFSRoundTrip(t *testing.T) {
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "a/b.bin", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(context.Background(), "a/b.bin")
	if err != nil || string(got) != "hello" {
		t.Fatalf("get=%q err=%v", got, err)
	}
	if err := s.Delete(context.Background(), "a/b.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), "a/b.bin"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestFSRejectsPathTraversal(t *testing.T) {
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(context.Background(), "../escape.bin", []byte("x")); err == nil {
		t.Fatal("expected error for path traversal key")
	}
}
