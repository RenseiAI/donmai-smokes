package smokes

// tempdir_removable_test.go — fixture homes that the test can always delete.
//
// The harness lanes run `donmai agent run` with HOME set to a per-test temp
// directory. A subprocess that runs Go with that HOME uses the default GOPATH,
// $HOME/go, and can fill a module cache there (for example a
// golang.org/toolchain download when the subprocess environment does not carry
// GOTOOLCHAIN=local). Go creates module cache directories read-only. As root,
// t.TempDir's RemoveAll deletes them anyway. As an ordinary user it fails with
// "unlinkat …: permission denied", and the testing package reports that as a
// test failure even when every assertion passed. An unprivileged CI job (the
// embedder's release harness lane) hit exactly this on every lane that uses a
// fixture home.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// removableTempDir returns t.TempDir() with a cleanup that restores owner write
// permission on every directory beneath it. Cleanups run last-in-first-out, so
// this runs before t.TempDir's own RemoveAll, which can then delete a read-only
// module cache as an ordinary user.
func removableTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { makeDirTreeOwnerWritable(dir) })
	return dir
}

// makeDirTreeOwnerWritable adds owner rwx to every directory under root that
// lacks owner write. It is best effort: an entry it cannot read or change is
// left for RemoveAll to report, so a genuine cleanup problem still surfaces.
func makeDirTreeOwnerWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode().Perm()&0o200 != 0 {
			return nil
		}
		_ = os.Chmod(path, info.Mode().Perm()|0o700)
		return nil
	})
}

// TestRemovableTempDirDeletesReadOnlyModuleCache builds the shape Go leaves
// behind (a read-only module cache tree under $HOME/go/pkg/mod) inside a
// subtest's removableTempDir, and requires the subtest's own cleanup to delete
// it. With plain t.TempDir the subtest fails with "TempDir RemoveAll cleanup:
// … permission denied" for any non-root user.
func TestRemovableTempDirDeletesReadOnlyModuleCache(t *testing.T) {
	// As root the RemoveAll succeeds regardless, so this only bites for an
	// ordinary user — which is the case it guards.
	var home string
	t.Run("fixture home with a read-only module cache", func(t *testing.T) {
		home = removableTempDir(t)
		modDir := filepath.Join(home, "go", "pkg", "mod", "golang.org", "toolchain@v0.0.1-go0.0.0.test")
		if err := os.MkdirAll(modDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(modDir, "README.md"), []byte("read-only module cache file\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		for dir := modDir; dir != home; dir = filepath.Dir(dir) {
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
		}
	})
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("fixture home %s was not removed by the subtest cleanup (stat err=%v)", home, err)
	}
}
