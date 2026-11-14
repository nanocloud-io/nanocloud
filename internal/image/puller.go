package image

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nanocloud-io/nanocloud/internal/storage"
)

// Pull downloads entry into dir, verifies its checksum, and renames the temp
// file into place only if it matches. Progress (0–100) is sent without
// blocking, and the channel is closed before Pull returns.
func Pull(ctx context.Context, entry CatalogEntry, dir string, progress chan<- int) error {
	return PullWithResolver(ctx, entry, dir, progress, NewDirIndexResolver(http.DefaultClient))
}

// PullWithResolver is like Pull with an injected resolver, for tests.
func PullWithResolver(ctx context.Context, entry CatalogEntry, dir string, progress chan<- int, resolver *DirIndexResolver) error {
	defer close(progress)

	if err := os.MkdirAll(dir, storage.DirMode); err != nil {
		return fmt.Errorf("image: create cache dir: %w", err)
	}

	resolved, err := resolver.Resolve(ctx, entry)
	if err != nil {
		return fmt.Errorf("image: resolve %s: %w", entry.ID, err)
	}

	// The cache file is always named entry.Filename, never the resolved remote
	// name, because the cache lookup, cleanup and VM backing path all key off
	// it. resolved.Filename only selects the line in a combined sums file.
	tmpPath := filepath.Join(dir, entry.Filename+".tmp")
	finalPath := filepath.Join(dir, entry.Filename)

	if err = downloadFile(ctx, resolved.ImageURL, tmpPath, resolved.ApproxBytes, progress); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of partial download
		return fmt.Errorf("image: download %s: %w", entry.ID, err)
	}

	sendProgress(progress, 99)

	expected, err := fetchChecksum(ctx, resolved.ChecksumURL, resolved.Filename)
	if err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of partial download
		return fmt.Errorf("image: fetch checksum for %s: %w", entry.ID, err)
	}

	actual, err := hashFile(tmpPath, resolved.ChecksumType)
	if err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of partial download
		return fmt.Errorf("image: hash %s: %w", entry.ID, err)
	}

	if !strings.EqualFold(actual, expected) {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of partial download
		return fmt.Errorf("image: checksum mismatch for %s: got %s, want %s", entry.ID, actual, expected)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup of partial download
		return fmt.Errorf("image: finalize %s: %w", entry.ID, err)
	}

	sendProgress(progress, 100)
	return nil
}

func downloadFile(ctx context.Context, url, dst string, approxBytes int64, progress chan<- int) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort body cleanup

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // write errors are caught inline; close is best-effort

	total := resp.ContentLength
	if total <= 0 {
		total = approxBytes
	}

	var written int64
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		nr, readErr := resp.Body.Read(buf)
		if nr > 0 {
			nw, writeErr := f.Write(buf[:nr])
			written += int64(nw)
			if writeErr != nil {
				return writeErr
			}
			if total > 0 {
				// Reserve 99–100 for checksum verification.
				pct := int(written * 98 / total)
				if pct > 98 {
					pct = 98
				}
				sendProgress(progress, pct)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	return nil
}

// fetchChecksum downloads the checksum file at checksumURL and returns the hex
// hash for filename. It accepts these formats, skipping PGP wrapper lines:
//
//   - GNU coreutils:  "<hash>  <filename>" or "<hash> *<filename>"
//   - BSD / RPM:      "SHA256 (<filename>) = <hash>"  (Fedora, Rocky, Alma)
//   - Single-hash:    bare hex string on its own line (Alpine .sha512)
func fetchChecksum(ctx context.Context, checksumURL, filename string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort body cleanup

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksum server returned %s", resp.Status)
	}

	var singleHash string // fallback for per-file checksum files (Alpine)

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "-") ||
			strings.HasPrefix(line, "#") || strings.HasPrefix(line, "=") {
			continue
		}

		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "SHA256 (") || strings.HasPrefix(upper, "SHA512 (") {
			parenIdx := strings.Index(line, " (")
			if parenIdx < 0 {
				continue
			}
			rest := line[parenIdx+2:] // after "SHA256 ("
			closeParen := strings.Index(rest, ")")
			if closeParen < 0 {
				continue
			}
			name := rest[:closeParen]
			eqIdx := strings.Index(rest[closeParen:], "= ")
			if eqIdx < 0 {
				continue
			}
			hashVal := strings.TrimSpace(rest[closeParen+eqIdx+2:])
			if name == filename {
				return hashVal, nil
			}
			continue
		}

		fields := strings.Fields(line)
		switch len(fields) {
		case 1:
			if isHexHash(fields[0]) {
				singleHash = fields[0]
			}
		default:
			name := strings.TrimPrefix(fields[len(fields)-1], "*")
			if name == filename || strings.HasSuffix(name, "/"+filename) {
				return fields[0], nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	if singleHash != "" {
		return singleHash, nil
	}
	return "", fmt.Errorf("checksum not found for %q in %s", filename, checksumURL)
}

// hashFile returns the lowercase hex digest of the file at path.
func hashFile(path string, ct ChecksumType) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is not actionable

	var h hash.Hash
	switch ct {
	case ChecksumSHA512:
		h = sha512.New()
	default: // ChecksumSHA256 and any unrecognized type default to SHA-256.
		h = sha256.New()
	}
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func sendProgress(ch chan<- int, pct int) {
	select {
	case ch <- pct:
	default:
	}
}

// isHexHash reports whether s is a hex SHA-256 or SHA-512 digest.
func isHexHash(s string) bool {
	if len(s) != 64 && len(s) != 128 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
