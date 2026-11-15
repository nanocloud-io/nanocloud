// Package api provides the HTTP and WebSocket handlers for nano-cloud.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nanocloud-io/nanocloud/internal/auth"
	"github.com/nanocloud-io/nanocloud/internal/cloudinit"
	"github.com/nanocloud-io/nanocloud/internal/config"
	"github.com/nanocloud-io/nanocloud/internal/events"
	"github.com/nanocloud-io/nanocloud/internal/hostdev"
	"github.com/nanocloud-io/nanocloud/internal/hypervisor"
	"github.com/nanocloud-io/nanocloud/internal/image"
	"github.com/nanocloud-io/nanocloud/internal/install"
	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/network"
	"github.com/nanocloud-io/nanocloud/internal/reconcile"
	"github.com/nanocloud-io/nanocloud/internal/services"
	"github.com/nanocloud-io/nanocloud/internal/settings"
	"github.com/nanocloud-io/nanocloud/internal/stats"
	"github.com/nanocloud-io/nanocloud/internal/storage"
	"github.com/nanocloud-io/nanocloud/internal/system"
	"github.com/nanocloud-io/nanocloud/internal/template"
	"github.com/nanocloud-io/nanocloud/internal/update"
	"github.com/nanocloud-io/nanocloud/internal/version"
	"github.com/nanocloud-io/nanocloud/internal/vm"
)

// Server is the HTTP API server.
type Server struct {
	mux        *http.ServeMux
	webFS      fs.FS
	cfg        *config.Config
	vmManager  hypervisor.Backend
	metaStore  *metadata.Store
	imgManager *image.Manager
	netManager *network.Manager
	// sysReader may be nil on non-Linux platforms; /system degrades gracefully.
	sysReader *system.Reader
	// events may be nil; a nil *events.Ring is inert, not a panic.
	events *events.Ring
	// statsSampler may be nil; a nil *stats.Sampler is inert, same pattern.
	statsSampler *stats.Sampler
	// reconcileLoop may be nil; inert, same pattern as events/statsSampler.
	reconcileLoop *reconcile.Loop
	// templates may be nil; the template endpoints then report 503.
	templates *template.Store
	// hostNet may be nil; only a tagged bridge VM needs it.
	hostNet VLANProvisioner
	// auth is nil only in tests that exercise handlers, not the login.
	auth    *auth.Store
	limiter *auth.Limiter
	// passwordChecks holds a slot per login being checked (maxPasswordChecks).
	passwordChecks chan struct{}
	// updates may be nil; GET /system/update then reports the check disabled.
	updates *update.Checker
	// services may be nil; the /services endpoints then report 503.
	services     *services.Manager
	settings     *settings.Store
	fileSettings FileSettings
}

// Deps bundles every dependency Server needs.
type Deps struct {
	// WebFS should be the web/dist subtree.
	WebFS fs.FS
	// Cfg must not be nil.
	Cfg *config.Config
	// VMManager must not be nil. While it is disconnected, handlers answer 503.
	VMManager hypervisor.Backend
	// MetaStore may be nil (degrades gracefully).
	MetaStore *metadata.Store
	// ImgManager may be nil; image endpoints return 503 when nil.
	ImgManager *image.Manager
	// NetManager may be nil; network endpoints return 503 when nil.
	NetManager *network.Manager
	// SysReader may be nil; /system returns zero values on non-Linux platforms.
	SysReader *system.Reader
	// Events backs GET /events and is the ring the reconcile loop appends to.
	// May be nil; the feed is then empty.
	Events *events.Ring
	// Stats backs the CPU and memory figures. May be nil; they then read zero.
	Stats *stats.Sampler
	// Reconcile backs GET /vms' gave-up/crash-looping states. May be nil.
	Reconcile *reconcile.Loop
	// Templates backs /templates. May be nil; those endpoints then report 503.
	Templates *template.Store
	// HostNet provisions VLANs. May be nil; a tagged bridge VM then gets 503.
	HostNet VLANProvisioner
	// Services backs /services. May be nil; those endpoints then report 503.
	Services *services.Manager
	// Auth is the login. Only tests leave it nil, which turns the login off.
	Auth *auth.Store
	// Updates looks for newer releases for GET /system/update. May be nil.
	Updates *update.Checker
	// Settings backs /settings and overlays config.yaml's storage pools. May
	// be nil; pools then come from config.yaml alone and /settings is 503.
	Settings *settings.Store
	// File is what GET /settings reports as changeable only in config.yaml.
	File FileSettings
}

// New creates a Server from the given dependencies.
func New(deps Deps) *Server {
	s := &Server{
		mux:            http.NewServeMux(),
		webFS:          deps.WebFS,
		cfg:            deps.Cfg,
		vmManager:      deps.VMManager,
		metaStore:      deps.MetaStore,
		imgManager:     deps.ImgManager,
		netManager:     deps.NetManager,
		sysReader:      deps.SysReader,
		events:         deps.Events,
		statsSampler:   deps.Stats,
		reconcileLoop:  deps.Reconcile,
		templates:      deps.Templates,
		hostNet:        deps.HostNet,
		services:       deps.Services,
		auth:           deps.Auth,
		limiter:        auth.NewLimiter(),
		passwordChecks: make(chan struct{}, maxPasswordChecks),
		updates:        deps.Updates,
		settings:       deps.Settings,
		fileSettings:   deps.File,
	}
	s.routes()
	// The services manager drives this API in-process, as the daemon itself,
	// so it is handed the mux behind the login rather than the server.
	if s.services != nil {
		s.services.Attach(s.mux)
	}
	return s
}

// maxBodyBytes caps every request body; a create with write_files fits well
// under it.
const maxBodyBytes = 1 << 20

// bodyTimeout bounds how long a client may take to send a request body.
// ReadHeaderTimeout covers only the headers, and a server-wide ReadTimeout
// would cut off the consoles' WebSockets. A variable so tests can shorten it.
var bodyTimeout = 30 * time.Second

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil && r.ContentLength != 0 {
		// Lifted once the body is read: the server's own read that follows
		// cancels the request when a deadline passes, which would cut off a
		// long response, such as a log being followed.
		rc := http.NewResponseController(w)
		if rc.SetReadDeadline(time.Now().Add(bodyTimeout)) == nil {
			r.Body = &deadlineBody{ReadCloser: r.Body, lift: func() { _ = rc.SetReadDeadline(time.Time{}) }}
		}
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	}
	if s.auth != nil && !s.authorize(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

// deadlineBody calls lift once its body has been read to the end.
type deadlineBody struct {
	io.ReadCloser
	lift func()
	once sync.Once
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.lift)
	}
	return n, err
}

