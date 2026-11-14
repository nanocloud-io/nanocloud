package install

import (
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type owned struct {
	uid, gid uint32
	perm     fs.FileMode
}

var rootDir = owned{0, 0, 0o755}

// fakeFS is an ownerFS over a fixed tree. A symlink is listed in tree with
// its target's owner, as Stat reports it, and in links with its target.
type fakeFS struct {
	tree  map[string]owned
	links map[string]string
	staff *uint32 // nil when no staff group is trusted
}

func (f fakeFS) owner(path string) (uint32, uint32, fs.FileMode, error) {
	o, ok := f.tree[path]
	if !ok {
		return 0, 0, 0, fs.ErrNotExist
	}
	return o.uid, o.gid, o.perm, nil
}

func (f fakeFS) resolve(path string) (string, error) {
	if target, ok := f.links[path]; ok {
		return target, nil
	}
	if _, ok := f.tree[path]; !ok {
		return "", fs.ErrNotExist
	}
	return path, nil
}

func (f fakeFS) staffGID() (uint32, bool) {
	if f.staff == nil {
		return 0, false
	}
	return *f.staff, true
}

// installChain is a stock host's chain down to binDst, with overrides applied.
func installChain(overrides map[string]owned) map[string]owned {
	tree := map[string]owned{"/": rootDir, "/usr": rootDir, "/usr/local": rootDir, "/usr/local/bin": rootDir, binDst: rootDir}
	for p, o := range overrides {
		tree[p] = o
	}
	return tree
}

func TestCheckRootOnly(t *testing.T) {
	binDir := filepath.Dir(binDst)
	staff := uint32(50)
	// Debian's /usr/local layout is root:staff 2775; Perm drops the setgid bit.
	staffDir := owned{0, staff, 0o775}
	debian := installChain(map[string]owned{"/usr/local": staffDir, binDir: staffDir})
	cases := []struct {
		name string
		path string
		fs   fakeFS
		// unsafe is the path the refusal must name and why; empty when allowed.
		unsafe, why string
	}{
		{name: "root-owned 0755 chain", path: binDst, fs: fakeFS{tree: installChain(nil)}},
		{
			name: "bin dir not created yet",
			path: binDir,
			fs:   fakeFS{tree: map[string]owned{"/": rootDir, "/usr": rootDir, "/usr/local": rootDir}},
		},
		{
			name:   "user-owned /usr/local/bin",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {501, 0, 0o755}})},
			unsafe: binDir, why: "owned by uid 501",
		},
		{
			name:   "group-writable dir",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {0, 0, 0o775}})},
			unsafe: binDir, why: "writable by group 0 (mode 775)",
		},
		{
			name:   "world-writable dir",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {0, 0, 0o777}})},
			unsafe: binDir, why: "writable by others (mode 777)",
		},
		{
			name:   "user-owned /usr/local with root-owned bin",
			path:   binDst,
			fs:     fakeFS{tree: installChain(map[string]owned{"/usr/local": {501, 0, 0o755}})},
			unsafe: "/usr/local", why: "owned by uid 501",
		},
		{
			name:   "user-owned binary",
			path:   binDst,
			fs:     fakeFS{tree: installChain(map[string]owned{binDst: {501, 0, 0o755}})},
			unsafe: binDst, why: "owned by uid 501",
		},
		{
			name: "bin dir linked into a user-owned tree",
			path: binDir,
			fs: fakeFS{
				tree: installChain(map[string]owned{
					"/Users": rootDir, "/Users/me": {501, 0, 0o755}, "/Users/me/bin": rootDir,
				}),
				links: map[string]string{binDir: "/Users/me/bin"},
			},
			unsafe: "/Users/me", why: "owned by uid 501",
		},
		{name: "Debian root:staff layout", path: binDst, fs: fakeFS{tree: debian, staff: &staff}},
		{
			name:   "staff-writable dir where staff is not trusted",
			path:   binDst,
			fs:     fakeFS{tree: debian},
			unsafe: binDir, why: "writable by group 50 (mode 775)",
		},
		{
			name:   "dir writable by a group other than staff",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {0, 4, 0o775}}), staff: &staff},
			unsafe: binDir, why: "writable by group 4 (mode 775)",
		},
		{
			name:   "user-owned staff dir",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {1000, staff, 0o775}}), staff: &staff},
			unsafe: binDir, why: "owned by uid 1000",
		},
		{
			name:   "world-writable staff dir",
			path:   binDir,
			fs:     fakeFS{tree: installChain(map[string]owned{binDir: {0, staff, 0o777}}), staff: &staff},
			unsafe: binDir, why: "writable by others (mode 777)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkRootOnly(c.path, c.fs)
			if c.unsafe == "" {
				if err != nil {
					t.Fatalf("refused a safe chain: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("allowed an install path that %s can be changed under", c.unsafe)
			}
			msg := err.Error()
			fix := "sudo chown root:" + rootGroup + " " + c.unsafe + " && sudo chmod 755 " + c.unsafe
			if !strings.HasPrefix(msg, c.unsafe+" is ") || !strings.Contains(msg, c.why) || !strings.Contains(msg, fix) {
				t.Errorf("message should name %s, say it is %s, and give %q; got:\n%s", c.unsafe, c.why, fix, msg)
			}
		})
	}
}

// hostFS must read owners the way the fake does: /usr/bin is root's, and a
// test's temp directory is not.
func TestHostFSReadsRealOwners(t *testing.T) {
	if err := checkRootOnly("/usr/bin", hostFS{}); err != nil {
		t.Errorf("refused /usr/bin: %v", err)
	}
	if os.Getuid() == 0 {
		t.Skip("running as root: the temp directory is root's too")
	}
	if err := checkRootOnly(t.TempDir(), hostFS{}); err == nil {
		t.Error("allowed a directory this user owns")
	}
}

// staff is trusted on Linux only: on macOS it is every account's primary
// group, so trusting it would let any user replace the binary.
func TestHostFSTrustsStaffOnlyOnLinux(t *testing.T) {
	g, err := user.LookupGroup("staff")
	if err != nil {
		t.Skipf("no staff group to resolve: %v", err)
	}
	gid, ok := hostFS{}.staffGID()
	switch runtime.GOOS {
	case "darwin":
		if ok {
			t.Errorf("trusts staff (gid %d) on macOS", gid)
		}
	case "linux":
		if !ok || strconv.FormatUint(uint64(gid), 10) != g.Gid {
			t.Errorf("staffGID() = %d, %v; want %s, true", gid, ok, g.Gid)
		}
	}
}
