//go:build !darwin

// Package install implements the nanocloud install and uninstall subcommands.
package install

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nanocloud-io/nanocloud/internal/vm"
)

const (
	binDst      = "/usr/local/bin/nanocloud"
	cfgDir      = "/etc/nano-cloud"
	cfgPath     = "/etc/nano-cloud/config.yaml"
	dataDir     = "/var/lib/nano-cloud"
	imagesDir   = "/var/lib/nano-cloud/images"
	vmDisksDir  = "/var/lib/nano-cloud/vm-disks"
	unitDst     = "/etc/systemd/system/nanocloud.service"
	serviceName = "nanocloud"
	// defaultPort mirrors the server's default, for the post-install health
	// probe when config.yaml does not pin a port.
	defaultPort = 8080

	serviceManager = "systemd"
	rootGroup      = "root"
	// trustStaff accepts a directory that group staff can write. Debian lays
	// out /usr/local as root:staff 2775 and documents staff as able to modify
	// it, so its members can already replace binaries on root's PATH.
	trustStaff = true
)

// defaultConfig is the minimal config written on first install.
const defaultConfig = `storage_pools:
  - name: default
    path: /var/lib/nano-cloud/vm-disks
    default: true

# port: 8080
# image_cache_dir: /var/lib/nano-cloud/images
`

// hostDep is a required host-level dependency.
type hostDep struct {
	binary string // command to check with exec.LookPath
	debian string // apt package name
	fedora string // dnf package name
}

var requiredDeps = []hostDep{
	{binary: "virsh", debian: "libvirt-daemon-system", fedora: "libvirt"},
	{binary: "xorriso", debian: "xorriso", fedora: "xorriso"},
	{binary: "qemu-img", debian: "qemu-utils", fedora: "qemu-img"},
}

// installHostDeps installs any missing dependency with the host's package
// manager. On an unsupported distro it warns instead of failing.
func installHostDeps() error {
	isDebian := fileExists("/etc/debian_version")
	isFedora := fileExists("/etc/fedora-release") || fileExists("/etc/redhat-release")

	for _, dep := range requiredDeps {
		if _, err := exec.LookPath(dep.binary); err == nil {
			continue // already installed
		}

		fmt.Printf("Installing %s...\n", dep.binary)

		var cmd *exec.Cmd
		switch {
		case isDebian:
			cmd = exec.Command("apt-get", "install", "-y", dep.debian)
			cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
		case isFedora:
			cmd = exec.Command("dnf", "install", "-y", dep.fedora)
		default:
			fmt.Printf("warning: unsupported distro — please install %q manually\n", dep.binary)
			continue
		}

		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("install %s: %w\n%s", dep.binary, err, out)
		}
		fmt.Printf("Installed %s.\n", dep.binary)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// setupLibvirt enables libvirtd and makes the default NAT network active and
// autostarted. An inactive default network is otherwise silent until a VM
// create fails.
func setupLibvirt() {
	if err := systemctl("enable", "--now", "libvirtd"); err != nil {
		// Some distros only ship the socket unit. Failure here is non-fatal;
		// the doctor summary reports it.
		if sockErr := systemctl("enable", "--now", "libvirtd.socket"); sockErr != nil {
			fmt.Printf("warning: could not enable/start libvirtd: %v\n", err)
			return
		}
	}

	if _, err := exec.LookPath("virsh"); err != nil {
		return
	}

	// net-start fails on an already active network, which is the state we want.
	if err := virsh("net-start", "default"); err != nil && !strings.Contains(err.Error(), "already active") {
		fmt.Printf("warning: could not start libvirt 'default' network: %v\n", err)
	}
	if err := virsh("net-autostart", "default"); err != nil {
		fmt.Printf("warning: could not set libvirt 'default' network to autostart: %v\n", err)
	}
}

// virsh runs the virsh CLI, including its output in the error on failure.
func virsh(args ...string) error {
	cmd := exec.Command("virsh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virsh %v: %w\n%s", args, err, out)
	}
	return nil
}

// waitForActive polls `systemctl is-active` until the unit is active or the
// attempts run out. A Type=simple unit reports started as soon as it forks, so
// this is what catches a crash on startup.
func waitForActive(service string, attempts int, delay time.Duration) bool {
	for i := 0; i < attempts; i++ {
		if runQuiet("systemctl", "is-active", "--quiet", service) == nil {
			return true
		}
		time.Sleep(delay)
	}
	return runQuiet("systemctl", "is-active", "--quiet", service) == nil
}

// serviceActivationArgs is the systemctl sequence Run uses to bring the unit up.
//
// The last verb must be "restart", not "start". copyBinary renames the new
// binary over the old one, but a running daemon keeps executing the old inode,
// and `start` on an active unit is a no-op, so an upgrade would keep serving
// the previous release. `restart` also starts an inactive unit.
func serviceActivationArgs() [][]string {
	return [][]string{
		{"daemon-reload"},
		{"enable", serviceName},
		{"restart", serviceName},
	}
}