func (s *Server) routes() {
	img := func(h http.HandlerFunc) http.HandlerFunc {
		return requires(s.imgManager != nil, "image manager unavailable", h)
	}
	nw := func(h http.HandlerFunc) http.HandlerFunc {
		return requires(s.netManager != nil, "network manager unavailable", h)
	}
	tmpl := func(h http.HandlerFunc) http.HandlerFunc {
		return requires(s.templates != nil, "this daemon has no template store, so templates cannot be read or saved", h)
	}
	svc := func(h http.HandlerFunc) http.HandlerFunc {
		return requires(s.services != nil, "services are unavailable on this host", h)
	}

	s.mux.HandleFunc("/health", s.handleHealth)
	if s.auth != nil {
		s.mux.HandleFunc("GET /auth/status", s.handleAuthStatus)
		s.mux.HandleFunc("POST /auth/login", s.handleLogin)
		s.mux.HandleFunc("POST /auth/logout", s.handleLogout)
		s.mux.HandleFunc("POST /auth/password", s.handleChangePassword)
		s.mux.HandleFunc("GET /auth/tokens", s.handleTokenList)
		s.mux.HandleFunc("POST /auth/tokens", s.handleTokenCreate)
		s.mux.HandleFunc("DELETE /auth/tokens/{id}", s.handleTokenRevoke)
	}
	s.mux.HandleFunc("GET /system", s.handleSystem)
	s.mux.HandleFunc("GET /system/deps", s.handleGetDeps)
	s.mux.HandleFunc("GET /system/update", s.handleUpdateStatus)
	s.mux.HandleFunc("GET /system/devices", s.handleGetDevices)
	s.mux.HandleFunc("GET /cloud-init/github-keys", s.handleGitHubKeys)
	s.mux.HandleFunc("GET /events", s.handleEvents)
	s.mux.HandleFunc("GET /stats/history", s.handleStatsHistory)
	s.mux.HandleFunc("/storage/pools", s.handleStoragePools)
	s.mux.HandleFunc("GET /settings", s.handleSettingsGet)
	s.mux.HandleFunc("PATCH /settings", s.handleSettingsPatch)
	s.mux.HandleFunc("POST /settings/pools", s.handleSettingsPoolAdd)
	s.mux.HandleFunc("DELETE /settings/pools/{name}", s.handleSettingsPoolRemove)
	s.mux.HandleFunc("/vms", s.handleVMs)
	s.mux.HandleFunc("POST /vms", s.handleVMCreate)
	s.mux.HandleFunc("POST /vms/{id}/start", s.handleVMStart)
	s.mux.HandleFunc("POST /vms/{id}/stop", s.handleVMStop)
	s.mux.HandleFunc("DELETE /vms/{id}", s.handleVMDelete)
	s.mux.HandleFunc("GET /vms/{id}/spec", s.handleVMSpec)
	s.mux.HandleFunc("PATCH /vms/{id}/spec", s.handleVMSpecPatch)
	s.mux.HandleFunc("POST /vms/{id}/disks", s.handleDiskAttach)
	s.mux.HandleFunc("DELETE /vms/{id}/disks/{target}", s.handleDiskDetach)
	s.mux.HandleFunc("POST /vms/{id}/disks/{target}/resize", s.handleDiskResize)
	s.mux.HandleFunc("POST /vms/{id}/clone", s.handleVMClone)
	s.mux.HandleFunc("GET /vms/{id}/stats", s.handleVMStats)
	s.mux.HandleFunc("GET /vms/{id}/console", s.handleVMConsole)
	s.mux.HandleFunc("GET /images", img(s.handleImagesCached))
	s.mux.HandleFunc("GET /images/catalog", img(s.handleImagesCatalog))
	s.mux.HandleFunc("POST /images/pull", img(s.handleImagesPull))
	s.mux.HandleFunc("GET /images/{id}/status", img(s.handleImagesPullStatus))
	s.mux.HandleFunc("DELETE /images/{id}/pull", img(s.handleImagesPullCancel))
	s.mux.HandleFunc("DELETE /images/{id}", img(s.handleImagesDelete))
	s.mux.HandleFunc("GET /templates", tmpl(s.handleTemplateList))
	s.mux.HandleFunc("POST /templates", tmpl(s.handleTemplateCreate))
	s.mux.HandleFunc("DELETE /templates/{name}", tmpl(s.handleTemplateDelete))
	// "catalog" is a literal, so it wins over {name}.
	s.mux.HandleFunc("GET /services/catalog", s.handleServiceCatalog)
	s.mux.HandleFunc("GET /services", svc(s.handleServiceList))
	s.mux.HandleFunc("POST /services", svc(s.handleServiceCreate))
	s.mux.HandleFunc("GET /services/{name}", svc(s.handleServiceGet))
	s.mux.HandleFunc("DELETE /services/{name}", svc(s.handleServiceDelete))
	s.mux.HandleFunc("POST /services/{name}/start", svc(s.handleServiceStart))
	s.mux.HandleFunc("POST /services/{name}/stop", svc(s.handleServiceStop))
	s.mux.HandleFunc("POST /services/{name}/share", svc(s.handleServiceShare))
	s.mux.HandleFunc("POST /services/{name}/retry", svc(s.handleServiceRetry))
	s.mux.HandleFunc("GET /services/{name}/logs/{vm}", svc(s.handleServiceLog))
	s.mux.HandleFunc("GET /services/{name}/cluster", svc(s.handleServiceCluster))
	s.mux.HandleFunc("GET /services/{name}/monitoring", svc(s.handleServiceMonitoring))
	s.mux.HandleFunc("POST /services/{name}/workers", svc(s.handleServiceAddWorkers))
	s.mux.HandleFunc("DELETE /services/{name}/workers/{vm}", svc(s.handleServiceRemoveWorker))
	s.mux.HandleFunc("POST /services/{name}/apply", svc(s.handleServiceApply))
	s.mux.HandleFunc("POST /services/{name}/published", svc(s.handleServicePublish))
	s.mux.HandleFunc("DELETE /services/{name}/published/{port}", svc(s.handleServiceUnpublish))
	s.mux.HandleFunc("GET /services/{name}/files/{file}", svc(s.handleServiceFile))
	s.mux.HandleFunc("GET /network/interfaces", nw(s.handleNetworkInterfaces))
	s.mux.HandleFunc("GET /network/leases", nw(s.handleNetworkLeases))
	s.mux.HandleFunc("GET /network/switch", s.handleNetworkSwitch)
	s.mux.HandleFunc("GET /vms/{id}/snapshots", s.handleSnapshotList)
	s.mux.HandleFunc("POST /vms/{id}/snapshots", s.handleSnapshotCreate)
	s.mux.HandleFunc("DELETE /vms/{id}/snapshots/{snap}", s.handleSnapshotDelete)
	s.mux.HandleFunc("POST /vms/{id}/snapshots/{snap}/restore", s.handleSnapshotRestore)
	s.mux.Handle("/", s.spaHandler())
}

