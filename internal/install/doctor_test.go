//go:build !darwin

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nanocloud-io/nanocloud/internal/network"
)

// TestCheckOVMF covers the check that decides whether the wizard offers UEFI.
func TestCheckOVMF(t *testing.T) {
	orig := ovmfDirs
	t.Cleanup(func() { ovmfDirs = orig })

	t.Run("absent", func(t *testing.T) {
		ovmfDirs = []string{filepath.Join(t.TempDir(), "nothing-here")}

		c := checkOVMF()
		if c.OK {
			t.Error("OK on a host with no OVMF directory")
		}
		if c.Required {
			t.Error("checkOVMF is Required — UEFI is opt-in, and BIOS is the default")
		}
		if c.Remedy == "" {
			t.Error("no remedy: an unavailable feature with no way to enable it is a dead end")
		}
	})

	// Either location is enough, so a check reading only the first must fail.
	for _, name := range []string{"OVMF", "edk2"} {
		t.Run("present at "+name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, name)
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			ovmfDirs = []string{filepath.Join(root, "OVMF"), filepath.Join(root, "edk2")}

			c := checkOVMF()
			if !c.OK {
				t.Errorf("not OK with %s present: %s", dir, c.Message)
			}
			if c.Remedy != "" {
				t.Errorf("remedy %q offered for a satisfied check", c.Remedy)
			}
		})
	}

	t.Run("a file is not a firmware directory", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "OVMF")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		ovmfDirs = []string{path}

		if checkOVMF().OK {
			t.Error("OK when the path is a file rather than a directory")
		}
	})
}

// TestFirmwareChecksAreNamedForTheWizard pins the check names the frontend
// gates its UEFI and TPM toggles on.
func TestFirmwareChecksAreNamedForTheWizard(t *testing.T) {
	names := map[string]bool{}
	for _, c := range RunChecks("") {
		names[c.Name] = true
	}

	for _, want := range []string{CheckOVMF, CheckSWTPM} {
		if !names[want] {
			t.Errorf("RunChecks does not report a %q check, so the wizard can never enable its toggle", want)
		}
	}
}

// TestBridgeCheck checks that a bridge is judged by its uplink, so a host with
// only docker0 and virbr0 is not told Bridge mode is available.
func TestBridgeCheck(t *testing.T) {
	t.Run("no bridge with an uplink", func(t *testing.T) {
		c := bridgeCheck([]network.Bridge{
			{Name: "docker0", VLANFiltering: true},
			{Name: "virbr0"},
		})

		if strings.Contains(c.Message, "available") {
			t.Errorf("message offers Bridge mode the picker refuses: %q", c.Message)
		}
		for _, b := range []string{"docker0", "virbr0"} {
			if !strings.Contains(c.Message, b) {
				t.Errorf("message names no %s, so the operator cannot tell which bridges were rejected: %q", b, c.Message)
			}
		}
		if c.Remedy == "" {
			t.Error("no remedy on the one outcome that has something to act on")
		}
	})

	t.Run("an uplink makes it usable", func(t *testing.T) {
		c := bridgeCheck([]network.Bridge{{Name: "docker0"}, {Name: "br0", Uplink: "eth0"}})

		if !strings.Contains(c.Message, "br0") || !strings.Contains(c.Message, "eth0") {
			t.Errorf("message names neither the bridge nor its uplink: %q", c.Message)
		}
		if strings.Contains(c.Message, "docker0") {
			t.Errorf("message offers docker0, which reaches nothing off this host: %q", c.Message)
		}
		if c.Remedy != "" {
			t.Errorf("remedy on a host that needs no action: %q", c.Remedy)
		}
	})

	t.Run("never fails the install", func(t *testing.T) {
		for _, bs := range [][]network.Bridge{nil, {{Name: "virbr0"}}, {{Name: "br0", Uplink: "eth0"}}} {
			if c := bridgeCheck(bs); !c.OK || c.Required {
				t.Errorf("bridgeCheck(%v) = OK %v, Required %v — want an optional, passing check", bs, c.OK, c.Required)
			}
		}
	})
}

// The daemon leaves existing VMs' files readable when this says QEMU could not
// open them otherwise, so a wrong "true" breaks VMs on upgrade.
func TestPrivateDisksUsable(t *testing.T) {
	for _, tc := range []struct {
		name, conf string
		want       bool
	}{
		{"no file", "", true},
		{"defaults commented out", "#user = \"root\"\n#dynamic_ownership = 1\n", true},
		{"relabelling on", "dynamic_ownership = 1\n", true},
		{"relabelling off", "dynamic_ownership = 0\n", false},
		{"off without spaces", "dynamic_ownership=0 # NFS pool\n", false},
		{"off, QEMU as root", "user = \"root\"\ndynamic_ownership = 0\n", true},
		{"off, QEMU as uid 0", "user = \"+0\"\ndynamic_ownership = 0\n", true},
		{"off, QEMU as qemu", "user = \"qemu\"\ndynamic_ownership = 0\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "qemu.conf")
			if tc.conf != "" {
				if err := os.WriteFile(path, []byte(tc.conf), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := privateDisksUsable(readLibvirtConf(path)); got != tc.want {
				t.Errorf("privateDisksUsable = %v, want %v", got, tc.want)
			}
		})
	}
}
