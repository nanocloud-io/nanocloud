package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nanocloud-io/nanocloud/internal/install"
	"github.com/nanocloud-io/nanocloud/internal/update"
	"github.com/nanocloud-io/nanocloud/internal/version"
)

// cmdUpdate replaces the installed binary with a verified release and
// restarts the daemon on it. The new binary runs its own install, so a
// release's installation changes apply on update too. If the daemon does not
// come back on the new version, the previous binary is reinstalled.
func cmdUpdate(args []string) {
	fset := flag.NewFlagSet("update", flag.ExitOnError)
	check := fset.Bool("check", false, "only report whether an update is available")
	want := fset.String("version", "", "install this release (vX.Y.Z) instead of the latest")
	_ = fset.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tag := *want
	if tag == "" {
		latest, err := update.Latest(ctx, &http.Client{Timeout: 15 * time.Second}, "https://api.github.com/repos/"+update.Repo+"/releases/latest")
		if err != nil {
			failUpdate(err)
		}
		tag = latest.Tag
	}
	if *check {
		fmt.Printf("installed: %s\nlatest:    %s\n", version.Version, tag)
		if update.Newer(tag, version.Version) {
			fmt.Println("An update is available: sudo nanocloud update")
		}
		return
	}
	if *want == "" && !update.Newer(tag, version.Version) {
		fmt.Printf("nanocloud %s is up to date.\n", version.Version)
		return
	}
	if *want != "" && *want == version.Version {
		fmt.Printf("nanocloud %s is already installed.\n", version.Version)
		return
	}
	if os.Geteuid() != 0 {
		fail("update: must run as root (try: sudo nanocloud update)")
	}

	if err := installRelease(ctx, tag); err != nil {
		failUpdate(err)
	}
}

// installRelease downloads, verifies and installs tag, rolling back if the
// daemon does not come back on it. It returns rather than exiting so the
// download directory is always removed.
func installRelease(ctx context.Context, tag string) error {
	// Checked here, not left to the new binary's install, because the download
	// and the .prev copy that rollback runs as root are written first.
	if err := install.CheckBinPath(); err != nil {
		return err
	}

	// The download sits beside the installed binary: /tmp may be noexec, and
	// the new binary has to run to install itself.
	work, err := os.MkdirTemp(filepath.Dir(install.BinPath), ".nanocloud-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work) //nolint:errcheck // best-effort cleanup

	fmt.Printf("Downloading nanocloud %s…\n", tag)
	newBin, err := update.Download(ctx, &http.Client{}, update.DownloadBase, tag, work)
	if err != nil {
		return err
	}
	fmt.Println("Signature and checksum verified.")

	prev := install.BinPath + ".prev"
	if err = copyExecutable(install.BinPath, prev); err != nil {
		return fmt.Errorf("keep the current binary as %s: %w", prev, err)
	}

	if err = swap(newBin, prev, runInstall, func() error { return waitForVersion(tag) }); err != nil {
		return err
	}
	fmt.Printf("\nUpdated nanocloud %s → %s.\n", version.Version, tag)
	fmt.Printf("The previous binary is kept at %s; to go back: sudo %s install\n", prev, prev)
	return nil
}

// swap installs newBin and confirms the daemon came back on it. If either
// step fails, prev is installed again.
func swap(newBin, prev string, install func(bin string) error, confirm func() error) error {
	err := install(newBin)
	if err == nil {
		err = confirm()
	}
	if err == nil {
		return nil
	}

	fmt.Fprintf(os.Stderr, "\nupdate: %v\nRolling back to %s…\n", err, version.Version)
	if rbErr := install(prev); rbErr != nil {
		return fmt.Errorf("the rollback failed too: %w\nThe previous binary is at %s; run: sudo %s install", rbErr, prev, prev)
	}
	return fmt.Errorf("rolled back to %s", version.Version)
}

// failUpdate exits with err, named once: most of the update package's errors
// already begin with "update:".
func failUpdate(err error) {
	fail("update: %s", strings.TrimPrefix(err.Error(), "update: "))
}

func runInstall(bin string) error {
	cmd := exec.Command(bin, "install")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s install: %w", bin, err)
	}
	return nil
}

// waitForVersion polls the local daemon's /health until it reports tag, which
// catches a unit that restarted on the old binary.
func waitForVersion(tag string) error {
	var seen, url string
	for range 30 {
		u, client, err := install.DaemonHealth()
		url = u
		if err == nil {
			if v, err := healthVersion(client, u); err == nil {
				if v == tag {
					return nil
				}
				seen = v
			}
		}
		time.Sleep(time.Second)
	}
	if seen != "" {
		return fmt.Errorf("the daemon is still reporting %s, not %s", seen, tag)
	}
	return fmt.Errorf("the daemon did not answer at %s", url)
}

func healthVersion(client *http.Client, url string) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only response body
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return "", err
	}
	return body.Version, nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // already failing
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