// requires serves a 503 carrying msg in place of h when a dependency is
// missing. Deps are fixed once New returns, so this is decided at routing.
func requires(present bool, msg string, h http.HandlerFunc) http.HandlerFunc {
	if present {
		return h
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, msg, http.StatusServiceUnavailable)
	}
}

// writeVMManagerError maps a backend error to a status: 404 for a VM that does
// not exist, 503 while libvirt is unreachable, 501 for an unsupported
// operation, 500 otherwise.
func writeVMManagerError(w http.ResponseWriter, action string, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, hypervisor.ErrVMNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, vm.ErrNotConnected) {
		status = http.StatusServiceUnavailable
	}
	if errors.Is(err, hypervisor.ErrUnsupported) {
		status = http.StatusNotImplemented
	}
	http.Error(w, fmt.Sprintf("%s: %v", action, err), status)
}

// The version is here, not only on GET /system, because `nanocloud update`
// confirms the new daemon is the one answering, and /health needs no login.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok", "version": version.Version})
}

// handleUpdateStatus handles GET /system/update: whether a newer release exists.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil || !s.updateCheckEnabled() {
		writeJSON(w, update.Status{Current: version.Version, Disabled: true})
		return
	}
	writeJSON(w, s.updates.Status(r.Context()))
}

// systemResponse is the JSON shape returned by GET /system.
type systemResponse struct {
	Hostname string `json:"hostname"`
	// Version is the release the running daemon was built from, which can
	// differ from the binary on disk until the service restarts.
	Version       string  `json:"version"`
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsedMB  uint64  `json:"memory_used_mb"`
	MemoryTotalMB uint64  `json:"memory_total_mb"`
	// CPUs is the host's CPU count from libvirt, zero when libvirt is
	// unreachable. The UI needs it to check a cpuset.
	CPUs int `json:"cpus"`
	// Backend and Capabilities say which hypervisor runs this daemon's VMs
	// and what it can do, so the UI hides what would only fail.
	Backend      string                  `json:"backend"`
	Capabilities hypervisor.Capabilities `json:"capabilities"`
}

// handleSystem handles GET /system. CPU and memory come from the stats
// sampler's latest tick, and read zero without one.
func (s *Server) handleSystem(w http.ResponseWriter, _ *http.Request) {
	resp := systemResponse{Hostname: "unknown", Version: version.Version, CPUs: s.vmManager.HostCPUs()}
	resp.Capabilities = s.capabilities()
	resp.Backend = resp.Capabilities.Backend
	if s.sysReader != nil {
		resp.Hostname = s.sysReader.Hostname()
	}
	if sample, ok := latestSample(s.statsSampler.Host(0)); ok {
		resp.CPUPercent = float64(sample.CPUPercent)
		resp.MemoryUsedMB = uint64(sample.MemUsedMB)
		resp.MemoryTotalMB = uint64(sample.MemTotalMB)
	}
	writeJSON(w, resp)
}

// latestSample returns the newest of a chronologically ordered slice.
func latestSample(samples []stats.Sample) (stats.Sample, bool) {
	if len(samples) == 0 {
		return stats.Sample{}, false
	}
	return samples[len(samples)-1], true
}

// poolResponse is the JSON shape returned by GET /storage/pools.
type poolResponse struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Default    bool   `json:"default"`
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

// handleStoragePools handles GET /storage/pools. A pool whose path cannot be
// read, such as an unmounted disk, is listed with zero bytes.
func (s *Server) handleStoragePools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	all := s.storagePools()
	pools := make([]poolResponse, 0, len(all))
	for _, p := range all {
		free, total, _ := storage.FreeSpace(p.Path) // errors surfaced as 0 values
		pools = append(pools, poolResponse{
			Name:       p.Name,
			Path:       p.Path,
			Default:    p.Default,
			FreeBytes:  free,
			TotalBytes: total,
		})
	}
	writeJSON(w, pools)
}

// vmJSON is one VM in GET /vms: the libvirt domain plus its metadata. The
// metadata fields are empty, never omitted, when a VM has no entry.
type vmJSON struct {
	// ID is the VM name, used as the path parameter in all VM API calls.
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	CPUCount uint16 `json:"cpu_count"`
	MemoryMB uint64 `json:"memory_mb"`
	// Fields from metadata.Store.
	PoolPath    string `json:"pool_path"`
	NetworkMode string `json:"network_mode"`
	// Bridge and VLANID come from networkOf, as in the spec response.
	Bridge string `json:"bridge"`
	VLANID int    `json:"vlan_id"`
	// MAC comes from metadata, since a bridged VM has no local lease to read.
	MAC string `json:"mac"`
	// OS is the distro tag, which picks the UI's icon.
	OS string `json:"os"`
	// IP is resolved live; empty when the VM is off or has none yet.
	IP string `json:"ip"`
	// Username is the cloud-init login user, from metadata. Empty when unknown.
	Username string `json:"username"`
	// Phase is vmPhase's readiness signal.
	Phase string `json:"phase"`
	// UptimeSeconds is zero when stopped or unknown.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// Service is the service this VM belongs to, if any.
	Service string `json:"service,omitempty"`
}

// noIPGracePeriod is how long a running VM may lack an IP and still read as
// "booting". It covers a slow first boot on modest hardware.
const noIPGracePeriod = 3 * time.Minute

// vmPhase derives a readiness phase from the domain status, IP and uptime.
// "ready" needs an address the guest itself confirmed (see vm.PrimaryIP),
// since a lease-file address outlives the guest that held it. Past
// noIPGracePeriod, a running VM without one is "no-network", not "booting".
//
// health only overrides the not-running branch: it is a separate snapshot
// from status, so a VM restarted moments ago can read stale for one tick.
func vmPhase(status, ip string, ipConfirmed bool, uptimeSeconds int64, health reconcile.VMHealth) string {
	if status != "running" {
		if health.GaveUp {
			return "gave-up"
		}
		if health.Attempts > 0 {
			return "crash-looping"
		}
		return "stopped"
	}
	if ip != "" && ipConfirmed {
		return "ready"
	}
	if uptimeSeconds > int64(noIPGracePeriod.Seconds()) {
		return "no-network"
	}
	return "booting"
}

