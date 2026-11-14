//go:build !darwin

package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/network"
)

// DefaultLibvirtURI is used for the libvirt connection check when no config
// override is available.
const DefaultLibvirtURI = "qemu:///system"

// RunChecks performs the host-readiness checks shared by the post-install
// summary and `nanocloud doctor`. An empty libvirtURI means DefaultLibvirtURI.
func RunChecks(libvirtURI string) []Check {
	if libvirtURI == "" {
		libvirtURI = DefaultLibvirtURI
	}
	return []Check{
		checkBinary("xorriso", "sudo apt install xorriso  # or: sudo dnf install xorriso"),
		checkBinary("ip", "sudo apt install iproute2  # or: sudo dnf install iproute2"),
		checkBinary("virsh", "sudo apt install libvirt-daemon-system  # or: sudo dnf install libvirt"),
		checkBinary("qemu-img", "sudo apt install qemu-utils  # or: sudo dnf install qemu-img"),
		checkKVM(),
		checkLibvirtd(),
		checkDefaultNetwork(),
		checkLibvirtConnection(libvirtURI),
		checkDiskOwnership(),
		checkBridge(),
		checkOVMF(),
		checkSWTPM(),
	}
}

// NetworkPreflight checks that libvirtd is running and the 'default' network is
// active, using the same checks as RunChecks. The error carries a remedy
// command, so VM create can replace an opaque libvirt error with it.
func NetworkPreflight() error {
	if c := checkLibvirtd(); !c.OK {
		return checkErr(c)
	}
	if c := checkDefaultNetwork(); !c.OK {
		return checkErr(c)
	}
	return nil
}

const qemuConfPath = "/etc/libvirt/qemu.conf"

// PrivateDisksUsable reports whether QEMU can open a disk only root may read:
// libvirt chowns each to QEMU's user while its domain runs unless qemu.conf
// turns dynamic_ownership off, and QEMU running as root needs no chown.
func PrivateDisksUsable() bool { return privateDisksUsable(readLibvirtConf(qemuConfPath)) }

func privateDisksUsable(conf map[string]string) bool {
	if conf["dynamic_ownership"] != "0" {
		return true
	}
	return conf["user"] == "root" || conf["user"] == "+0"
}

// readLibvirtConf returns a libvirt config file's settings, unquoted. An
// unreadable file has none, which means libvirt's defaults.
func readLibvirtConf(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	conf := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if k, v, ok := strings.Cut(line, "="); ok {
			conf[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return conf
}

func checkDiskOwnership() Check {
	const name = "disk ownership"
	if PrivateDisksUsable() {
		return Check{Name: name, OK: true, Required: true, Message: "libvirt hands each VM's disks to QEMU while it runs"}
	}
	return Check{
		Name:     name,
		Required: true,
		Message:  "dynamic_ownership is off in " + qemuConfPath + ", so QEMU cannot open VM disks and seeds, which only root may read",
		Remedy:   "set dynamic_ownership = 1 in " + qemuConfPath + ", then: sudo systemctl restart libvirtd",
	}
}

func checkErr(c Check) error {
	if c.Remedy == "" {
		return errors.New(c.Message)
	}
	return fmt.Errorf("%s. Run: %s", c.Message, c.Remedy)
}

func checkBinary(name string, remedy string) Check {
	if _, err := exec.LookPath(name); err != nil {
		return Check{Name: name, Required: true, Message: name + " not found on PATH", Remedy: remedy}
	}
	return Check{Name: name, OK: true, Required: true, Message: name + " installed"}
}

// checkKVM is advisory: without /dev/kvm libvirt falls back to emulation, so
// VMs still boot, many times slower.
func checkKVM() Check {
	fi, err := os.Stat("/dev/kvm")
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return Check{Name: "kvm", Message: "/dev/kvm not found — VMs run without hardware acceleration, many times slower",
			Remedy: "enable virtualization (VT-x/AMD-V) in the firmware, or nested virtualization if this host is a VM, then: sudo modprobe kvm"}
	}
	return Check{Name: "kvm", OK: true, Message: "hardware acceleration available (/dev/kvm)"}
}

func checkLibvirtd() Check {
	const name = "libvirtd"
	if runQuiet("systemctl", "is-active", "--quiet", "libvirtd") == nil {
		return Check{Name: name, OK: true, Required: true, Message: "libvirtd is active"}
	}
	// Some distros only socket-activate libvirtd.
	if runQuiet("systemctl", "is-active", "--quiet", "libvirtd.socket") == nil {
		return Check{Name: name, OK: true, Required: true, Message: "libvirtd.socket is active (socket-activated)"}
	}
	return Check{
		Name:     name,
		Required: true,
		Message:  "libvirtd is not running",
		Remedy:   "sudo systemctl enable --now libvirtd",
	}
}

func checkDefaultNetwork() Check {
	const name = "default network"
	if _, err := exec.LookPath("virsh"); err != nil {
		return Check{Name: name, Required: true, Message: "cannot check — virsh not installed"}
	}
	out, err := exec.Command("virsh", "net-info", "default").CombinedOutput()
	if err != nil {
		return Check{
			Name:     name,
			Required: true,
			Message:  "libvirt 'default' network does not exist",
			Remedy:   "sudo virsh net-define /usr/share/libvirt/networks/default.xml && sudo virsh net-start default && sudo virsh net-autostart default",
		}
	}
	if active, ok := parseInfoField(string(out), "Active"); ok && strings.EqualFold(active, "yes") {
		return Check{Name: name, OK: true, Required: true, Message: "'default' network is active"}
	}
	return Check{
		Name:     name,
		Required: true,
		Message:  "'default' network exists but is inactive — VM networking will fail",
		Remedy:   "sudo virsh net-start default && sudo virsh net-autostart default",
	}
}

func checkLibvirtConnection(uri string) Check {
	const name = "libvirt connection"
	if _, err := exec.LookPath("virsh"); err != nil {
		return Check{Name: name, Required: true, Message: "cannot check — virsh not installed"}
	}
	if err := runQuiet("virsh", "-c", uri, "uri"); err != nil {
		return Check{
			Name:     name,
			Required: true,
			Message:  fmt.Sprintf("cannot connect to libvirt at %s", uri),
			Remedy:   "sudo systemctl status libvirtd  # confirm the daemon is running and reachable",
		}
	}
	return Check{Name: name, OK: true, Required: true, Message: fmt.Sprintf("connected to %s", uri)}
}

// CheckOVMF and CheckSWTPM are the names the firmware checks report under.
// The wizard gates its UEFI and TPM toggles on these exact strings.
const (
	CheckOVMF  = "ovmf"
	CheckSWTPM = "swtpm"
)

// ovmfDirs are where Debian/Ubuntu and the RHEL family install OVMF.
var ovmfDirs = []string{"/usr/share/OVMF", "/usr/share/edk2"}

// checkOVMF reports whether UEFI firmware is installed. It is optional, but OK
// must track actual presence: the wizard offers the UEFI toggle only when it
// is true.
func checkOVMF() Check {
	for _, dir := range ovmfDirs {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return Check{
				Name: CheckOVMF, OK: true, Required: false,
				Message: "UEFI firmware present at " + dir + " — UEFI and secure boot available",
			}
		}
	}
	return Check{
		Name: CheckOVMF, Required: false,
		Message: "OVMF not found — VMs can boot BIOS, but UEFI and secure boot are unavailable",
		Remedy:  "sudo apt install ovmf  # or: sudo dnf install edk2-ovmf",
	}
}

