package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nanocloud-io/nanocloud/internal/api"
	"github.com/nanocloud-io/nanocloud/internal/config"
	"github.com/nanocloud-io/nanocloud/internal/hostnet"
	"github.com/nanocloud-io/nanocloud/internal/image"
	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/stats"
	"github.com/nanocloud-io/nanocloud/internal/storage"
	"github.com/nanocloud-io/nanocloud/internal/vm"
)

// minimalWebFS returns an in-memory FS holding the index.html the SPA needs.
func minimalWebFS() *fstest.MapFS {
	return &fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html></html>")},
	}
}

// disconnectedManager returns a vm.Manager that can never connect, for tests
// of the libvirt-unreachable path.
func disconnectedManager() *vm.Manager {
	return vm.NewLazy("qemu+unix:///system?socket=/nonexistent/nano-cloud-test-unreachable.sock")
}

// libvirtManager connects to test:///default, skipping the test when no
// libvirtd is reachable.
func libvirtManager(t *testing.T) *vm.Manager {
	t.Helper()
	m, err := vm.New("test:///default")
	if err != nil {
		t.Skipf("libvirt not available (%v): skipping", err)
	}
	t.Cleanup(func() { m.Close() }) //nolint:errcheck // best-effort cleanup in test
	return m
}

func TestHandleHealth(t *testing.T) {
	cfg := &config.Config{
		LibvirtURI:   "test:///default",
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: nil, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("health: status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("health: decode body: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("health: status = %q, want %q", body["status"], "ok")
	}
}

func TestHandleStoragePools_OK(t *testing.T) {
	poolDir := t.TempDir()
	cfg := &config.Config{
		LibvirtURI: "test:///default",
		StoragePools: []config.StoragePool{
			{Name: "default", Path: poolDir, Default: true},
		},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: nil, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/storage/pools", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("storage/pools: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var pools []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Default    bool   `json:"default"`
		FreeBytes  uint64 `json:"free_bytes"`
		TotalBytes uint64 `json:"total_bytes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&pools); err != nil {
		t.Fatalf("storage/pools: decode body: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	p := pools[0]
	if p.Name != "default" {
		t.Errorf("pool Name = %q, want %q", p.Name, "default")
	}
	if p.Path != poolDir {
		t.Errorf("pool Path = %q, want %q", p.Path, poolDir)
	}
	if !p.Default {
		t.Error("pool Default = false, want true")
	}
	if p.TotalBytes == 0 {
		t.Error("pool TotalBytes = 0, expected > 0 for an accessible path")
	}
	if p.FreeBytes > p.TotalBytes {
		t.Errorf("pool FreeBytes (%d) > TotalBytes (%d), impossible", p.FreeBytes, p.TotalBytes)
	}
}

func TestHandleStoragePools_UnavailablePath(t *testing.T) {
	// A pool with an unreadable path is listed with zero bytes, not a 500.
	cfg := &config.Config{
		LibvirtURI: "test:///default",
		StoragePools: []config.StoragePool{
			{Name: "ghost", Path: "/nonexistent/pool/xyz", Default: true},
		},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: nil, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/storage/pools", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("storage/pools (unavailable path): status = %d, want %d", rec.Code, http.StatusOK)
	}
	var pools []struct {
		Name       string `json:"name"`
		FreeBytes  uint64 `json:"free_bytes"`
		TotalBytes uint64 `json:"total_bytes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&pools); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(pools))
	}
	if pools[0].FreeBytes != 0 || pools[0].TotalBytes != 0 {
		t.Errorf("expected 0 bytes for unavailable path, got free=%d total=%d",
			pools[0].FreeBytes, pools[0].TotalBytes)
	}
}

func TestHandleStoragePools_MultiplePools(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	cfg := &config.Config{
		LibvirtURI: "test:///default",
		StoragePools: []config.StoragePool{
			{Name: "primary", Path: d1, Default: true},
			{Name: "secondary", Path: d2, Default: false},
		},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: nil, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/storage/pools", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var pools []struct {
		Name    string `json:"name"`
		Default bool   `json:"default"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&pools); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pools) != 2 {
		t.Fatalf("len(pools) = %d, want 2", len(pools))
	}
	if pools[0].Name != "primary" || pools[1].Name != "secondary" {
		t.Errorf("unexpected pool order: %v", pools)
	}
}

// TestStatusWithoutBackends pins the 503 each endpoint answers when libvirt or
// an optional manager is missing. The 4xx rows prove validation runs before
// the manager is touched.
func TestStatusWithoutBackends(t *testing.T) {
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: disconnectedManager()})

	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/vms", "", http.StatusServiceUnavailable},
		{http.MethodPut, "/vms", "", http.StatusMethodNotAllowed},
		// Exists is the first manager call, reached only after validation.
		{http.MethodPost, "/vms", vmCreateBody("test-vm"), http.StatusServiceUnavailable},
		// Stats come from the sampler, which has never ticked for this VM.
		{http.MethodGet, "/vms/test/stats", "", http.StatusNotFound},
		// Distinct from the 409 a VM that is not running gets.
		{http.MethodGet, "/vms/test/console", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/network/interfaces", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/network/leases", "", http.StatusServiceUnavailable},
		{http.MethodGet, "/vms/test/snapshots", "", http.StatusServiceUnavailable},
		{http.MethodPost, "/vms/test/snapshots", `{"name":"snap1","description":"before upgrade"}`, http.StatusServiceUnavailable},
		{http.MethodPost, "/vms/test/snapshots", "not json", http.StatusBadRequest},
		{http.MethodPost, "/vms/test/snapshots", `{"description":"no name"}`, http.StatusBadRequest},
		{http.MethodPost, "/vms/test/snapshots/snap1/restore", "", http.StatusServiceUnavailable},
		{http.MethodDelete, "/vms/test/snapshots/snap1", "", http.StatusServiceUnavailable},
		{http.MethodPost, "/storage/pools", "", http.StatusMethodNotAllowed},
		{http.MethodPut, "/storage/pools", "", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/storage/pools", "", http.StatusMethodNotAllowed},
	} {
		if rec := serve(srv, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s %s: status = %d, want %d (body: %s)", tc.method, tc.path, tc.body, rec.Code, tc.want, rec.Body)
		}
	}
}

// TestHandleVMConsole_UnknownVM checks that a console request for a VM that is
// missing or stopped gets a 409 before any WebSocket upgrade.
func TestHandleVMConsole_UnknownVM(t *testing.T) {
	m := libvirtManager(t)

	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/vms/does-not-exist-xyz/console", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("GET /vms/does-not-exist-xyz/console: status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

// TestHandleSnapshotRestore_UnknownVM checks that restoring a snapshot on a
// missing VM is a 404 naming the operation.
func TestHandleSnapshotRestore_UnknownVM(t *testing.T) {
	m := libvirtManager(t)

	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms/does-not-exist-xyz/snapshots/snap1/restore", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /vms/does-not-exist-xyz/snapshots/snap1/restore: status = %d, want %d",
			rec.Code, http.StatusNotFound)
	}
	if !strings.Contains(rec.Body.String(), "restore snapshot") {
		t.Errorf("response body = %q; want it to mention %q", rec.Body.String(), "restore snapshot")
	}
}

// TestHandleVMStartStop_UnknownVMIsNotFound checks that starting or stopping a
// name no domain has is the caller's mistake: a 404 naming the VM, not a 500
// carrying libvirt's own error.
func TestHandleVMStartStop_UnknownVMIsNotFound(t *testing.T) {
	m := libvirtManager(t)

	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	tests := []struct {
		name string
		url  string
	}{
		{"start", "/vms/does-not-exist-xyz/start"},
		{"stop", "/vms/does-not-exist-xyz/stop"},
		{"force stop", "/vms/does-not-exist-xyz/stop?force=true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.url, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("POST %s: status = %d, want %d (body: %s)",
					tc.url, rec.Code, http.StatusNotFound, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "does-not-exist-xyz") {
				t.Errorf("POST %s: body = %q; want it to name the VM", tc.url, rec.Body.String())
			}
		})
	}
}

// testImageID and testImageFilename are a real catalog entry: create resolves
// the ID and stats the cached file before reaching the guards under test.
const (
	testImageID       = "ubuntu-2404-standard"
	testImageFilename = "ubuntu-24.04-server-cloudimg-amd64.img"
)

// vmCreateBody builds a minimal valid POST /vms payload with the given name.
func vmCreateBody(name string) string {
	return `{"name":"` + name + `","image_id":"` + testImageID + `","vcpus":1,"memory_mb":512,"disk_gb":5}`
}

// cachedImageManager returns an image.Manager whose cache already holds the
// test image. The file is a real qcow2 when qemu-img is installed, since a
// create that runs to 201 needs a valid backing file.
func cachedImageManager(t *testing.T) *image.Manager {
	t.Helper()
	cacheDir := t.TempDir()
	imgPath := filepath.Join(cacheDir, testImageFilename)
	if _, err := exec.LookPath("qemu-img"); err == nil {
		if out, cerr := exec.Command("qemu-img", "create", "-f", "qcow2", imgPath, "1M").CombinedOutput(); cerr != nil {
			t.Fatalf("seed real backing image: %v: %s", cerr, out)
		}
	} else if err := os.WriteFile(imgPath, []byte("not a real qcow2"), 0o600); err != nil {
		t.Fatalf("seed image cache: %v", err)
	}
	return image.NewManager(cacheDir)
}

// TestHandleVMCreate_RejectsIllegalNames checks that a name unsafe as a path
// component is refused before anything is written under it.
func TestHandleVMCreate_RejectsIllegalNames(t *testing.T) {
	m := libvirtManager(t)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: cachedImageManager(t), NetManager: nil, SysReader: nil})

	names := []string{
		"../../etc/passwd", // path traversal
		"foo/bar",          // separator
		"-leading-dash",    // must start alphanumeric
		"has space",
		"trailing/",
		strings.Repeat("a", 64), // over the 63-char limit
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(vmCreateBody(name)))
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /vms name=%q: status = %d, want %d (body: %s)",
					name, rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}

	// The pool must be untouched: no partial ISO, no disk, nothing outside it.
	entries, err := os.ReadDir(poolDir)
	if err != nil {
		t.Fatalf("read pool dir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("pool dir is not empty after rejected creates: %v", names)
	}
}

// TestHandleVMCreate_RefusesAnImpossibleCPU checks the CPU shape before the
// pool is touched; libvirt would refuse it only after the seed and disk exist.
func TestHandleVMCreate_RefusesAnImpossibleCPU(t *testing.T) {
	m := libvirtManager(t)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, ImgManager: cachedImageManager(t)})

	// A 2×1×1 topology does not match vcpus=1.
	body := `{"name":"cpu-shape-probe","image_id":"` + testImageID +
		`","vcpus":1,"memory_mb":512,"disk_gb":5,"cpu":{"topology":{"sockets":2,"cores":1,"threads":1}}}`

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body)
	}

	entries, err := os.ReadDir(poolDir)
	if err != nil {
		t.Fatalf("read pool dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("pool dir is not empty after a rejected create: %v", entries)
	}
}

// TestHandleVMCreate_RefusesAVLANOnNAT checks that create validates the
// network as PATCH does, and refuses before writing anything.
func TestHandleVMCreate_RefusesAVLANOnNAT(t *testing.T) {
	m := libvirtManager(t)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, ImgManager: cachedImageManager(t)})

	body := `{"name":"vlan-on-nat-probe","image_id":"` + testImageID +
		`","vcpus":1,"memory_mb":512,"disk_gb":5,"network":{"mode":"nat","vlan_id":20}}`

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body)
	}

	entries, err := os.ReadDir(poolDir)
	if err != nil {
		t.Fatalf("read pool dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("pool dir is not empty after a rejected create: %v", entries)
	}
}

