package cloudinit_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/nanocloud-io/nanocloud/internal/cloudinit"
)

func validParams() cloudinit.Params {
	return cloudinit.Params{
		Hostname: "test-host",
		Username: "ubuntu",
		Password: "s3cr3t",
	}
}

func TestBuild_MissingHostname(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.Hostname = ""

	err := cloudinit.Build(p, filepath.Join(t.TempDir(), "seed.iso"))
	if err == nil {
		t.Fatal("expected error for empty hostname, got nil")
	}
	if !strings.Contains(err.Error(), "hostname") {
		t.Errorf("error should mention 'hostname', got: %v", err)
	}
}

func TestBuild_MissingUsername(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.Username = ""

	err := cloudinit.Build(p, filepath.Join(t.TempDir(), "seed.iso"))
	if err == nil {
		t.Fatal("expected error for empty username, got nil")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error should mention 'username', got: %v", err)
	}
}

func TestPreflight_ReportsXorrisoMissing(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS builds the seed with hdiutil, not xorriso")
	}
	emptyDir := t.TempDir()
	t.Setenv("PATH", emptyDir)

	err := cloudinit.Preflight()
	if err == nil {
		t.Fatal("expected error when xorriso not on PATH, got nil")
	}
	if !strings.Contains(err.Error(), "xorriso") {
		t.Errorf("error should mention 'xorriso', got: %v", err)
	}
}

func TestBuild_ProducesISO(t *testing.T) {
	t.Parallel()

	if err := cloudinit.Preflight(); err != nil {
		t.Skipf("skipping ISO build test: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "seed.iso")
	p := validParams()
	p.SSHKeys = []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test-key"}

	if err := cloudinit.Build(p, dest); err != nil {
		t.Fatalf("Build failed: %v", err)
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("ISO file not found at %s: %v", dest, err)
	}
	if info.Size() == 0 {
		t.Errorf("ISO file exists but is empty")
	}
	// A service's seed carries its secrets in plain text.
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("seed ISO has mode %04o, want 0600", got)
	}
}

func TestGenerateUserData_ContainsFields(t *testing.T) {
	t.Parallel()

	p := validParams()
	out, err := cloudinit.GenerateUserData(p)
	if err != nil {
		t.Fatalf("GenerateUserData failed: %v", err)
	}

	checks := []struct {
		field string
		want  string
	}{
		{"#cloud-config header", "#cloud-config"},
		{"hostname", "hostname: test-host"},
		{"username", "name: ubuntu"},
		{"sudo", "sudo: ALL=(ALL) NOPASSWD:ALL"},
		{"lock_passwd", "lock_passwd: false"},
		// Top-level, or stock images reject the password.
		{"ssh_pwauth", "\nssh_pwauth: true"},
	}

	for _, c := range checks {
		if !strings.Contains(out, c.want) {
			t.Errorf("user-data missing %s: expected to contain %q\ngot:\n%s", c.field, c.want, out)
		}
	}

	// With Shell empty, as on bash-less images like Alpine, no shell is pinned.
	if strings.Contains(out, "shell:") {
		t.Errorf("user-data must not pin a login shell when Params.Shell is empty (breaks bash-less images like Alpine)\ngot:\n%s", out)
	}
}

func TestGenerateUserData_Shell(t *testing.T) {
	t.Parallel()

	t.Run("shell set", func(t *testing.T) {
		t.Parallel()
		p := validParams()
		p.Shell = "/bin/bash"
		out, err := cloudinit.GenerateUserData(p)
		if err != nil {
			t.Fatalf("GenerateUserData failed: %v", err)
		}
		if !strings.Contains(out, "shell: /bin/bash") {
			t.Errorf("expected user-data to pin shell: /bin/bash\ngot:\n%s", out)
		}
	})

	t.Run("shell empty", func(t *testing.T) {
		t.Parallel()
		p := validParams()
		out, err := cloudinit.GenerateUserData(p)
		if err != nil {
			t.Fatalf("GenerateUserData failed: %v", err)
		}
		if strings.Contains(out, "shell:") {
			t.Errorf("expected no shell key when Params.Shell is empty\ngot:\n%s", out)
		}
	})
}

func TestGenerateUserData_SSHKey(t *testing.T) {
	t.Parallel()

	const sshKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI user@host"

	t.Run("key present", func(t *testing.T) {
		t.Parallel()

		p := validParams()
		p.SSHKeys = []string{sshKey}

		out, err := cloudinit.GenerateUserData(p)
		if err != nil {
			t.Fatalf("GenerateUserData failed: %v", err)
		}
		if !strings.Contains(out, "ssh_authorized_keys") {
			t.Error("user-data should contain 'ssh_authorized_keys' when a key is set")
		}
		if !strings.Contains(out, sshKey) {
			t.Errorf("user-data should contain the SSH key\ngot:\n%s", out)
		}
	})

	t.Run("key absent", func(t *testing.T) {
		t.Parallel()

		p := validParams()
		p.SSHKeys = nil

		out, err := cloudinit.GenerateUserData(p)
		if err != nil {
			t.Fatalf("GenerateUserData failed: %v", err)
		}
		if strings.Contains(out, "ssh_authorized_keys") {
			t.Errorf("user-data should NOT contain 'ssh_authorized_keys' when no keys are set\ngot:\n%s", out)
		}
	})
}

// TestBuild_NeverWritesNetworkConfig guards that the seed has no
// network-config. Cloud images bring their single NIC up on their own, and a
// netplan v2 file with name globs leaves minimal images like Alpine with no
// interface at all. Prove any multi-NIC need on Alpine before relaxing this.
func TestBuild_NeverWritesNetworkConfig(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not installed: skipping ISO build test")
	}

	p := cloudinit.Params{Hostname: "test-vm", Username: "admin", Password: "hunter2"}
	iso := filepath.Join(t.TempDir(), "seed.iso")
	if err := cloudinit.Build(p, iso); err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := os.ReadFile(iso)
	if err != nil {
		t.Fatalf("read iso: %v", err)
	}
	if isoNames(data, "network-config") {
		t.Error("seed ISO carries a network-config; guests must fall back to their image defaults")
	}
	for _, want := range []string{"user-data", "meta-data"} {
		if !isoNames(data, want) {
			t.Errorf("seed ISO is missing %s", want)
		}
	}
}

// isoNames reports whether an ISO's directory records name a file, in every
// spelling the two builders write: as is for xorriso's Rock Ridge, Joliet
// UTF-16BE and upper-case ISO 9660 for hdiutil.
func isoNames(data []byte, name string) bool {
	var joliet []byte
	for _, r := range utf16.Encode([]rune(name)) {
		joliet = append(joliet, byte(r>>8), byte(r))
	}
	return bytes.Contains(data, []byte(name)) || bytes.Contains(data, joliet) ||
		bytes.Contains(data, []byte(strings.ToUpper(name)))
}