// checkSWTPM reports whether an emulated TPM can be backed. The remedy also
// installs swtpm-tools, which holds the setup helper libvirt calls.
func checkSWTPM() Check {
	if _, err := exec.LookPath(CheckSWTPM); err != nil {
		return Check{
			Name: CheckSWTPM, Required: false,
			Message: "swtpm not found — VMs cannot be given an emulated TPM 2.0",
			Remedy:  "sudo apt install swtpm swtpm-tools  # or: sudo dnf install swtpm swtpm-tools",
		}
	}
	return Check{
		Name: CheckSWTPM, OK: true, Required: false,
		Message: "swtpm installed — emulated TPM 2.0 available",
	}
}

// checkBridge reports which host bridges a VM can reach the network through.
// It is informational only: nano-cloud never creates or modifies the
// operator's LAN bridge, and its own nc-vlan<N> bridges are not reported here.
func checkBridge() Check {
	bridges, err := network.ListBridges("")
	if err != nil {
		return Check{Name: bridgeCheckName, OK: true, Required: false,
			Message: fmt.Sprintf("could not inspect host bridges: %v", err)}
	}
	return bridgeCheck(bridges)
}

const bridgeCheckName = "bridge"

// bridgeCheck is checkBridge's decision, separated from the sysfs read for
// tests. It judges a bridge by its uplink, never by vlan_filtering, matching
// what the picker offers and validateNetwork accepts.
func bridgeCheck(bridges []network.Bridge) Check {
	var usable, portless []string
	for _, b := range bridges {
		if b.Uplink != "" {
			usable = append(usable, fmt.Sprintf("%s (uplink %s)", b.Name, b.Uplink))
			continue
		}
		portless = append(portless, b.Name)
	}

	if len(usable) > 0 {
		return Check{Name: bridgeCheckName, OK: true, Required: false,
			Message: fmt.Sprintf("bridge(s) with a physical uplink: %s — Bridge network mode available", strings.Join(usable, ", "))}
	}
	msg := "no host bridge with a physical port — Bridge network mode disabled (NAT still works, and a VLAN needs no host bridge: nano-cloud builds its own)"
	if len(portless) > 0 {
		verb := "has"
		if len(portless) > 1 {
			verb = "have"
		}
		msg = fmt.Sprintf("%s %s no physical port, so it reaches nothing off this host — Bridge network mode disabled (NAT still works, and a VLAN needs no host bridge: nano-cloud builds its own)",
			strings.Join(portless, ", "), verb)
	}
	return Check{Name: bridgeCheckName, OK: true, Required: false, Message: msg,
		Remedy: fmt.Sprintf("nano-cloud does not create host bridges; create one, e.g. %s, and attach the uplink to it", metadata.DefaultBridge)}
}

// parseInfoField extracts the value of a "Field:   value" line from virsh's
// info tables. It matches on the field name, not column widths, because the
// padding varies across libvirt versions.
func parseInfoField(output, field string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(parts[0]), field) {
			return strings.TrimSpace(parts[1]), true
		}
	}
	return "", false
}

func runQuiet(name string, args ...string) error {
	return exec.Command(name, args...).Run()
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