// TestHandleVMCreate_ConflictsWithExistingDisk checks that a create whose disk
// path is taken gets 409 rather than truncating the disk.
func TestHandleVMCreate_ConflictsWithExistingDisk(t *testing.T) {
	m := libvirtManager(t)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: cachedImageManager(t), NetManager: nil, SysReader: nil})

	// Stand in for an orphaned disk from an earlier create.
	diskPath := storage.DiskPathFor(poolDir, "orphaned")
	sentinel := []byte("existing disk contents")
	if err := os.WriteFile(diskPath, sentinel, 0o600); err != nil {
		t.Fatalf("seed disk: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(vmCreateBody("orphaned")))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("POST /vms with existing disk: status = %d, want %d (body: %s)",
			rec.Code, http.StatusConflict, rec.Body.String())
	}

	// The existing disk is still intact.
	got, err := os.ReadFile(diskPath)
	if err != nil {
		t.Fatalf("read disk after rejected create: %v", err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing disk was modified: got %q, want %q", got, sentinel)
	}
}

// TestHandleVMStop_ForceUsesDestroy checks that only ?force=true takes the
// force-off path. Both paths are a 404 for an unknown domain, so the error
// message is what tells them apart.
func TestHandleVMStop_ForceUsesDestroy(t *testing.T) {
	m := libvirtManager(t)

	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	tests := []struct {
		name    string
		url     string
		wantMsg string
	}{
		{"graceful", "/vms/does-not-exist-xyz/stop", "stop vm"},
		{"force", "/vms/does-not-exist-xyz/stop?force=true", "force stop vm"},
		// Anything but the exact string "true" stays graceful.
		{"force=false stays graceful", "/vms/does-not-exist-xyz/stop?force=false", "stop vm"},
		{"force=1 stays graceful", "/vms/does-not-exist-xyz/stop?force=1", "stop vm"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.url, nil)
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("POST %s: status = %d, want %d", tc.url, rec.Code, http.StatusNotFound)
			}
			body := rec.Body.String()
			if !strings.Contains(body, tc.wantMsg) {
				t.Errorf("POST %s: body = %q, want it to mention %q", tc.url, body, tc.wantMsg)
			}
			// "force stop vm" contains "stop vm", so rule out the force path.
			if tc.wantMsg == "stop vm" && strings.Contains(body, "force stop") {
				t.Errorf("POST %s: took the force path, want graceful", tc.url)
			}
		})
	}
}

