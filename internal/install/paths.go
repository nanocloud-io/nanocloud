package install

import (
	"fmt"
	"os"

	"github.com/nanocloud-io/nanocloud/internal/storage"
)

// BinPath is where install puts the binary, on every platform.
const BinPath = binDst

// makeStateDirs creates dirs with storage.DirMode and sets it on any that
// exist, so an upgrade tightens what an older release made 0755.
func makeStateDirs(dirs ...string) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, storage.DirMode); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		if err := os.Chmod(dir, storage.DirMode); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}
	return nil
}