// handleVMs handles GET /vms. Each domain is joined with its metadata entry;
// a missing entry or unreadable store leaves those fields empty.
func (s *Server) handleVMs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	vms, err := s.vmManager.List()
	if err != nil {
		writeVMManagerError(w, "list VMs", err)
		return
	}

	var metaEntries map[string]metadata.VMEntry
	if s.metaStore != nil {
		metaEntries, _ = s.metaStore.Read() // errors degrade gracefully
	}
	// nil-safe: indexing a nil map returns the zero VMHealth ("healthy").
	var health map[string]reconcile.VMHealth
	if s.reconcileLoop != nil {
		health = s.reconcileLoop.States()
	}

	out := make([]vmJSON, 0, len(vms))
	for _, v := range vms {
		j := vmJSON{
			ID:       v.Name, // name is the path param used in all VM API calls
			Name:     v.Name,
			Status:   v.State,
			CPUCount: v.VCPUs,
			MemoryMB: v.MemoryKB / 1024,
		}
		var entry metadata.VMEntry
		if e, ok := metaEntries[v.Name]; ok {
			entry = e
			j.PoolPath = entry.PoolPath
			j.NetworkMode = entry.NetworkMode
			netw := networkOf(entry)
			j.Bridge, j.VLANID = netw.Bridge, netw.VLANID
			j.MAC = entry.MAC
			j.OS = entry.OS
			j.Username = entry.CloudInitUser
			j.Service = entry.Service
		}
		var ipConfirmed bool
		if v.State == "running" {
			j.IP, ipConfirmed = s.vmManager.PrimaryIP(v.Name)
		}
		// Uptime must be set before vmPhase runs, or every VM without an IP
		// reads as no-network the moment it starts.
		if j.Status == "running" && entry.StartedAtUnix > 0 {
			j.UptimeSeconds = time.Now().Unix() - entry.StartedAtUnix
		}
		j.Phase = vmPhase(j.Status, j.IP, ipConfirmed, j.UptimeSeconds, health[v.Name])
		out = append(out, j)
	}
	writeJSON(w, out)
}

// vmCreateRequest is the JSON body for POST /vms.
type vmCreateRequest struct {
	Name        string          `json:"name"`
	ImageID     string          `json:"image_id"`
	VCPUs       int             `json:"vcpus"`
	MemoryMB    int             `json:"memory_mb"`
	DiskGB      int             `json:"disk_gb"`
	PoolName    string          `json:"pool_name"`
	NetworkMode string          `json:"network_mode"`
	CloudInit   cloudInitParams `json:"cloud_init"`
	// Firmware is "uefi", or empty/"bios" for BIOS (UEFI is opt-in).
	Firmware   string `json:"firmware"`
	SecureBoot bool   `json:"secure_boot"`
	TPM        bool   `json:"tpm"`
	// CPU's zero value is the default: host-model, no topology, unpinned.
	CPU cpuShapeBody `json:"cpu"`
	// Network is the guest's networking; its zero value is NAT. NetworkMode
	// above is the legacy alias for Network.Mode.
	Network networkBody `json:"network"`
	// PCIDevices are host PCI addresses ("0000:01:00.0") and USBDevices
	// "vendor:product" pairs, both checked against the host inventory.
	PCIDevices []string `json:"pci_devices"`
	USBDevices []string `json:"usb_devices"`
	// Service is set by the services manager on the VMs it creates.
	Service string `json:"service"`
}

// validateFirmware rejects firmware combinations that libvirt refuses, or
// that define cleanly and then never boot.
func validateFirmware(req vmCreateRequest, distro string) error {
	switch req.Firmware {
	case "", metadata.FirmwareBIOS, metadata.FirmwareUEFI:
	default:
		return fmt.Errorf(`"firmware" must be %q or %q`, metadata.FirmwareBIOS, metadata.FirmwareUEFI)
	}

	if req.SecureBoot && req.Firmware != metadata.FirmwareUEFI {
		// Inferring UEFI here would change the guest's whole boot path off a
		// checkbox; ignoring it would claim a protection the VM does not have.
		return errors.New(`"secure_boot" requires "firmware":"uefi" — secure boot is a UEFI feature`)
	}

	// Alpine ships no signed shim, so a secure-boot domain defines fine and
	// then halts in the firmware with nothing logged.
	if req.SecureBoot && distro == distroAlpine {
		return errors.New("this image runs Alpine, which does not ship a signed bootloader, so it cannot boot with secure boot enabled — use UEFI without secure boot, or pick another image")
	}
	return nil
}