// seedDomainName returns the name of a domain the test driver has defined, or
// skips. test:///default pre-populates a domain called "test".
func seedDomainName(t *testing.T, m *vm.Manager) string {
	t.Helper()
	vms, err := m.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(vms) == 0 {
		t.Skip("test driver defined no domains; nothing to delete")
	}
	return vms[0].Name
}

// TestHandleVMDelete_RemovesArtifacts covers delete end to end: undefine, then
// the disk, seed ISO and metadata entry. It also pins the naming convention
// the files are found by.
func TestHandleVMDelete_RemovesArtifacts(t *testing.T) {
	m := libvirtManager(t)

	name := seedDomainName(t, m)

	poolDir := t.TempDir()
	diskPath := storage.DiskPathFor(poolDir, name)
	isoPath := filepath.Join(poolDir, name+"-cidata.iso")
	for _, p := range []string{diskPath, isoPath} {
		if writeErr := os.WriteFile(p, []byte("x"), 0o600); writeErr != nil {
			t.Fatalf("seed %s: %v", p, writeErr)
		}
	}

	store := metadata.New(filepath.Join(t.TempDir(), "metadata.json"))
	if writeErr := store.Write(map[string]metadata.VMEntry{
		name: {ID: name, PoolPath: poolDir, NetworkMode: "nat"},
	}); writeErr != nil {
		t.Fatalf("seed metadata: %v", writeErr)
	}

	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: store, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/vms/"+name, nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /vms/%s: status = %d, want %d (body: %s)",
			name, rec.Code, http.StatusNoContent, rec.Body.String())
	}

	for _, p := range []string{diskPath, isoPath} {
		if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
			t.Errorf("%s still exists after delete (stat err: %v)", p, statErr)
		}
	}

	entries, err := store.Read()
	if err != nil {
		t.Fatalf("read metadata after delete: %v", err)
	}
	if _, ok := entries[name]; ok {
		t.Errorf("metadata entry for %q survived the delete", name)
	}

	exists, err := m.Exists(name)
	if err != nil {
		t.Fatalf("Exists after delete: %v", err)
	}
	if exists {
		t.Errorf("domain %q is still defined after delete", name)
	}
}

// TestHandleVMCreate_ConflictsWithExistingDomain checks that reusing a defined
// domain's name gets 409, before any image or pool is resolved.
func TestHandleVMCreate_ConflictsWithExistingDomain(t *testing.T) {
	m := libvirtManager(t)

	name := seedDomainName(t, m)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	srv := api.New(api.Deps{WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: nil, ImgManager: nil, NetManager: nil, SysReader: nil})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(vmCreateBody(name)))
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST /vms reusing domain name %q: status = %d, want %d (body: %s)",
			name, rec.Code, http.StatusConflict, rec.Body.String())
	}

	entries, err := os.ReadDir(poolDir)
	if err != nil {
		t.Fatalf("read pool dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("pool dir is not empty after a rejected create: %d entries", len(entries))
	}
}

