package install

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/nanocloud-io/nanocloud/internal/config"
)

// minMacOS is the oldest macOS whose Virtualization.framework supports EFI boot
// with a persistent variable store and a generic platform identifier.
const minMacOS = 13

// RunChecks performs the macOS host-readiness checks. The argument is the
// libvirt URI on Linux and is ignored here. There are no "ovmf" or "swtpm"
// checks, so the UI keeps its firmware and TPM toggles off.
func RunChecks(string) []Check {
	return []Check{
		checkMacOSVersion(),
		checkHypervisor(),
		checkEntitlement(),
		checkHdiutil(),
		checkLaunchd(),
		checkNetworking(),
	}
}

// NetworkPreflight always succeeds on macOS, where NAT needs no setup.
func NetworkPreflight() error { return nil }

// PrivateDisksUsable is true on macOS, where each VM's helper runs as root.
func PrivateDisksUsable() bool { return true }

func checkMacOSVersion() Check {
	v, err := unix.Sysctl("kern.osproductversion")
	if err != nil {
		return Check{Name: "macos", Required: true, Message: "could not read the macOS version: " + err.Error()}
	}
	major, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if major < minMacOS {
		return Check{Name: "macos", Required: true,
			Message: fmt.Sprintf("macOS %s is too old: nano-cloud needs macOS %d or later", v, minMacOS)}
	}
	return Check{Name: "macos", OK: true, Required: true, Message: "macOS " + v}
}

func checkHypervisor() Check {
	n, err := unix.SysctlUint32("kern.hv_support")
	if err != nil || n != 1 {
		return Check{Name: "virtualization", Required: true,
			Message: "this Mac does not offer hardware virtualization (kern.hv_support is not 1) — inside a VM, nested virtualization must be enabled"}
	}
	return Check{Name: "virtualization", OK: true, Required: true, Message: "hardware virtualization available"}
}

func checkEntitlement() Check {
	exe, err := os.Executable()
	if err != nil {
		return Check{Name: "entitlement", Required: true, Message: "could not find this binary: " + err.Error()}
	}
	if !hasEntitlement(exe) {
		return Check{Name: "entitlement", Required: true,
			Message: exe + " is not signed with " + virtualizationEntitlement + ", so no VM can start",
			Remedy:  "sudo " + exe + " install"}
	}
	return Check{Name: "entitlement", OK: true, Required: true, Message: "signed with " + virtualizationEntitlement}
}

func checkHdiutil() Check {
	if _, err := exec.LookPath("hdiutil"); err != nil {
		return Check{Name: "hdiutil", Required: true, Message: "hdiutil not found on PATH — it ships with macOS in /usr/bin"}
	}
	return Check{Name: "hdiutil", OK: true, Required: true, Message: "hdiutil installed (builds cloud-init seeds)"}
}

func checkLaunchd() Check {
	if err := exec.Command("launchctl", "print", "system/"+label).Run(); err != nil {
		return Check{Name: "launchd", Message: label + " is not loaded — the daemon will not start at boot",
			Remedy: "sudo nanocloud install"}
	}
	return Check{Name: "launchd", OK: true, Message: label + " loaded; starts at boot"}
}

// checkNetworking describes the networking on offer. It is always OK, since
// neither mode needs setup.
func checkNetworking() Check {
	const noBridge = "; bridging onto your LAN and VLANs are not available on macOS yet"
	if cfg, err := config.Load(cfgPath); err == nil && cfg.VZNetwork == config.VZNetworkNAT {
		return Check{Name: "bridge", OK: true,
			Message: "VMs use Apple's NAT (vz_network: nat): they reach this Mac and the internet, but not each other" + noBridge}
	}
	return Check{Name: "bridge", OK: true,
		Message: "VMs share nano-cloud's switch: they reach each other, this Mac and the internet" + noBridge}
}

// PrintChecks writes a human-readable checklist to stdout and reports whether
// every required check passed.
func PrintChecks(checks []Check) bool {
	allOK := true
	for _, c := range checks {
		mark := "✓"
		if !c.OK {
			mark = "✗"
			if c.Required {
				allOK = false
			}
		}
		fmt.Printf("  %s %-20s %s\n", mark, c.Name, c.Message)
		if !c.OK && c.Remedy != "" {
			fmt.Printf("      → %s\n", c.Remedy)
		}
	}
	return allOK
}
