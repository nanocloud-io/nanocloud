// Package install implements `nanocloud install`, `uninstall` and `doctor`.
// On macOS every dependency ships with the OS, so install copies and signs the
// binary, writes a default config and a launchd job, and starts it.
package install

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	binDst    = "/usr/local/bin/nanocloud"
	cfgDir    = "/etc/nano-cloud"
	cfgPath   = "/etc/nano-cloud/config.yaml"
	dataDir   = "/Library/Application Support/nano-cloud"
	imagesDir = dataDir + "/images"
	poolDir   = dataDir + "/pools/default"
	logDir    = "/Library/Logs/nanocloud"
	logPath   = logDir + "/nanocloud.log"
	// runDir holds the VM helpers' sockets and status files.
	runDir   = "/var/run/nanocloud"
	label    = "io.nanocloud.nanocloud"
	plistDst = "/Library/LaunchDaemons/" + label + ".plist"
	// defaultPort mirrors the server's default, for the post-install health
	// probe when config.yaml does not pin a port.
	defaultPort = 8080

	serviceManager = "launchd"
	rootGroup      = "wheel"
	// staff is every account's primary group on macOS, so it is not trusted.
	trustStaff = false
)

// virtualizationEntitlement is required by every Virtualization.framework
// machine.
const virtualizationEntitlement = "com.apple.security.virtualization"

// Entitlements is the entitlements plist the binary is signed with. The
// daemon's main package sets it from the embedded darwin/entitlements.plist.
var Entitlements []byte

// defaultConfig is the minimal config written on first install. Images and
// disks share one APFS volume so a new VM's disk can be an instant clone.
const defaultConfig = `storage_pools:
  - name: default
    path: "` + poolDir + `"
    default: true
image_cache_dir: "` + imagesDir + `"
metadata_path: "` + dataDir + `/metadata.json"
`

// Run executes the install sequence. unitBytes is the embedded launchd job.
// Re-running on an installed Mac upgrades it in place.
func Run(unitBytes []byte) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("install must be run as root (try: sudo nanocloud install)")
	}

	// launchd runs binDst as root, so anyone who can change it or rename a
	// directory above it gets root. Directories are checked before the copy so
	// a refusal leaves the host unchanged.
	if err := checkRootOnly(filepath.Dir(binDst), hostFS{}); err != nil {
		return err
	}
	if err := copyBinary(); err != nil {
		return err
	}
	if err := checkRootOnly(binDst, hostFS{}); err != nil {
		return err
	}

	for _, dir := range []string{cfgDir, imagesDir, poolDir, logDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}

	if err := writeFileIfAbsent(cfgPath, []byte(defaultConfig), 0o644); err != nil {
		return err
	}

	// launchd refuses a job file not owned by root or writable by others.
	if err := os.WriteFile(plistDst, unitBytes, 0o644); err != nil {
		return fmt.Errorf("write launchd job %s: %w", plistDst, err)
	}
	if err := restartService(); err != nil {
		return err
	}

	ep := installedEndpoint()
	if url := ep.healthURL(); !waitForHealthy(url, ep.certPath, 20, time.Second) {
		return fmt.Errorf(
			"%s was started but never answered %s — it may have failed to bind the port or crashed while loading config.\nCheck what happened with: tail -n 50 %s",
			label, url, logPath,
		)
	}

	fmt.Printf("nanocloud installed successfully.\n")
	ep.printAccess()
	fmt.Printf("Config: %s\n", cfgPath)
	fmt.Printf("Data:   %s\n", dataDir)
	fmt.Printf("To view logs: tail -f %s\n", logPath)

	fmt.Println()
	fmt.Println("Host readiness:")
	if !PrintChecks(RunChecks("")) {
		fmt.Println()
		fmt.Println("Some required checks above failed. nanocloud is installed but may not")
		fmt.Println("work correctly until they're resolved — see the remedies above, or re-run:")
		fmt.Println("  sudo nanocloud doctor")
	}
	return nil
}