// fakeProvisioner stands in for *hostnet.Manager and records what was asked.
type fakeProvisioner struct {
	uplink    string
	err       error
	ensured   []string // "uplink/vid", one per Ensure call, in order
	ensureErr error
	okCalls   int // calls that succeed before ensureErr starts being returned
}

func (f *fakeProvisioner) DefaultRouteInterface() (string, error) { return f.uplink, f.err }
func (f *fakeProvisioner) Ensure(uplink string, vid int) (string, error) {
	f.ensured = append(f.ensured, fmt.Sprintf("%s/%d", uplink, vid))
	if f.ensureErr != nil && len(f.ensured) > f.okCalls {
		return "", f.ensureErr
	}
	return hostnet.BridgeName(vid), nil
}

// testServer bundles a Server with its libvirt manager and metadata store, so
// a test can seed a domain and read back the spec. Unlike specFixture it
// defines no domain up front, which a create test must not have.
type testServer struct {
	*api.Server
	meta    *metadata.Store
	mgr     *vm.Manager
	poolDir string
}

// newTestServerWithProvisioner builds a server for the create and patch paths
// with prov as the VLAN provisioner. A nil prov is valid.
func newTestServerWithProvisioner(t *testing.T, prov api.VLANProvisioner) testServer {
	t.Helper()

	m := libvirtManager(t)

	poolDir := t.TempDir()
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}},
	}
	store := metadata.New(filepath.Join(t.TempDir(), "metadata.json"))

	srv := api.New(api.Deps{
		WebFS: minimalWebFS(), Cfg: cfg, VMManager: m, MetaStore: store,
		ImgManager: cachedImageManager(t), HostNet: prov,
	})
	return testServer{Server: srv, meta: store, mgr: m, poolDir: poolDir}
}

func doJSON(t *testing.T, srv testServer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(srv, method, path, body)
}

// serve sends one request to h; an empty body is sent as none.
func serve(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// seedVM defines a domain matching e and records e in the store. Zero VCPUs,
// MemoryMB and PoolPath get test defaults.
func seedVM(t *testing.T, srv testServer, e metadata.VMEntry) {
	t.Helper()
	if e.VCPUs <= 0 {
		e.VCPUs = 1
	}
	if e.MemoryMB <= 0 {
		e.MemoryMB = 512
	}
	if e.PoolPath == "" {
		e.PoolPath = srv.poolDir
	}

	if err := srv.mgr.Define(vm.VMCreateParams{
		Name:         e.ID,
		DiskPath:     storage.DiskPathFor(e.PoolPath, e.ID),
		CloudInitISO: filepath.Join(e.PoolPath, e.ID+"-cidata.iso"),
		VCPUs:        e.VCPUs, MemoryMB: e.MemoryMB, NetworkMode: "nat",
	}); err != nil {
		t.Fatalf("seed domain %q: %v", e.ID, err)
	}
	t.Cleanup(func() { srv.mgr.Delete(e.ID) }) //nolint:errcheck // best-effort cleanup in test

	if err := srv.meta.Upsert(e.ID, func(entry *metadata.VMEntry) { *entry = e }); err != nil {
		t.Fatalf("seed spec %q: %v", e.ID, err)
	}
}

// TestHandleVMCreate_ProvisionsTheVLANAndRecordsTheUplink checks a tagged
// create: the uplink comes from the default route, the bridge is the one
// Ensure created, and both are recorded in the spec.
func TestHandleVMCreate_ProvisionsTheVLANAndRecordsTheUplink(t *testing.T) {
	// Runs through to 201, so it needs qemu-img and xorriso.
	needCloneTools(t)

	prov := &fakeProvisioner{uplink: "eth0"}
	srv := newTestServerWithProvisioner(t, prov)

	body := `{"name":"web","image_id":"` + testImageID + `","vcpus":1,"memory_mb":512,"disk_gb":1,
	          "network":{"mode":"bridge","vlan_id":20},
	          "cloud_init":{"username":"nano","password":"pw"}}`
	rec := doJSON(t, srv, http.MethodPost, "/vms", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /vms = %d: %s", rec.Code, rec.Body)
	}
	if len(prov.ensured) != 1 || prov.ensured[0] != "eth0/20" {
		t.Fatalf("Ensure calls = %v, want [eth0/20]", prov.ensured)
	}

	e, err := srv.meta.Get("web")
	if err != nil {
		t.Fatalf("no metadata entry for web: %v", err)
	}
	if e.VLANUplink != "eth0" {
		t.Errorf("VLANUplink = %q, want eth0", e.VLANUplink)
	}
	if e.Bridge != "nc-vlan20" {
		t.Errorf("Bridge = %q, want nc-vlan20 — the server picks it, not the client", e.Bridge)
	}

	// Must carry no guest-side <vlan> tag: nc-vlan20 already tags at the host.
	hw, err := srv.mgr.ActiveHardware("web")
	if err != nil {
		t.Fatalf("read live hardware: %v", err)
	}
	if want := (vm.NetworkShape{Mode: metadata.NetworkBridge, Bridge: "nc-vlan20"}); hw.Network != want {
		t.Errorf("defined domain network = %+v, want %+v (no guest-side tag)", hw.Network, want)
	}
}

// TestHandleVMCreate_AProvisioningFailureIs422AndDefinesNothing checks that a
// refused provision answers 422 and leaves nothing behind.
func TestHandleVMCreate_AProvisioningFailureIs422AndDefinesNothing(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0", ensureErr: errors.New(`bridge "nc-vlan20" holds address 10.20.0.1/24`)}
	srv := newTestServerWithProvisioner(t, prov)

	body := `{"name":"web","image_id":"` + testImageID + `","vcpus":1,"memory_mb":512,"disk_gb":1,
	          "network":{"mode":"bridge","vlan_id":20},
	          "cloud_init":{"username":"nano","password":"pw"}}`
	rec := doJSON(t, srv, http.MethodPost, "/vms", body)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /vms = %d, want 422: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "holds address") {
		t.Errorf("the 422 lost the real reason: %s", rec.Body)
	}
	if _, err := srv.meta.Get("web"); !errors.Is(err, metadata.ErrNotFound) {
		t.Errorf("a failed provision left a metadata entry behind (err = %v)", err)
	}

	// Provisioning runs before any write: no domain, no disk.
	exists, err := srv.mgr.Exists("web")
	if err != nil {
		t.Fatalf("check domain exists: %v", err)
	}
	if exists {
		t.Error("a failed provision left a domain defined behind")
	}
	entries, err := os.ReadDir(srv.poolDir)
	if err != nil {
		t.Fatalf("read pool dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("pool dir is not empty after a failed provision: %v", entries)
	}
}

// TestPatchSpec_MovingToANewVLANProvisionsIt checks that moving a VM to a new
// VLAN tag provisions it, not just records it.
func TestPatchSpec_MovingToANewVLANProvisionsIt(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0"}
	srv := newTestServerWithProvisioner(t, prov)
	seedVM(t, srv, metadata.VMEntry{ID: "web", NetworkMode: metadata.NetworkBridge, Bridge: "br0"})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec", `{"network":{"mode":"bridge","vlan_id":30}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body)
	}
	// Twice: once for the patch and once by the redefine under it.
	if len(prov.ensured) == 0 {
		t.Fatal("Ensure was never called for the VLAN being moved to")
	}
	for _, got := range prov.ensured {
		if got != "eth0/30" {
			t.Fatalf("Ensure calls = %v, want every one to be eth0/30", prov.ensured)
		}
	}
	e, err := srv.meta.Get("web")
	if err != nil {
		t.Fatalf("get web: %v", err)
	}
	if e.Bridge != "nc-vlan30" || e.VLANUplink != "eth0" {
		t.Errorf("entry = bridge %q uplink %q, want nc-vlan30 / eth0", e.Bridge, e.VLANUplink)
	}
}

