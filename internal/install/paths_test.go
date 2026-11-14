package install

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nanocloud-io/nanocloud/internal/storage"
)

func TestMakeStateDirs_CreatesAndTightens(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "nano-cloud")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	created := filepath.Join(existing, "vm-disks")

	if err := makeStateDirs(existing, created); err != nil {
		t.Fatalf("makeStateDirs: %v", err)
	}
	for _, dir := range []string{existing, created} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != storage.DirMode {
			t.Errorf("%s has mode %04o, want %04o", filepath.Base(dir), got, storage.DirMode)
		}
	}
}