// Run executes the install sequence. unitBytes is the embedded systemd unit.
// Re-running on an installed host is safe.
func Run(unitBytes []byte) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("install must be run as root (try: sudo nanocloud install)")
	}

	// systemd runs binDst as root, so anyone who can change it or rename a
	// directory above it gets root. Directories are checked before anything
	// else so a refusal leaves the host unchanged.
	if err := checkRootOnly(filepath.Dir(binDst), hostFS{}); err != nil {
		return err
	}

	if err := installHostDeps(); err != nil {
		return err
	}

	setupLibvirt()

	if err := copyBinary(); err != nil {
		return err
	}
	if err := checkRootOnly(binDst, hostFS{}); err != nil {
		return err
	}

	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cfgDir, err)
	}
	if err := makeStateDirs(dataDir, imagesDir, vmDisksDir); err != nil {
		return err
	}

	if err := writeFileIfAbsent(cfgPath, []byte(defaultConfig), 0o644); err != nil {
		return err
	}

	if err := os.WriteFile(unitDst, unitBytes, 0o644); err != nil {
		return fmt.Errorf("write unit %s: %w", unitDst, err)
	}

	for _, args := range serviceActivationArgs() {
		if err := systemctl(args...); err != nil {
			return err
		}
	}

	if !waitForActive(serviceName, 5, time.Second) {
		return fmt.Errorf(
			"%s did not become active after restart — it may have crashed on startup.\nCheck what happened with: journalctl -u %s -e",
			serviceName, serviceName,
		)
	}

	ep := installedEndpoint()
	if url := ep.healthURL(); !waitForHealthy(url, ep.certPath, 10, time.Second) {
		return fmt.Errorf(
			"%s is active but never answered %s — it may have failed to bind the port or crashed while loading config.\nCheck what happened with: journalctl -u %s -e",
			serviceName, url, serviceName,
		)
	}

	fmt.Printf("nanocloud installed successfully.\n")
	ep.printAccess()
	fmt.Printf("Config: %s\n", cfgPath)
	fmt.Printf("To view logs: journalctl -u %s -f\n", serviceName)

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

// Uninstall removes the binary, config and systemd unit, and keeps VM disks,
// the image cache, the metadata store and the libvirt domains, so existing VMs
// keep running. It is idempotent. With Purge it deletes the VMs and all state.
func Uninstall(opts UninstallOptions) error {
	if os.Getuid() != 0 {
		return fmt.Errorf("uninstall must be run as root (try: sudo nanocloud uninstall)")
	}

	var plan purgePlan
	var lv *vm.Manager
	if opts.Purge {
		var err error
		if plan, err = readPurgePlan(cfgPath, dataDir, imagesDir); err != nil {
			return err
		}
		if !opts.Yes && !confirmPurge(plan, opts.In) {
			return fmt.Errorf("purge not confirmed; nothing was removed")
		}
		// Connect before removing anything, so no file is deleted from under
		// a domain libvirt still defines.
		uri := plan.libvirtURI
		if uri == "" {
			uri = DefaultLibvirtURI
		}
		if lv, err = vm.New(uri); err != nil {
			return fmt.Errorf("purge needs libvirt to delete the VMs, and %s is not reachable: %w\nStart it (sudo systemctl start libvirtd) and re-run, or uninstall without --purge", uri, err)
		}
		defer lv.Close() //nolint:errcheck // best-effort; the process is exiting
	}

	// Stopping the daemon first means nothing restarts a VM the purge powers off.
	_ = systemctl("stop", serviceName)    //nolint:errcheck // best-effort; may already be stopped
	_ = systemctl("disable", serviceName) //nolint:errcheck // best-effort; may already be disabled

	if opts.Purge {
		for _, e := range plan.vms {
			_ = lv.Destroy(e.ID) //nolint:errcheck // most are already off
			if err := lv.Delete(e.ID); err != nil {
				return fmt.Errorf("delete VM %s: %w", e.ID, err)
			}
		}
	}

	for _, path := range uninstallRemoves() {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	_ = systemctl("daemon-reload") //nolint:errcheck // best-effort cleanup

	if opts.Purge {
		kept, err := purgeFiles(plan)
		if err != nil {
			return err
		}
		printPurged(plan, kept)
		return nil
	}
	fmt.Printf("nanocloud uninstalled.\n")
	fmt.Printf("Removed: binary, %s and the systemd unit.\n", cfgDir)
	fmt.Println("libvirt domains are untouched, so your VMs keep running.")
	printKept(dataDir)
	return nil
}

// uninstallRemoves is what an uninstall without --purge deletes. The image
// cache stays: every VM disk is a qcow2 overlay on its image, and a VM whose
// backing image is gone cannot start again.
func uninstallRemoves() []string {
	return []string{unitDst, binDst, binDst + ".prev", cfgDir}
}

// copyBinary copies the running binary to binDst, skipping the copy when both
// are the same file.
func copyBinary() error {
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source binary: %w", err)
	}
	dstInfo, err := os.Stat(binDst)
	if err == nil {
		if os.SameFile(srcInfo, dstInfo) {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", binDst, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only file; close error is not actionable

	// Same directory as binDst, so the rename is atomic.
	tmp, err := os.CreateTemp("/usr/local/bin", ".nanocloud-install-*")
	if err != nil {
		return fmt.Errorf("create temp binary: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { os.Remove(tmpName) }() //nolint:errcheck // best-effort temp file cleanup

	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close() //nolint:errcheck // already returning a copy error; close is best-effort
		return fmt.Errorf("copy binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close() //nolint:errcheck // already returning a chmod error; close is best-effort
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}
	if err := os.Rename(tmpName, binDst); err != nil {
		return fmt.Errorf("install binary to %s: %w", binDst, err)
	}
	return nil
}

// writeFileIfAbsent writes content to path only if path does not already exist,
// so an operator's config is never overwritten.
func writeFileIfAbsent(path string, content []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if os.IsExist(err) {
			return nil // already exists — leave operator's customisations intact
		}
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup; write errors are caught inline
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// systemctl runs systemctl, including its output in the error on failure.
func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w\n%s", args, err, out)
	}
	return nil
}