// TestPatchSpec_NetworkPatchWithoutUplinkKeepsIt checks that a network patch
// without vlan_uplink, which is what the UI sends, keeps the stored uplink.
func TestPatchSpec_NetworkPatchWithoutUplinkKeepsIt(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0"}
	srv := newTestServerWithProvisioner(t, prov)
	seedVM(t, srv, metadata.VMEntry{
		ID: "web", NetworkMode: metadata.NetworkBridge,
		Bridge: "nc-vlan20", VLANID: 20, VLANUplink: "eth0",
	})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec", `{"network":{"mode":"bridge","vlan_id":20}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body)
	}
	e, err := srv.meta.Get("web")
	if err != nil {
		t.Fatalf("get web: %v", err)
	}
	if e.VLANUplink != "eth0" {
		t.Errorf("VLANUplink = %q, want eth0 — an omitted field must not zero the server-owned uplink", e.VLANUplink)
	}
	if e.Bridge != "nc-vlan20" {
		t.Errorf("Bridge = %q, want nc-vlan20", e.Bridge)
	}
}

// TestPatchSpec_AnUnrelatedChangeStillEnsuresTheVLAN checks that every
// redefine ensures the VLAN bridge. libvirt accepts a domain naming a missing
// bridge and fails only at its next boot.
func TestPatchSpec_AnUnrelatedChangeStillEnsuresTheVLAN(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0"}
	srv := newTestServerWithProvisioner(t, prov)
	seedVM(t, srv, metadata.VMEntry{
		ID: "web", NetworkMode: metadata.NetworkBridge,
		Bridge: "nc-vlan20", VLANID: 20, VLANUplink: "eth0",
	})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec", `{"cpu":{"mode":"host-passthrough"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body)
	}
	if len(prov.ensured) != 1 || prov.ensured[0] != "eth0/20" {
		t.Fatalf("Ensure calls = %v, want [eth0/20] — a CPU patch redefines onto the VLAN bridge too", prov.ensured)
	}
}

// TestPatchSpec_AFailedRedefineRollsTheSpecBack checks that the spec is rolled
// back when provisioning succeeds but the redefine fails. Otherwise it would
// report restart_required forever.
func TestPatchSpec_AFailedRedefineRollsTheSpecBack(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0", okCalls: 1, ensureErr: errors.New(`"eth0.20" holds address fe80::1/64`)}
	srv := newTestServerWithProvisioner(t, prov)
	seedVM(t, srv, metadata.VMEntry{ID: "web", NetworkMode: metadata.NetworkNAT})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec", `{"network":{"mode":"bridge","vlan_id":20},"cpu":{"mode":"host-passthrough"}}`)
	if rec.Code < 400 {
		t.Fatalf("PATCH = %d, want a failure: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "holds address") {
		t.Errorf("the error lost the real reason: %s", rec.Body)
	}

	e, err := srv.meta.Get("web")
	if err != nil {
		t.Fatalf("get web: %v", err)
	}
	if e.NetworkMode != metadata.NetworkNAT || e.VLANID != 0 || e.Bridge != "" || e.VLANUplink != "" {
		t.Errorf("spec network = %q/%q/%d/%q, want the NAT it had — the domain was never redefined",
			e.NetworkMode, e.Bridge, e.VLANID, e.VLANUplink)
	}
	if e.CPUMode != "" {
		t.Errorf("CPUMode = %q, want it rolled back with the rest of the patch", e.CPUMode)
	}
	hw, err := srv.mgr.ActiveHardware("web")
	if err != nil {
		t.Fatalf("read hardware: %v", err)
	}
	if hw.Network != vm.NetworkShapeOf(e) {
		t.Errorf("domain network %+v and spec %+v disagree", hw.Network, vm.NetworkShapeOf(e))
	}
}

// An out-of-range tag is a bad request, so it must be refused before Ensure is
// asked; the status alone would pass without that check.
func TestPatchSpec_AnOutOfRangeVLANIs400(t *testing.T) {
	prov := &fakeProvisioner{uplink: "eth0"}
	srv := newTestServerWithProvisioner(t, prov)
	seedVM(t, srv, metadata.VMEntry{ID: "web", NetworkMode: metadata.NetworkBridge, Bridge: "br0"})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec", `{"network":{"mode":"bridge","vlan_id":9000}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH = %d, want 400: %s", rec.Code, rec.Body)
	}
	if len(prov.ensured) != 0 {
		t.Errorf("Ensure calls = %v, want none — an impossible tag is refused before the host is asked", prov.ensured)
	}
}