// cloudInitParams is the cloud_init object in the POST /vms body. It is
// create-time only, since cloud-init applies user-data on first boot.
type cloudInitParams struct {
	Hostname     string `json:"hostname"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	SSHPublicKey string `json:"ssh_public_key"`
	// GitHubUser imports that account's published keys into the seed.
	GitHubUser string `json:"github_user"`
	// Packages installed, commands run and files written on first boot.
	Packages   []string             `json:"packages"`
	RunCmd     []string             `json:"runcmd"`
	WriteFiles []metadata.WriteFile `json:"write_files"`
}

// resolveSSHKeys merges a pasted key and an imported GitHub account's keys
// into one list. A failed import fails the create.
func resolveSSHKeys(ctx context.Context, ci cloudInitParams) ([]string, error) {
	var keys []string
	if k := strings.TrimSpace(ci.SSHPublicKey); k != "" {
		keys = append(keys, k)
	}
	if user := strings.TrimSpace(ci.GitHubUser); user != "" {
		imported, err := cloudinit.FetchGitHubKeys(ctx, user)
		if err != nil {
			return nil, err
		}
		keys = append(keys, imported...)
	}
	return keys, nil
}

// handleGitHubKeys handles GET /cloud-init/github-keys?user=<login>, so the
// wizard can show which keys an import would install before the VM exists.
func (s *Server) handleGitHubKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := cloudinit.FetchGitHubKeys(r.Context(), r.URL.Query().Get("user"))
	if err != nil {
		// 422, not 502: no such account, or no keys on it, is about the request.
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, struct {
		Keys []string `json:"keys"`
	}{Keys: keys})
}

// loginShellFor returns the login shell cloud-init should set: bash, except
// on Alpine, which has only busybox /bin/sh. Left unset, useradd picks
// /bin/sh, which on Debian is dash, without history or line editing.
func loginShellFor(distro string) string {
	if distro == distroAlpine {
		return ""
	}
	return "/bin/bash"
}

// guestAgentFor returns how a distro's seed starts qemu-guest-agent, which
// confirms a bridged guest's IP (see vm.PrimaryIP). Only the libvirt backend
// renders the agent channel.
func (s *Server) guestAgentFor(distro string) cloudinit.GuestAgent {
	if s.capabilities().Backend != hypervisor.BackendLibvirt {
		return ""
	}
	if distro == distroAlpine {
		return cloudinit.GuestAgentOpenRC
	}
	return cloudinit.GuestAgentSystemd
}

// adminGroupsFor returns the supplementary groups that give a distro's login
// user root. The seed's `sudo:` rule does that everywhere sudo exists; Alpine
// ships doas instead, and its cloud image grants `permit nopass :wheel` in
// /etc/doas.d/wheel.conf, so wheel there is the same passwordless root.
func adminGroupsFor(distro string) []string {
	if distro == distroAlpine {
		return []string{"wheel"}
	}
	return nil
}

// distroAlpine is the catalog tag for the one image that is the exception to
// two unrelated rules: it has no bash, and no signed bootloader.
const distroAlpine = "alpine"

// vmNamePattern keeps a VM name safe as a path component. The disk and seed
// ISO are written under the raw name before libvirt sees it, so libvirt's own
// validation cannot stop a name like "../../etc/foo" escaping the pool.
var vmNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

const vmNameRule = `"name" must start with a letter or digit and contain only letters, digits, dots, dashes, and underscores (max 63 characters)`

// handleVMCreate handles POST /vms. Files it creates before a failure are
// removed.
func (s *Server) handleVMCreate(w http.ResponseWriter, r *http.Request) {
	var req vmCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, `"name" is required`, http.StatusBadRequest)
		return
	}
	// An if, not a switch case: code scanning only sees an if as guarding the
	// disk and seed paths built from the name below.
	if !vmNamePattern.MatchString(req.Name) {
		http.Error(w, vmNameRule, http.StatusBadRequest)
		return
	}

	switch {
	case strings.TrimSpace(req.ImageID) == "":
		http.Error(w, `"image_id" is required`, http.StatusBadRequest)
		return
	case req.VCPUs <= 0:
		http.Error(w, `"vcpus" must be > 0`, http.StatusBadRequest)
		return
	case req.MemoryMB <= 0:
		http.Error(w, `"memory_mb" must be > 0`, http.StatusBadRequest)
		return
	case req.DiskGB <= 0:
		http.Error(w, `"disk_gb" must be > 0`, http.StatusBadRequest)
		return
	}

	// Refuse what this backend cannot do before anything is written.
	if err := s.unsupportedFeature(featureAsk{
		SecureBoot:  req.SecureBoot,
		TPM:         req.TPM,
		CPUTuned:    cpuTuned(req.CPU),
		Devices:     len(req.PCIDevices) + len(req.USBDevices),
		NetworkMode: networkModeOf(req),
		VLANID:      req.Network.VLANID,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	// A taken name must be refused before anything is written: qemu-img would
	// truncate the existing disk and DomainDefineXML redefine the live domain.
	if exists, existsErr := s.vmManager.Exists(req.Name); existsErr != nil {
		writeVMManagerError(w, fmt.Sprintf("check vm %q", req.Name), existsErr)
		return
	} else if exists {
		http.Error(w, fmt.Sprintf("a VM named %q already exists", req.Name), http.StatusConflict)
		return
	}

	catalogEntry, ok := image.EntryByID(req.ImageID)
	if !ok {
		http.Error(w, fmt.Sprintf("unknown image_id %q", req.ImageID), http.StatusBadRequest)
		return
	}

	// Validated here because the Alpine check needs the catalog entry.
	if err := validateFirmware(req, catalogEntry.Distro); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Store "bios" as empty, so only non-default VMs carry the key.
	firmware := req.Firmware
	if firmware == metadata.FirmwareBIOS {
		firmware = ""
	}

	cpu := req.CPU.normalise()
	candidate := metadata.VMEntry{ID: req.Name, VCPUs: req.VCPUs}
	cpu.applyTo(&candidate)
	if status, cpuErr := validateCPUShape(candidate, s.vmManager.HostCPUs()); cpuErr != nil {
		http.Error(w, cpuErr.Error(), status)
		return
	}

	// Resolved before anything is written, so an unusable device costs nothing.
	var pciDevices []metadata.PCIDevice
	var usbDevices []metadata.USBDevice
	var deviceWarnings []string
	if len(req.PCIDevices) > 0 || len(req.USBDevices) > 0 {
		report, repErr := s.deviceReport()
		if repErr != nil {
			writeVMManagerError(w, "list host devices", repErr)
			return
		}
		resolved, resErr := hostdev.Resolve(report, hostdev.Selection{PCI: req.PCIDevices, USB: req.USBDevices})
		if resErr != nil {
			http.Error(w, resErr.Error(), http.StatusUnprocessableEntity)
			return
		}
		pciDevices, usbDevices = specDevicesOf(resolved)
		deviceWarnings = resolved.Warnings
	}

	if s.imgManager == nil {
		http.Error(w, "image manager unavailable", http.StatusServiceUnavailable)
		return
	}
	imagePath := s.imgManager.CachedPath(catalogEntry.Filename)

	// Otherwise qemu-img fails later with "Could not open backing image".
	if _, err := os.Stat(imagePath); err != nil {
		http.Error(w, fmt.Sprintf("image %q is not downloaded yet — pull it before creating a VM", catalogEntry.Name), http.StatusUnprocessableEntity)
		return
	}

	pool, err := s.resolvePool(req.PoolName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The network group wins over the legacy top-level network_mode.
	netw := req.Network
	if netw.Mode == "" {
		netw.Mode = req.NetworkMode
	}
	netw = netw.normalise()

	// Ahead of every file write below, so a failed provision leaves nothing.
	if status, provErr := s.provisionVLAN(&netw); provErr != nil {
		http.Error(w, provErr.Error(), status)
		return
	}

	netw.applyTo(&candidate)
	if status, netErr := validateNetwork(candidate, s.hostBridges()); netErr != nil {
		http.Error(w, netErr.Error(), status)
		return
	}

	diskPath := storage.DiskPathFor(pool.Path, req.Name)
	isoPath := storage.ISOPathFor(pool.Path, req.Name)

	// A disk left over from an earlier create is refused, not truncated.
	if _, statErr := os.Stat(diskPath); statErr == nil {
		http.Error(w, fmt.Sprintf("a disk for %q already exists at %s — remove it before reusing the name", req.Name, diskPath), http.StatusConflict)
		return
	}

	// Before any file is written, so a failed import leaves nothing behind.
	sshKeys, err := resolveSSHKeys(r.Context(), req.CloudInit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	ciParams := cloudinit.Params{
		Hostname:   req.CloudInit.Hostname,
		Username:   req.CloudInit.Username,
		Password:   req.CloudInit.Password,
		SSHKeys:    sshKeys,
		Shell:      loginShellFor(catalogEntry.Distro),
		GuestAgent: s.guestAgentFor(catalogEntry.Distro),
		Groups:     adminGroupsFor(catalogEntry.Distro),
		Packages:   req.CloudInit.Packages,
		RunCmd:     req.CloudInit.RunCmd,
		WriteFiles: req.CloudInit.WriteFiles,
	}
	if ciParams.Hostname == "" {
		ciParams.Hostname = req.Name
	}

	// The seed goes first, so a failure here has no disk to clean up.
	if err := cloudinit.Build(ciParams, isoPath); err != nil {
		msg := fmt.Sprintf("cloud-init build: %v", err)
		if pErr := cloudinit.Preflight(); pErr != nil {
			msg = "xorriso is not installed on this host. Run: sudo apt install xorriso (Debian/Ubuntu) or sudo dnf install xorriso (Fedora/RHEL)"
		}
		http.Error(w, msg, http.StatusUnprocessableEntity)
		return
	}

	if err := storage.AllocateDisk(imagePath, diskPath, req.DiskGB); err != nil {
		os.Remove(isoPath) //nolint:errcheck
		http.Error(w, fmt.Sprintf("allocate disk: %v", err), http.StatusInternalServerError)
		return
	}

	createParams := vm.VMCreateParams{
		Name:         req.Name,
		ImagePath:    imagePath,
		DiskPath:     diskPath,
		CloudInitISO: isoPath,
		VCPUs:        req.VCPUs,
		MemoryMB:     req.MemoryMB,
		NetworkMode:  netw.Mode,
		Bridge:       netw.Bridge,
		VLANID:       netw.VLANID,
		VLANUplink:   netw.VLANUplink,
		Firmware:     firmware,
		SecureBoot:   req.SecureBoot,
		TPM:          req.TPM,
		CPUMode:      cpu.Mode,
		CPUTopology:  cpu.Topology,
		CPUSet:       cpu.CPUSet,
		PCIDevices:   pciDevices,
		USBDevices:   usbDevices,
	}
	if err := s.vmManager.Create(createParams); err != nil {
		os.Remove(isoPath)           //nolint:errcheck
		storage.DeleteDisk(diskPath) //nolint:errcheck

		if errors.Is(err, vm.ErrNotConnected) {
			writeVMManagerError(w, "create vm", err)
			return
		}
		msg := fmt.Sprintf("create vm: %v", err)
		if pErr := install.NetworkPreflight(); pErr != nil {
			msg = pErr.Error()
		}
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	// Persist the spec. A failure rolls the create back: a domain with no
	// entry looks like a successful create until the next reboot loses it.
	if s.metaStore != nil {
		// Upsert, not Update: this is the one path that creates an entry.
		if metaErr := s.metaStore.Upsert(req.Name, func(e *metadata.VMEntry) {
			e.ID = req.Name
			e.PoolPath = pool.Path
			netw.applyTo(e)
			e.CloudInitUser = ciParams.Username
			e.Hostname = ciParams.Hostname
			e.ImageID = catalogEntry.ID
			e.OS = catalogEntry.Distro
			e.VCPUs = req.VCPUs
			e.MemoryMB = req.MemoryMB
			e.DiskGB = req.DiskGB
			e.Firmware = firmware
			e.SecureBoot = req.SecureBoot
			e.TPM = req.TPM
			cpu.applyTo(e)
			e.CloudInit = cloudInitSpecOf(req.CloudInit, sshKeys)
			if strings.TrimSpace(req.Service) != "" {
				e.CloudInit = withoutProvisioning(e.CloudInit)
			}
			e.PCIDevices = pciDevices
			e.USBDevices = usbDevices
			e.Service = strings.TrimSpace(req.Service)
			// Explicit, so metadata.json reads plainly to someone editing it.
			e.RestartPolicy = metadata.DefaultRestartPolicy
			e.SetRunning(true)
			e.StartedAtUnix = time.Now().Unix()
		}); metaErr != nil {
			s.rollbackCreate(req.Name, diskPath, isoPath)
			http.Error(w, fmt.Sprintf("create vm %q: could not record its spec, so the VM was rolled back: %v", req.Name, metaErr), http.StatusInternalServerError)
			return
		}
	}

	// Device warnings ride along with the 201 rather than blocking it: passing
	// the host's boot GPU can be deliberate.
	writeJSONStatus(w, http.StatusCreated, struct {
		Name     string   `json:"name"`
		Warnings []string `json:"warnings,omitempty"`
	}{Name: req.Name, Warnings: deviceWarnings})
}

// rollbackCreate undoes a create whose spec could not be persisted, so no
// domain or file is left that no entry names. Every step is best-effort; the
// caller is already returning 500.
func (s *Server) rollbackCreate(name string, files ...string) {
	// Destroy before Delete: undefine refuses a running domain.
	_ = s.vmManager.Destroy(name)
	if err := s.vmManager.Delete(name); err != nil {
		log.Printf("api: rolling back create of %q: undefine failed, domain may be orphaned: %v", name, err)
	}
	for _, f := range files {
		os.Remove(f) //nolint:errcheck // best-effort rollback; the 500 is already going out
	}
}

// writeSpec applies fn to the VM's metadata entry. A missing entry, as for a
// domain defined with virsh, is not an error.
func (s *Server) writeSpec(id string, fn func(*metadata.VMEntry)) error {
	if s.metaStore == nil {
		return nil
	}
	if err := s.metaStore.Update(id, fn); err != nil && !errors.Is(err, metadata.ErrNotFound) {
		return err
	}
	return nil
}

// handleVMStart handles POST /vms/{id}/start. The spec is written first, so
// if the daemon dies before libvirt acts, the reconcile loop still starts it.
func (s *Server) handleVMStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if err := s.writeSpec(id, func(e *metadata.VMEntry) {
		e.SetRunning(true)
		e.StartedAtUnix = time.Now().Unix()
	}); err != nil {
		http.Error(w, fmt.Sprintf("start vm %q: could not record desired state: %v", id, err), http.StatusInternalServerError)
		return
	}

	if err := s.vmManager.Start(id); err != nil {
		// running stays true: a failed start does not retract the request.
		// The loop retries, and surfaces persistent failure as give-up.
		writeVMManagerError(w, fmt.Sprintf("start vm %q", id), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleVMStop handles POST /vms/{id}/stop. The default is an ACPI shutdown,
// which a guest without an ACPI daemon may ignore forever; ?force=true pulls
// the power instead. Escalation is left to the caller rather than a timer
// here, because the reconcile loop is the only owner of power state.
func (s *Server) handleVMStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"

	// Spec first on both paths: a crash in between otherwise leaves the loop
	// looking at running:true, and it undoes the stop.
	specErr := s.writeSpec(id, func(e *metadata.VMEntry) {
		e.SetRunning(false)
		if force {
			// Only force guarantees the domain is off; a guest may ignore
			// ACPI and keep running.
			e.StartedAtUnix = 0
		}
	})
	if specErr != nil {
		if !force {
			http.Error(w, fmt.Sprintf("stop vm %q: could not record desired state: %v", id, specErr), http.StatusInternalServerError)
			return
		}
		// Force fails open: the default pool shares a filesystem with
		// metadata.json, so a full disk must not stop force from killing the
		// VM that filled it. A stale running:true may restart it later.
		log.Printf("api: force stop %q: could not record desired state, proceeding with DomainDestroy anyway: %v", id, specErr)
	}

	stop, action := s.vmManager.Stop, "stop"
	if force {
		stop, action = s.vmManager.Destroy, "force stop"
	}
	if err := stop(id); err != nil && !errors.Is(err, vm.ErrNotRunning) {
		// running stays false: rolling it back would resurrect a VM the user
		// asked to stop, on the reconcile loop's next tick.
		writeVMManagerError(w, fmt.Sprintf("%s vm %q", action, id), err)
		return
	}
	// An already-stopped domain is the state the caller asked for.
	w.WriteHeader(http.StatusNoContent)
}

// handleVMDelete handles DELETE /vms/{id}: it force-stops and undefines the
// domain, removes its disks and seed, then drops its metadata entry. The
// entry goes last because a retry after a failed undefine needs its PoolPath
// to find the files.
func (s *Server) handleVMDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Mark it stopped first so the loop cannot revive it mid-delete. Like
	// force stop this fails open: removing the disks below frees the space
	// that most likely broke the write.
	if err := s.writeSpec(id, func(e *metadata.VMEntry) {
		e.SetRunning(false)
	}); err != nil {
		log.Printf("api: delete %q: could not record desired state, proceeding anyway: %v", id, err)
	}

	// The domain may already be off.
	_ = s.vmManager.Destroy(id)

	// Read metadata before undefining so we can locate every file this VM owns.
	var entry metadata.VMEntry
	if s.metaStore != nil {
		if entries, err := s.metaStore.Read(); err == nil {
			entry = entries[id]
		}
	}

	if err := s.vmManager.Delete(id); err != nil {
		writeVMManagerError(w, fmt.Sprintf("delete vm %q", id), err)
		return
	}

	// Every disk in the spec, not just the boot overlay.
	if entry.PoolPath != "" {
		for _, d := range disksOf(entry) {
			storage.DeleteDisk(d.Path) //nolint:errcheck // best-effort; the domain is already gone
		}
		os.Remove(storage.ISOPathFor(entry.PoolPath, id)) //nolint:errcheck
	}

	// Drop the spec last, once the domain and its files are actually gone.
	if s.metaStore != nil {
		if err := s.metaStore.Delete(id); err != nil {
			http.Error(w, fmt.Sprintf("delete vm %q: undefined and cleaned up, but could not remove its spec: %v", id, err), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// vmStatsResponse is the JSON shape returned by GET /vms/{id}/stats.
type vmStatsResponse struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryUsedMB  uint64  `json:"memory_used_mb"`
	MemoryTotalMB uint64  `json:"memory_total_mb"`
}

// handleVMStats handles GET /vms/{id}/stats from the latest sample.
func (s *Server) handleVMStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sample, ok := latestSample(s.statsSampler.VM(id, 0))
	if !ok {
		http.Error(w, fmt.Sprintf("no stats yet for vm %q", id), http.StatusNotFound)
		return
	}
	writeJSON(w, vmStatsResponse{
		CPUPercent:    float64(sample.CPUPercent),
		MemoryUsedMB:  uint64(sample.MemUsedMB),
		MemoryTotalMB: uint64(sample.MemTotalMB),
	})
}

// handleStatsHistory handles GET /stats/history?vm=<name>&since=<unix>: the
// host series, or one VM's when vm is set. A missing or invalid since returns
// the whole retained window.
func (s *Server) handleStatsHistory(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	var samples []stats.Sample
	if name := r.URL.Query().Get("vm"); name != "" {
		samples = s.statsSampler.VM(name, since)
	} else {
		samples = s.statsSampler.Host(since)
	}
	// Never null, since the UI maps over it. VM returns nil for a VM created
	// since the last tick.
	if samples == nil {
		samples = []stats.Sample{}
	}

	writeJSON(w, struct {
		Samples []stats.Sample `json:"samples"`
	}{samples})
}

// resolvePool returns the pool called name. An empty name selects the default
// pool, or the first one when none is marked default.
func (s *Server) resolvePool(name string) (config.StoragePool, error) {
	pools := s.storagePools()
	if len(pools) == 0 {
		return config.StoragePool{}, fmt.Errorf("no storage pools configured")
	}
	if name == "" {
		for _, p := range pools {
			if p.Default {
				return p, nil
			}
		}
		return pools[0], nil
	}
	for _, p := range pools {
		if p.Name == name {
			return p, nil
		}
	}
	return config.StoragePool{}, fmt.Errorf("storage pool %q not found", name)
}

func (s *Server) handleImagesCached(w http.ResponseWriter, _ *http.Request) {
	imgs, err := s.imgManager.Cached()
	if err != nil {
		http.Error(w, fmt.Sprintf("list cached images: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, imgs)
}

func (s *Server) handleImagesCatalog(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.imgManager.Catalog())
}

func (s *Server) handleImagesPull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.ID == "" {
		http.Error(w, `"id" is required`, http.StatusBadRequest)
		return
	}
	if err := s.imgManager.Pull(body.ID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleImagesPullStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	progress, done, err := s.imgManager.PullStatus(id)
	errStr := ""
	if err != nil {
		// Distinguish "no such pull" (404) from a pull error (200 with error field).
		if progress == 0 && !done {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		errStr = err.Error()
	}
	writeJSON(w, map[string]any{
		"id":       id,
		"progress": progress,
		"done":     done,
		"error":    errStr,
	})
}

// handleImagesPullCancel handles DELETE /images/{id}/pull. It answers 204
// whether or not a pull was running, and 404 only for an unknown catalog ID.
func (s *Server) handleImagesPullCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.imgManager.CancelPull(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleImagesDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	users, err := s.imageUsers(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(users) > 0 {
		http.Error(w, fmt.Sprintf("image %s is the base disk of %s: delete those VMs first", id, strings.Join(users, ", ")), http.StatusConflict)
		return
	}
	if err := s.imgManager.Remove(id); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// imageUsers names the VMs whose boot disk is an overlay on image id, which
// would become unreadable without it. A clone is not a user: its disk was
// flattened when it was made.
func (s *Server) imageUsers(id string) ([]string, error) {
	entry, ok := image.EntryByID(id)
	if !ok || s.metaStore == nil {
		return nil, nil // Remove reports an unknown ID itself
	}
	base, err := filepath.Abs(s.imgManager.CachedPath(entry.Filename))
	if err != nil {
		return nil, err
	}
	specs, err := s.metaStore.Read()
	if err != nil {
		return nil, fmt.Errorf("read VM specs: %w", err)
	}
	var users []string
	for name, e := range specs {
		if e.ImageID != "" && e.ImageID != id {
			continue
		}
		disk, _, ok := storage.PathsFor(e.PoolPath, name)
		if !ok {
			continue
		}
		backing, err := storage.BackingFile(disk)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		// Fail closed: a disk that cannot be read might still depend on it.
		if err != nil {
			return nil, fmt.Errorf("check whether %s uses image %s: %w", name, id, err)
		}
		if backing == base {
			users = append(users, name)
		}
	}
	sort.Strings(users)
	return users, nil
}

// networkInterfacesResponse is the JSON shape returned by GET /network/interfaces.
type networkInterfacesResponse struct {
	Bridges    []network.Bridge        `json:"bridges"`
	Interfaces []network.HostInterface `json:"interfaces"`
	// Uplink is the host's default-route interface, which a VLAN would ride.
	// When empty, the wizard offers no VLAN.
	Uplink string `json:"uplink"`
}

// handleNetworkInterfaces handles GET /network/interfaces.
func (s *Server) handleNetworkInterfaces(w http.ResponseWriter, _ *http.Request) {
	ifaces, err := s.netManager.Interfaces()
	if err != nil {
		http.Error(w, fmt.Sprintf("list interfaces: %v", err), http.StatusInternalServerError)
		return
	}
	bridges, err := s.netManager.Bridges()
	if err != nil {
		http.Error(w, fmt.Sprintf("list bridges: %v", err), http.StatusInternalServerError)
		return
	}
	// Only bridges with a physical port reach the picker.
	bridges = network.WithUplink(bridges)

	// No hostNet or an unreadable routing table means no VLAN offer, not a
	// failed request.
	var uplink string
	if s.hostNet != nil {
		if u, routeErr := s.hostNet.DefaultRouteInterface(); routeErr == nil {
			uplink = u
		}
	}

	writeJSON(w, networkInterfacesResponse{Bridges: bridges, Interfaces: ifaces, Uplink: uplink})
}

// handleNetworkLeases handles GET /network/leases: the DHCP leases from
// libvirt's dnsmasq status file.
func (s *Server) handleNetworkLeases(w http.ResponseWriter, _ *http.Request) {
	leases, err := s.netManager.Leases()
	if err != nil {
		http.Error(w, fmt.Sprintf("read leases: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, leases)
}

// snapshotCreateRequest is the JSON body for POST /vms/{id}/snapshots.
type snapshotCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleSnapshotList handles GET /vms/{id}/snapshots.
func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snaps, err := s.snapshots().ListSnapshots(id)
	if err != nil {
		writeVMManagerError(w, fmt.Sprintf("list snapshots for %q", id), err)
		return
	}
	writeJSON(w, snaps)
}

// handleSnapshotCreate handles POST /vms/{id}/snapshots.
func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req snapshotCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, `"name" is required`, http.StatusBadRequest)
		return
	}

	if err := s.snapshots().CreateSnapshot(id, req.Name, req.Description); err != nil {
		writeVMManagerError(w, fmt.Sprintf("create snapshot %q on %q", req.Name, id), err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// handleSnapshotDelete handles DELETE /vms/{id}/snapshots/{snap}.
func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap := r.PathValue("snap")

	if err := s.snapshots().DeleteSnapshot(id, snap); err != nil {
		writeVMManagerError(w, fmt.Sprintf("delete snapshot %q on %q", snap, id), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSnapshotRestore handles POST /vms/{id}/snapshots/{snap}/restore.
func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	snap := r.PathValue("snap")

	if err := s.snapshots().RestoreSnapshot(id, snap); err != nil {
		writeVMManagerError(w, fmt.Sprintf("restore snapshot %q on %q", snap, id), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// spaHandler serves the embedded UI, falling back to index.html for the
// client-side routes.
func (s *Server) spaHandler() http.Handler {
	fileServer := http.FileServer(http.FS(s.webFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := s.webFS.Open(path); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// withoutProvisioning drops the files and commands from a service VM's
// recorded cloud-init. They carry the service's generated secrets, which
// belong in root-only services.json, not in metadata.json or in a template
// saved from the VM.
func withoutProvisioning(spec *metadata.CloudInitSpec) *metadata.CloudInitSpec {
	if spec == nil {
		return nil
	}
	out := *spec
	out.WriteFiles, out.RunCmd = nil, nil
	if len(out.Packages) == 0 && len(out.SSHKeys) == 0 {
		return nil
	}
	return &out
}

// cloudInitSpecOf records what a VM was provisioned with, or nil when that was
// nothing beyond a user, so metadata.json gets no empty "cloud_init": {}.
func cloudInitSpecOf(ci cloudInitParams, sshKeys []string) *metadata.CloudInitSpec {
	spec := metadata.CloudInitSpec{
		Packages:   ci.Packages,
		RunCmd:     ci.RunCmd,
		WriteFiles: ci.WriteFiles,
		SSHKeys:    sshKeys,
	}
	if len(spec.Packages) == 0 && len(spec.RunCmd) == 0 && len(spec.WriteFiles) == 0 && len(spec.SSHKeys) == 0 {
		return nil
	}
	return &spec
}
