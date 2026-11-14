package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/nanocloud-io/nanocloud/internal/config"
	"github.com/nanocloud-io/nanocloud/internal/settings"
	"github.com/nanocloud-io/nanocloud/internal/storage"
)

// FileSettings are the values only config.yaml can change, as the daemon
// resolved them at startup. Settings shows them read-only, with the file.
type FileSettings struct {
	ConfigPath    string `json:"config_path"`
	Listen        string `json:"listen"`
	LibvirtURI    string `json:"libvirt_uri,omitempty"`
	ImageCacheDir string `json:"image_cache_dir"`
	MetadataPath  string `json:"metadata_path"`
	VZNetwork     string `json:"vz_network,omitempty"`
	// TLSCert is the certificate being served, "" when TLS is off.
	// TLSFingerprint is its SHA-256: what to compare a browser warning with.
	TLSCert        string `json:"tls_cert,omitempty"`
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`
}

type settingsPool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Default bool   `json:"default"`
	// Removable is a pool added in Settings; config.yaml's are changed there.
	Removable bool `json:"removable"`
}

type settingsResponse struct {
	SelfHealing bool           `json:"self_healing"`
	UpdateCheck bool           `json:"update_check"`
	Pools       []settingsPool `json:"pools"`
	File        FileSettings   `json:"file"`
}

var poolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

func (s *Server) updateCheckEnabled() bool {
	if s.settings == nil {
		return s.cfg.UpdateCheckEnabled()
	}
	return s.settings.Get().UpdateCheckEnabled(s.cfg)
}

// storagePools is config.yaml's pools and those added in Settings.
func (s *Server) storagePools() []config.StoragePool {
	if s.settings == nil {
		return s.cfg.StoragePools
	}
	return s.settings.Get().StoragePools(s.cfg)
}

func (s *Server) settingsView(st settings.Settings) settingsResponse {
	added := make(map[string]bool, len(st.Pools))
	for _, p := range st.Pools {
		added[p.Name] = true
	}
	resp := settingsResponse{SelfHealing: st.SelfHealingEnabled(s.cfg), UpdateCheck: st.UpdateCheckEnabled(s.cfg), File: s.fileSettings}
	for _, p := range st.StoragePools(s.cfg) {
		resp.Pools = append(resp.Pools, settingsPool{Name: p.Name, Path: p.Path, Default: p.Default, Removable: added[p.Name]})
	}
	return resp
}

// handleSettingsGet handles GET /settings.
func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	if s.settings == nil {
		http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, s.settingsView(s.settings.Get()))
}

// handleSettingsPatch handles PATCH /settings. Each key present is applied;
// null hands that setting back to config.yaml.
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	for key := range body {
		if key != "self_healing" && key != "default_pool" && key != "update_check" {
			http.Error(w, fmt.Sprintf("unknown setting %q", key), http.StatusBadRequest)
			return
		}
	}
	st, err := s.settings.Update(func(st *settings.Settings) error {
		if raw, ok := body["self_healing"]; ok {
			if err := json.Unmarshal(raw, &st.SelfHealing); err != nil {
				return badRequest("self_healing must be true, false or null")
			}
		}
		if raw, ok := body["update_check"]; ok {
			if err := json.Unmarshal(raw, &st.UpdateCheck); err != nil {
				return badRequest("update_check must be true, false or null")
			}
		}
		if raw, ok := body["default_pool"]; ok {
			var name *string
			if err := json.Unmarshal(raw, &name); err != nil {
				return badRequest("default_pool must be a pool name or null")
			}
			st.DefaultPool = ""
			if name != nil {
				if !hasPool(st.StoragePools(s.cfg), *name) {
					return badRequest(fmt.Sprintf("no storage pool named %q", *name))
				}
				st.DefaultPool = *name
			}
		}
		return nil
	})
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	if s.reconcileLoop != nil {
		s.reconcileLoop.SetEnabled(st.SelfHealingEnabled(s.cfg))
	}
	writeJSON(w, s.settingsView(st))
}

// handleSettingsPoolAdd handles POST /settings/pools {name, path}. The
// directory is made and written to first, so a path the daemon cannot use
// is refused here rather than at the first VM create.
func (s *Server) handleSettingsPoolAdd(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
		return
	}
	var p settings.Pool
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !poolNamePattern.MatchString(p.Name) {
		http.Error(w, "name must be letters, digits, '.', '_' or '-', starting with a letter or digit", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) == "/" {
		http.Error(w, "path must be an absolute directory other than /", http.StatusBadRequest)
		return
	}
	p.Path = filepath.Clean(p.Path)
	for _, existing := range s.storagePools() {
		if existing.Name == p.Name || filepath.Clean(existing.Path) == p.Path {
			http.Error(w, fmt.Sprintf("pool %q already uses that name or path", existing.Name), http.StatusConflict)
			return
		}
	}
	if err := checkWritable(p.Path); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	st, err := s.settings.Update(func(st *settings.Settings) error {
		st.Pools = append(st.Pools, p)
		return nil
	})
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, s.settingsView(st))
}

// handleSettingsPoolRemove handles DELETE /settings/pools/{name}. A pool a VM
// still has a disk in is refused; the directory itself is left alone.
func (s *Server) handleSettingsPoolRemove(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
		return
	}
	name := r.PathValue("name")
	var path string
	for _, p := range s.settings.Get().Pools {
		if p.Name == name {
			path = p.Path
		}
	}
	if path == "" {
		if hasPool(s.cfg.StoragePools, name) {
			http.Error(w, fmt.Sprintf("pool %q is declared in config.yaml; remove it there", name), http.StatusConflict)
		} else {
			http.Error(w, fmt.Sprintf("no storage pool named %q", name), http.StatusNotFound)
		}
		return
	}
	if users := s.vmsUsingPool(path); len(users) > 0 {
		http.Error(w, fmt.Sprintf("pool %q still holds disks of: %s", name, strings.Join(users, ", ")), http.StatusConflict)
		return
	}
	st, err := s.settings.Update(func(st *settings.Settings) error {
		kept := st.Pools[:0]
		for _, p := range st.Pools {
			if p.Name != name {
				kept = append(kept, p)
			}
		}
		st.Pools = kept
		if st.DefaultPool == name {
			st.DefaultPool = ""
		}
		return nil
	})
	if err != nil {
		writeSettingsError(w, err)
		return
	}
	writeJSON(w, s.settingsView(st))
}

// vmsUsingPool names the VMs with a disk under path.
func (s *Server) vmsUsingPool(path string) []string {
	if s.metaStore == nil {
		return nil
	}
	entries, err := s.metaStore.Read()
	if err != nil {
		return []string{"(the VM list could not be read: " + err.Error() + ")"}
	}
	var users []string
	for id, e := range entries {
		for _, d := range disksOf(e) {
			if filepath.Dir(d.Path) == path {
				users = append(users, id)
				break
			}
		}
	}
	sort.Strings(users)
	return users
}

func hasPool(pools []config.StoragePool, name string) bool {
	for _, p := range pools {
		if p.Name == name {
			return true
		}
	}
	return false
}

// checkWritable makes dir and writes a file in it. The systemd unit lets the
// daemon write only below its data directory, so on Linux a refusal comes with
// a drop-in allowing the path (install rewrites the unit itself).
func checkWritable(dir string) error {
	err := os.MkdirAll(dir, storage.DirMode)
	if err == nil {
		var f *os.File
		if f, err = os.CreateTemp(dir, ".nanocloud-write-test-*"); err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return nil
		}
	}
	msg := fmt.Sprintf("nano-cloud cannot write to %s: %v", dir, err)
	if runtime.GOOS == "linux" && (errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission)) {
		msg += fmt.Sprintf(". If the directory is fine, the service's sandbox is refusing it; allow it with:\n"+
			"  sudo systemctl edit nanocloud   # add the two lines:\n"+
			"  [Service]\n  ReadWritePaths=%s\n"+
			"then: sudo systemctl restart nanocloud", dir)
	}
	return errors.New(msg)
}

type badRequest string

func (e badRequest) Error() string { return string(e) }

func writeSettingsError(w http.ResponseWriter, err error) {
	var bad badRequest
	if errors.As(err, &bad) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
