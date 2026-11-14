package cloudinit

import (
	"os"
	"path/filepath"
	"testing"
)

// A seed an older release wrote 0644 must not keep that mode when it is
// rebuilt, as an O_TRUNC rewrite would.
func TestInstallPrivate_ReplacesWithAnOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	src, dest := filepath.Join(dir, "built.iso"), filepath.Join(dir, "web-cidata.iso")
	for p, body := range map[string]string{src: "new seed", dest: "old seed"} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := installPrivate(src, dest); err != nil {
		t.Fatalf("installPrivate: %v", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("seed has mode %04o, want 0600", got)
	}
	if got, _ := os.ReadFile(dest); string(got) != "new seed" {
		t.Errorf("seed holds %q, want the new build", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".web-cidata.iso-*")); len(left) > 0 {
		t.Errorf("temp file left behind: %v", left)
	}
}