// Untagged, so provisionVLAN is a no-op and normalise alone must drop the
// client's uplink.
func TestPatchSpec_ClientCannotInjectAnUplinkOnAnUntaggedRequest(t *testing.T) {
	srv := newTestServerWithProvisioner(t, nil)
	seedVM(t, srv, metadata.VMEntry{ID: "web", NetworkMode: metadata.NetworkBridge, Bridge: "br0"})

	rec := doJSON(t, srv, http.MethodPatch, "/vms/web/spec",
		`{"network":{"mode":"bridge","vlan_id":0,"vlan_uplink":"evil"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", rec.Code, rec.Body)
	}
	e, err := srv.meta.Get("web")
	if err != nil {
		t.Fatalf("get web: %v", err)
	}
	if e.VLANUplink != "" {
		t.Errorf("VLANUplink = %q, want empty — a client cannot set this field", e.VLANUplink)
	}
}

// storeIn returns a metadata.Store in dir seeded with entries.
func storeIn(t *testing.T, dir string, entries ...metadata.VMEntry) *metadata.Store {
	t.Helper()
	store := metadata.New(filepath.Join(dir, "metadata.json"))
	seed := make(map[string]metadata.VMEntry, len(entries))
	for _, e := range entries {
		seed[e.ID] = e
	}
	if err := store.Write(seed); err != nil {
		t.Fatalf("seed metadata: %v", err)
	}
	return store
}

// specServer wires a Server to m and store with poolDir as the default pool.
// Deps fields left out are nil, which is what these tests want.
func specServer(m *vm.Manager, store *metadata.Store, poolDir string) *api.Server {
	return api.New(api.Deps{
		WebFS:     minimalWebFS(),
		Cfg:       &config.Config{StoragePools: []config.StoragePool{{Name: "default", Path: poolDir, Default: true}}},
		VMManager: m,
		MetaStore: store,
	})
}

// specStore is storeIn+specServer for the common one-entry, no-pool case.
func specStore(t *testing.T, m *vm.Manager, entry metadata.VMEntry) (*metadata.Store, http.Handler) {
	t.Helper()
	store := storeIn(t, t.TempDir(), entry)
	return store, specServer(m, store, os.TempDir())
}

// seedArtifacts writes stand-in disk and seed ISO files for name in poolDir.
func seedArtifacts(t *testing.T, poolDir, name string) (disk, iso string) {
	t.Helper()
	disk = storage.DiskPathFor(poolDir, name)
	iso = filepath.Join(poolDir, name+"-cidata.iso")
	for _, p := range []string{disk, iso} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", p, err)
		}
	}
	return disk, iso
}

// defineProbeDomain defines name so a later undefine has something to act on.
// test:///default never opens ImagePath, so a bogus one is fine.
func defineProbeDomain(t *testing.T, m *vm.Manager, name, disk, iso string) {
	t.Helper()
	if err := m.Create(vm.VMCreateParams{
		Name: name, ImagePath: "/nonexistent/base.img", DiskPath: disk,
		CloudInitISO: iso, VCPUs: 1, MemoryMB: 256, NetworkMode: "nat",
	}); err != nil {
		t.Skipf("could not define probe domain on test:///default (%v): skipping", err)
	}
}

// freezeDir makes dir read-only, so metadata writes fail as on a full disk.
func freezeDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod %s read-only: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func ptr[T any](v T) *T { return &v }

// assertArtifacts asserts each path exists (want true) or is gone (want false).
func assertArtifacts(t *testing.T, want bool, msg string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		_, err := os.Stat(p)
		if want && err != nil {
			t.Errorf("%s: %s missing: %v", msg, filepath.Base(p), err)
		}
		if !want && !os.IsNotExist(err) {
			t.Errorf("%s: %s still exists (stat err: %v)", msg, filepath.Base(p), err)
		}
	}
}

// desiredRunning reads back name's desired power state, failing on nil.
func desiredRunning(t *testing.T, store *metadata.Store, name string) bool {
	t.Helper()
	entries, err := store.Read()
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	entry, ok := entries[name]
	if !ok {
		t.Fatalf("no metadata entry for %q", name)
	}
	running, known := entry.DesiredRunning()
	if !known {
		t.Fatalf("entry for %q has Running=nil; want a determined desired state", name)
	}
	return running
}

// TestHandleVMStop_SpecEffects pins what each stop path writes: both record
// running=false, and only force clears StartedAtUnix.
func TestHandleVMStop_SpecEffects(t *testing.T) {
	const startedAt int64 = 1_700_000_000
	for _, tc := range []struct {
		name       string
		query      string
		wantMarker int64
	}{
		{"graceful", "", startedAt},
		{"force", "?force=true", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := libvirtManager(t)

			domain := seedDomainName(t, m)
			store, srv := specStore(t, m, metadata.VMEntry{
				ID: domain, Running: ptr(true), StartedAtUnix: startedAt,
			})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop"+tc.query, nil))
			if rec.Code != http.StatusNoContent {
				t.Fatalf("stop: status = %d, want %d (body: %s)", rec.Code, http.StatusNoContent, rec.Body.String())
			}

			if desiredRunning(t, store, domain) {
				t.Error("after stop: desired state is still running=true; the reconcile loop would restart this VM")
			}
			entries, readErr := store.Read()
			if readErr != nil {
				t.Fatalf("read metadata: %v", readErr)
			}
			if got := entries[domain].StartedAtUnix; got != tc.wantMarker {
				t.Errorf("StartedAtUnix = %d, want %d", got, tc.wantMarker)
			}
		})
	}
}

// TestHandleVMStart_WritesRunningTrue verifies the start path records desired
// state, which is what brings the VM back after a host reboot.
func TestHandleVMStart_WritesRunningTrue(t *testing.T) {
	m := libvirtManager(t)

	domain := seedDomainName(t, m)
	store, srv := specStore(t, m, metadata.VMEntry{ID: domain, Running: ptr(false)})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/start", nil))
	// test:///default's seeded domain is already running, so start may report
	// that; either way the spec must say the VM is meant to be on.
	if rec.Code != http.StatusNoContent && rec.Code < 500 {
		t.Logf("start returned %d (%s) — the spec assertion below is the point",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	if !desiredRunning(t, store, domain) {
		t.Error("after start: desired state is running=false; a host reboot would leave this VM off")
	}
}

// TestHandleVMStart_KeepsDesiredStateWhenLibvirtFails checks that a failed
// start leaves running=true for the reconcile loop to retry.
func TestHandleVMStart_KeepsDesiredStateWhenLibvirtFails(t *testing.T) {
	const domain = "spec-survives-failed-start"
	store, srv := specStore(t, disconnectedManager(), metadata.VMEntry{ID: domain, Running: ptr(false)})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/start", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start against disconnected libvirt: status = %d, want %d (body: %s)",
			rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	if !desiredRunning(t, store, domain) {
		t.Error("a failed start rolled desired state back to running=false; it must stay true so the loop retries")
	}
}

func TestHandleVMStop_KeepsDesiredStateWhenLibvirtFails(t *testing.T) {
	const domain = "spec-survives-failed-stop"
	store, srv := specStore(t, disconnectedManager(), metadata.VMEntry{ID: domain, Running: ptr(true)})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stop against disconnected libvirt: status = %d, want %d (body: %s)",
			rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	if desiredRunning(t, store, domain) {
		t.Error("a failed stop left desired state at running=true; the loop would restart the VM")
	}
}

// TestHandleVMLifecycle_ToleratesMissingSpecEntry checks that a domain with no
// metadata entry, such as one defined with virsh, can be started and stopped.
func TestHandleVMLifecycle_ToleratesMissingSpecEntry(t *testing.T) {
	m := libvirtManager(t)

	domain := seedDomainName(t, m)
	// Store is empty: no entry for this domain at all.
	store := storeIn(t, t.TempDir())
	srv := specServer(m, store, os.TempDir())

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stop of a VM with no spec entry: status = %d, want %d (body: %s)",
			rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func TestHandleVMStop_AlreadyStoppedIsSuccess(t *testing.T) {
	for _, query := range []string{"", "?force=true"} {
		t.Run("stop"+query, func(t *testing.T) {
			m := libvirtManager(t)

			domain := seedDomainName(t, m)
			store, srv := specStore(t, m, metadata.VMEntry{ID: domain, Running: ptr(true)})

			// First stop takes it down; the second is the one under test.
			for i := range 2 {
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop"+query, nil))
				if rec.Code != http.StatusNoContent {
					t.Fatalf("stop #%d: status = %d, want %d (body: %s)",
						i+1, rec.Code, http.StatusNoContent, rec.Body.String())
				}
			}

			// The spec still has to record the intent, or the reconcile loop
			// would read running=true and start it again.
			if desiredRunning(t, store, domain) {
				t.Error("a repeated stop left desired state at running=true")
			}
		})
	}
}

// TestHandleVMDelete_RetryAfterFailedUndefineRecoversArtifacts checks that a
// DELETE failing at undefine keeps the spec entry and its PoolPath, so a retry
// can still find and remove the disk and ISO.
func TestHandleVMDelete_RetryAfterFailedUndefineRecoversArtifacts(t *testing.T) {
	m := libvirtManager(t)

	const name = "delete-retry-probe"
	poolDir := t.TempDir()
	diskPath, isoPath := seedArtifacts(t, poolDir, name)
	defineProbeDomain(t, m, name, diskPath, isoPath)
	store := storeIn(t, t.TempDir(), metadata.VMEntry{ID: name, PoolPath: poolDir, NetworkMode: "nat"})

	// First attempt: disconnected manager, so the undefine step fails.
	failingSrv := specServer(disconnectedManager(), store, poolDir)
	rec := httptest.NewRecorder()
	failingSrv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/vms/"+name, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("first DELETE (disconnected libvirt): status = %d, want %d (body: %s)",
			rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}

	// The entry survives with its PoolPath, and the files are still on disk.
	entries, err := store.Read()
	if err != nil {
		t.Fatalf("read metadata after failed delete: %v", err)
	}
	entry, ok := entries[name]
	if !ok {
		t.Fatal("metadata entry was removed despite the undefine failing — a retry cannot find PoolPath")
	}
	if entry.PoolPath != poolDir {
		t.Errorf("entry.PoolPath = %q, want %q — a retry cannot locate the disk/ISO without it", entry.PoolPath, poolDir)
	}
	if running, known := entry.DesiredRunning(); !known || running {
		t.Errorf("entry.DesiredRunning() = (%v, %v), want (false, true) — the loop must not restart this VM", running, known)
	}
	assertArtifacts(t, true, "after the failed attempt", diskPath, isoPath)

	// Second attempt: a real, working connection to the same domain.
	workingSrv := specServer(m, store, poolDir)
	rec = httptest.NewRecorder()
	workingSrv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/vms/"+name, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second DELETE (working libvirt): status = %d, want %d (body: %s)",
			rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// The retry must finish the job: files gone, domain gone, entry gone.
	assertArtifacts(t, false, "after the successful retry", diskPath, isoPath)
	if exists, existsErr := m.Exists(name); existsErr != nil {
		t.Fatalf("Exists after retry: %v", existsErr)
	} else if exists {
		t.Error("domain still defined after the successful retry")
	}
	entries, err = store.Read()
	if err != nil {
		t.Fatalf("read metadata after retry: %v", err)
	}
	if _, ok := entries[name]; ok {
		t.Error("metadata entry survived the successful retry")
	}
}

// TestHandleVMDelete_DomainAlreadyGoneStillCleansUp checks that deleting a VM
// whose domain is already gone still removes its files and entry. GET /vms
// lists domains, so a stranded entry would be invisible.
func TestHandleVMDelete_DomainAlreadyGoneStillCleansUp(t *testing.T) {
	m := libvirtManager(t)

	const name = "delete-lost-domain-probe"
	poolDir := t.TempDir()
	diskPath, isoPath := seedArtifacts(t, poolDir, name)
	// No defineProbeDomain: the files and the spec outlive the domain.
	store := storeIn(t, t.TempDir(), metadata.VMEntry{ID: name, PoolPath: poolDir, NetworkMode: "nat"})
	srv := specServer(m, store, poolDir)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/vms/"+name, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE of a VM whose domain is gone: status = %d, want %d (body: %s)",
			rec.Code, http.StatusNoContent, rec.Body.String())
	}

	assertArtifacts(t, false, "after deleting a VM libvirt had already lost", diskPath, isoPath)
	entries, err := store.Read()
	if err != nil {
		t.Fatalf("read metadata after delete: %v", err)
	}
	if _, ok := entries[name]; ok {
		t.Error("metadata entry survived — GET /vms cannot show it and no retry can remove it")
	}
}

// TestHandleVMDelete_ProceedsWhenSpecWriteFails checks that delete fails open
// on a spec-write failure, since deleting a VM is how a full disk is
// reclaimed. A read-only directory does not heal as ENOSPC would, so the
// response still fails, but only after the domain and files are gone.
func TestHandleVMDelete_ProceedsWhenSpecWriteFails(t *testing.T) {
	m := libvirtManager(t)

	const name = "delete-proceeds-probe"
	poolDir := t.TempDir()
	diskPath, isoPath := seedArtifacts(t, poolDir, name)
	defineProbeDomain(t, m, name, diskPath, isoPath)

	metaDir := t.TempDir()
	store := storeIn(t, metaDir, metadata.VMEntry{ID: name, PoolPath: poolDir, NetworkMode: "nat"})
	freezeDir(t, metaDir)
	srv := specServer(m, store, poolDir)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/vms/"+name, nil))

	// The failed spec write must not have stopped the undefine or the cleanup.
	if strings.Contains(rec.Body.String(), "could not record desired state") {
		t.Fatalf("delete blocked on the initial spec write instead of proceeding to libvirt (body: %s)", rec.Body.String())
	}
	assertArtifacts(t, false, "delete never reached the cleanup steps", diskPath, isoPath)
	if exists, existsErr := m.Exists(name); existsErr != nil {
		t.Fatalf("Exists after delete: %v", existsErr)
	} else if exists {
		t.Error("domain still defined — delete never reached vmManager.Delete")
	}
}

// TestHandleVMStop_ForceProceedsWhenSpecWriteFails checks that force stop
// still reaches DomainDestroy when the spec write fails, as it would with the
// disk holding metadata.json full.
func TestHandleVMStop_ForceProceedsWhenSpecWriteFails(t *testing.T) {
	m := libvirtManager(t)

	domain := seedDomainName(t, m)

	metaDir := t.TempDir()
	store := storeIn(t, metaDir, metadata.VMEntry{ID: domain, Running: ptr(true), StartedAtUnix: 1_700_000_000})
	freezeDir(t, metaDir)
	srv := specServer(m, store, os.TempDir())

	// Force must still succeed.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop?force=true", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("force stop with a broken spec write: status = %d, want %d (body: %s)",
			rec.Code, http.StatusNoContent, rec.Body.String())
	}

	// Graceful stop still fails closed.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/vms/"+domain+"/stop", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("graceful stop with a broken spec write: status = %d, want %d — graceful must still fail closed",
			rec.Code, http.StatusInternalServerError)
	}
}

// GET /stats/history must never encode samples as null: the UI maps over the
// array, and VM() returns nil for a VM not sampled yet.
func TestHandleStatsHistory_EmptySeriesIsArrayNotNull(t *testing.T) {
	cfg := &config.Config{
		StoragePools: []config.StoragePool{{Name: "default", Path: os.TempDir(), Default: true}},
	}
	srv := api.New(api.Deps{Cfg: cfg})

	for _, path := range []string{"/stats/history", "/stats/history?vm=never-sampled"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		var got struct {
			Samples []stats.Sample `json:"samples"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
		}
		if got.Samples == nil {
			t.Errorf("GET %s: samples = null, want []", path)
		}
	}
}
