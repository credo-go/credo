package credo_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/credo-go/credo"
)

func TestDirFS_ReadsFilesAndErrorsOnMissingDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	fsys, closer, err := credo.DirFS(dir)
	if err != nil {
		t.Fatalf("DirFS(%q) error: %v", dir, err)
	}
	defer closer.Close() // release the handle so TempDir cleanup can remove dir

	data, err := fs.ReadFile(fsys, "ok.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("content = %q, want %q", data, "hello")
	}

	if _, _, err := credo.DirFS(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Error("DirFS on a missing directory should return an error")
	}
}

func TestDirFS_BlocksSymlinkTraversal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink inside root that points outside it.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable on this platform/run: %v (runtime=%s)", err, runtime.GOOS)
	}

	fsys, closer, err := credo.DirFS(root)
	if err != nil {
		t.Fatalf("DirFS: %v", err)
	}
	defer closer.Close()

	// os.DirFS would follow this symlink; the os.Root-backed FS must refuse.
	if _, err := fs.ReadFile(fsys, "escape"); err == nil {
		t.Fatal("DirFS followed a symlink escaping the root — traversal not blocked")
	}
}