// restartService reloads the launchd job and starts it. bootstrap is retried
// because it can fail briefly while the old process is still exiting.
func restartService() error {
	_ = launchctl("bootout", "system/"+label) //nolint:errcheck // best-effort; may not be loaded
	var err error
	for i := 0; i < 10; i++ {
		if err = launchctl("bootstrap", "system", plistDst); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	_ = launchctl("enable", "system/"+label) //nolint:errcheck // already enabled unless someone disabled it
	return launchctl("kickstart", "-k", "system/"+label)
}

// Uninstall removes the binary, config, launchd job and image cache, and keeps
// VM disks and the metadata store. VM disks on macOS are APFS clones, so they
// do not depend on the cached images. Running VMs are shut down cleanly first.
// It is idempotent. With Purge it deletes the VMs and all state as well.
func Uninstall(opts UninstallOptions) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("uninstall must be run as root (try: sudo nanocloud uninstall)")
	}

	var plan purgePlan
	if opts.Purge {
		var err error
		if plan, err = readPurgePlan(cfgPath, dataDir, imagesDir); err != nil {
			return err
		}
		if !opts.Yes && !confirmPurge(plan, opts.In) {
			return fmt.Errorf("purge not confirmed; nothing was removed")
		}
	}

	_ = launchctl("bootout", "system/"+label) //nolint:errcheck // best-effort; may not be loaded

	// VM helpers outlive the daemon by design, so they are stopped here.
	// SIGTERM makes each one power its guest off cleanly.
	if n := stopProcesses("vz-machine"); n > 0 {
		fmt.Printf("Shut down %d running VM(s).\n", n)
	}
	stopProcesses("vz-switch") // only once no VM is linked into it

	for _, path := range []string{plistDst, binDst, binDst + ".prev", cfgDir, imagesDir} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	if opts.Purge {
		// A VM on macOS is just its files once the helpers are stopped.
		kept, err := purgeFiles(plan)
		if err != nil {
			return err
		}
		for _, dir := range []string{logDir, runDir} {
			if err = os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove %s: %w", dir, err)
			}
		}
		printPurged(plan, kept)
		return nil
	}
	fmt.Printf("nanocloud uninstalled.\n")
	fmt.Printf("Removed: binary, %s, launchd job, and cached images (%s).\n", cfgDir, imagesDir)
	printKept(dataDir, logDir)
	return nil
}

// stopProcesses sends SIGTERM to every `nanocloud <sub>` process and waits up
// to 30 seconds for them to exit. It returns how many there were.
func stopProcesses(sub string) int {
	out, err := exec.Command("pgrep", "-f", binDst+" "+sub).Output()
	if err != nil {
		return 0 // pgrep exits 1 when nothing matches
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid != os.Getpid() {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, pid := range pids {
		for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
	}
	return len(pids)
}

// copyBinary installs the running binary at binDst, signed with the
// virtualization entitlement. The copy is signed before the rename, because
// re-signing in place can invalidate a running process's signature and macOS
// kills it.
func copyBinary() error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source binary: %w", err)
	}
	if dstInfo, statErr := os.Stat(binDst); statErr == nil && os.SameFile(srcInfo, dstInfo) && hasEntitlement(binDst) {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only file; close error is not actionable

	if err = os.MkdirAll("/usr/local/bin", 0o755); err != nil {
		return fmt.Errorf("mkdir /usr/local/bin: %w", err)
	}
	tmp, err := os.CreateTemp("/usr/local/bin", ".nanocloud-install-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { os.Remove(tmpName) }() //nolint:errcheck // best-effort temp file cleanup

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close() //nolint:errcheck // already returning a copy error
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close() //nolint:errcheck // already returning a chmod error
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}
	if !hasEntitlement(tmpName) {
		if err := sign(tmpName); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, binDst); err != nil {
		return fmt.Errorf("install binary to %s: %w", binDst, err)
	}
	return nil
}

// hasEntitlement reports whether the binary at path is signed with the
// virtualization entitlement.
func hasEntitlement(path string) bool {
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", path).CombinedOutput()
	if err != nil {
		// Older codesign spells it ":-" and has no --xml.
		out, err = exec.Command("codesign", "-d", "--entitlements", ":-", path).CombinedOutput()
		if err != nil {
			return false
		}
	}
	return bytes.Contains(out, []byte(virtualizationEntitlement))
}

// sign ad-hoc signs the binary at path with Entitlements, which is enough for a
// locally installed daemon.
func sign(path string) error {
	if len(Entitlements) == 0 {
		return fmt.Errorf("cannot sign %s: no entitlements embedded in this build", path)
	}
	f, err := os.CreateTemp("", "nanocloud-entitlements-*.plist")
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	defer os.Remove(f.Name()) //nolint:errcheck // best-effort temp file cleanup
	if _, err = f.Write(Entitlements); err != nil {
		f.Close() //nolint:errcheck // already failing
		return fmt.Errorf("sign: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	out, err := exec.Command("codesign", "--force", "--sign", "-", "--entitlements", f.Name(), path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign %s: %w\n%s", path, err, out)
	}
	return nil
}

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// writeFileIfAbsent writes content to path only if path does not exist yet,
// so an operator's edited config survives a reinstall.
func writeFileIfAbsent(path string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // write errors are caught inline
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
