// Package cloudinit generates cloud-init NoCloud seed ISOs for VM provisioning.
package cloudinit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nanocloud-io/nanocloud/internal/metadata"
)

// Params holds the cloud-init configuration supplied by the user at VM create time.
type Params struct {
	// Hostname is required. It is also the meta-data local-hostname.
	Hostname string

	// Username is required. The user gets sudo and an unlocked password.
	Username string

	// Password is required. Only its bcrypt hash is written to user-data.
	Password string

	// SSHKeys are the user's authorized keys, pasted or imported from GitHub.
	SSHKeys []string

	// Shell is the login shell, resolved per distro by the caller. Leave it empty
	// on images without bash, such as Alpine.
	Shell string

	// Groups are resolved per distro by the caller. Only useradd reads them, so
	// a user the image already ships keeps the groups it has.
	Groups []string

	// Packages are installed on first boot. Naming any also turns on
	// package_update.
	Packages []string

	// RunCmd are shell commands run once, late in first boot, in order.
	RunCmd []string

	// WriteFiles are written before RunCmd runs, so a command can read them.
	WriteFiles []metadata.WriteFile

	// GuestAgent installs qemu-guest-agent for the given init system; empty
	// installs nothing.
	GuestAgent GuestAgent
}

// Build renders user-data and meta-data from params and writes a NoCloud seed
// ISO to destPath, an absolute path. Call Preflight first to check that the
// ISO tool is installed.
func Build(params Params, destPath string) error {
	userData, err := GenerateUserData(params)
	if err != nil {
		return err
	}

	metaData, err := generateMetaData(params.Hostname)
	if err != nil {
		return fmt.Errorf("generating meta-data: %w", err)
	}
	return writeSeed(userData, metaData, destPath)
}

// writeSeed stages the two NoCloud documents in a temp directory and builds
// the ISO from them. BuildClone shares it.
func writeSeed(userData, metaData, destPath string) error {
	tmpDir, err := os.MkdirTemp("", "cloud-init-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort temp dir cleanup

	srcDir := filepath.Join(tmpDir, "cidata")
	if err := os.Mkdir(srcDir, 0o700); err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "user-data"), []byte(userData), 0o600); err != nil {
		return fmt.Errorf("writing user-data: %w", err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "meta-data"), []byte(metaData), 0o600); err != nil {
		return fmt.Errorf("writing meta-data: %w", err)
	}

	// The ISO tools create their output with the umask's mode, so the image is
	// built in the private temp directory and only its bytes reach destPath.
	iso := filepath.Join(tmpDir, "seed.iso")
	if err := buildISO(srcDir, iso); err != nil {
		return err
	}
	return installPrivate(iso, destPath)
}

// installPrivate copies src to path through a 0600 temp file beside it.
func installPrivate(src, path string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("cloudinit: %w", err)
	}
	defer in.Close() //nolint:errcheck // read-only

	out, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("cloudinit: write seed: %w", err)
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(out.Name(), path)
	}
	if err != nil {
		os.Remove(out.Name()) //nolint:errcheck // best-effort cleanup
		return fmt.Errorf("cloudinit: write seed %s: %w", path, err)
	}
	return nil
}

// validateParams returns an error if required fields are missing or if a
// provisioning extra names something cloud-init would act on wrongly.
func validateParams(params Params) error {
	if strings.TrimSpace(params.Hostname) == "" {
		return errors.New("cloudinit: hostname is required")
	}
	if strings.TrimSpace(params.Username) == "" {
		return errors.New("cloudinit: username is required")
	}
	if params.Password == "" {
		return errors.New("cloudinit: password is required")
	}

	for _, f := range params.WriteFiles {
		// A relative path lands somewhere nothing looks, while cloud-init reports
		// success.
		if !strings.HasPrefix(f.Path, "/") {
			return fmt.Errorf("cloudinit: write_files path %q must be absolute", f.Path)
		}
		if f.Permissions != "" && !octalMode.MatchString(f.Permissions) {
			return fmt.Errorf("cloudinit: write_files permissions %q for %s must be an octal mode like \"0644\"", f.Permissions, f.Path)
		}
	}
	return nil
}

// octalMode matches the "0644" form. cloud-init reads a bare "644" as decimal.
var octalMode = regexp.MustCompile(`^0[0-7]{3}$`)

// newUUIDv4 generates a random UUID v4 string using crypto/rand.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// Version 4, RFC 4122 variant.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	), nil
}
