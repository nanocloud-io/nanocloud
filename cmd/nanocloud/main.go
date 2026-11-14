package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	nanocloud "github.com/nanocloud-io/nanocloud"
	"github.com/nanocloud-io/nanocloud/internal/api"
	"github.com/nanocloud-io/nanocloud/internal/auth"
	"github.com/nanocloud-io/nanocloud/internal/cloudinit"
	"github.com/nanocloud-io/nanocloud/internal/config"
	"github.com/nanocloud-io/nanocloud/internal/events"
	"github.com/nanocloud-io/nanocloud/internal/hypervisor"
	"github.com/nanocloud-io/nanocloud/internal/install"
	"github.com/nanocloud-io/nanocloud/internal/metadata"
	"github.com/nanocloud-io/nanocloud/internal/reconcile"
	"github.com/nanocloud-io/nanocloud/internal/services"
	"github.com/nanocloud-io/nanocloud/internal/settings"
	"github.com/nanocloud-io/nanocloud/internal/stats"
	"github.com/nanocloud-io/nanocloud/internal/storage"
	"github.com/nanocloud-io/nanocloud/internal/system"
	"github.com/nanocloud-io/nanocloud/internal/template"
	"github.com/nanocloud-io/nanocloud/internal/tlscert"
	"github.com/nanocloud-io/nanocloud/internal/update"
	"github.com/nanocloud-io/nanocloud/internal/version"
)

// defaultLibvirtURI is the production libvirt connection URI.
const defaultLibvirtURI = "qemu:///system"

// defaultMetadataPath and defaultImageCacheDir are defined per platform, in
// platform_libvirt.go and platform_vz.go.

// shutdownTimeout bounds how long graceful shutdown waits for in-flight
// requests, including open console WebSockets, before closing them. It stays
// under the unit's TimeoutStopSec=20 so it fires before systemd's SIGKILL.
const shutdownTimeout = 10 * time.Second

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		if err := cmdServe(os.Args[2:]); err != nil {
			log.Fatalf("serve: %v", err)
		}
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "passwd":
		cmdPasswd(os.Args[2:])
	case "update":
		cmdUpdate(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version.Version)
	default:
		if platformCommand(cmd) {
			return
		}
		fmt.Fprintf(os.Stderr, "nanocloud: unknown command %q\n", cmd)
		fmt.Fprintf(os.Stderr, "usage: nanocloud [serve|install|uninstall|update|doctor|passwd|version]\n")
		os.Exit(1)
	}
}

// cmdServe wires up every dependency and runs the HTTP server until it is
// asked to stop. Once the server runs it returns errors rather than calling
// log.Fatal, so the process exits only after a graceful shutdown; the
// log.Fatalf calls before that point have nothing to shut down.
func cmdServe(args []string) error {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fset.String("addr", ":8080", "listen address")
	cfgPath := fset.String("config", config.DefaultPath, "path to config.yaml")
	// With ExitOnError, Parse exits rather than returning an error.
	_ = fset.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.Port != 0 {
		*addr = fmt.Sprintf(":%d", cfg.Port)
	}

	webFS, err := fs.Sub(nanocloud.WebFS, "web/dist")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	libvirtURI := cfg.LibvirtURI
	if libvirtURI == "" {
		libvirtURI = defaultLibvirtURI
	}

	// VM creation needs xorriso, so warn early if it is missing.
	if preflightErr := cloudinit.Preflight(); preflightErr != nil {
		log.Printf("warning: %v — install xorriso before creating VMs", preflightErr)
	}

	// The libvirt backend dials on demand and reconnects on later calls, so a
	// libvirtd that is slow to start after a reboot is picked up on its own.
	vmManager := newBackend(cfg, libvirtURI)

	metadataPath := metadataPathFor(cfg)
	metaStore := metadata.New(metadataPath)

	// Migrate a legacy file, and refuse one written by a newer binary: the next
	// write would drop every field this build does not know.
	if err = metaStore.Migrate(); err != nil {
		if errors.Is(err, metadata.ErrSchemaTooNew) {
			log.Fatalf("metadata: %v\n\nThis binary is older than the one that wrote %s. "+
				"Re-install the newer release, or move that file aside to start fresh.",
				err, metadataPath)
		}
		// Any other failure only degrades the VM endpoints.
		log.Printf("warning: could not migrate %s: %v", metadataPath, err)
	}

	restrictVMFiles(metaStore, install.PrivateDisksUsable())

	// Only the VZ backend needs this; libvirt keeps its own definitions.
	adoptSpecs(vmManager, metaStore)

	// Templates are the same kind of state as metadata.json, so they live beside it.
	tmplStore := template.New(filepath.Join(filepath.Dir(metadataPath), "templates.json"))

	imageCacheDir := cfg.ImageCacheDir
	if imageCacheDir == "" {
		imageCacheDir = defaultImageCacheDir
	}
	imgManager := newImageManager(imageCacheDir)

	netManager := newNetManager()

	// hostNet is nil on a platform without VLAN support.
	hostNetManager := newHostNet()

	// sysReader is nil off Linux, which api.New handles.
	sysReader := system.New()

	// The reconcile loop appends to this ring too, so it is built here.
	eventRing := events.New(events.DefaultCapacity)

	sampler := stats.New(vmManager, sysReader)

	// Settings from the UI overlay config.yaml. A file from a newer binary is
	// refused, since a save would drop what this one does not know.
	settingsStore, err := settings.Open(settings.PathFor(metadataPath))
	if err != nil {
		log.Fatalf("%v\nRe-install the newer release, or move %s aside.", err, settings.PathFor(metadataPath))
	}

	// The loop is paused rather than absent when switched off, so Settings can
	// turn it back on without a restart.
	loop := reconcile.New(vmManager, metaStore, eventRing, hostNetManager)
	loop.SetEnabled(settingsStore.Get().SelfHealingEnabled(cfg))

	// Services keep their state and SSH key beside metadata.json. A key that
	// cannot be read or made disables services, not the daemon.
	stateDir := filepath.Dir(metadataPath)
	svcManager, svcErr := services.New(services.NewStore(filepath.Join(stateDir, "services.json")), eventRing,
		filepath.Join(stateDir, "service_ed25519"))
	if svcErr != nil {
		log.Printf("warning: services unavailable: %v", svcErr)
		svcManager = nil
	}

	authStore := auth.New(filepath.Join(stateDir, "auth.json"))
	if set, authErr := authStore.PasswordSet(); authErr != nil {
		log.Fatalf("auth: %v\n\nMove %s aside and run `sudo nanocloud passwd` to set a new password.", authErr, authStore.Path())
	} else if !set {
		log.Printf("warning: no password is set, so every request will be refused — run `sudo nanocloud passwd`")
	}

	tlsConfig, certPath, err := serverTLSConfig(cfg, stateDir)
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	fingerprint := ""
	if certPath != "" {
		fingerprint, _ = tlscert.Fingerprint(certPath) // the pair just loaded; it reads
	}

	srv := api.New(api.Deps{
		WebFS:      webFS,
		Cfg:        cfg,
		VMManager:  vmManager,
		MetaStore:  metaStore,
		ImgManager: imgManager,
		NetManager: netManager,
		SysReader:  sysReader,
		Events:     eventRing,
		Stats:      sampler,
		Reconcile:  loop,
		Templates:  tmplStore,
		HostNet:    hostNetManager,
		Services:   svcManager,
		Auth:       authStore,
		Updates:    update.NewChecker(version.Version),
		Settings:   settingsStore,
		File: api.FileSettings{
			ConfigPath:     *cfgPath,
			Listen:         *addr,
			LibvirtURI:     platformLibvirtURI(libvirtURI),
			ImageCacheDir:  imageCacheDir,
			MetadataPath:   metadataPath,
			VZNetwork:      cfg.VZNetwork,
			TLSCert:        certPath,
			TLSFingerprint: fingerprint,
		},
	})

	return runServer(srv, vmManager, loop, sampler, svcManager, *addr, tlsConfig)
}

// runServer serves srv until SIGINT or SIGTERM, then shuts down the HTTP
// server, bounded by shutdownTimeout, and closes the backend. loop, sampler
// and svc share the same signal context; svc may be nil.
func runServer(srv http.Handler, vmManager hypervisor.Backend, loop *reconcile.Loop, sampler *stats.Sampler, svc *services.Manager, addr string, tlsConfig *tls.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// No ReadTimeout or WriteTimeout, which would cut off consoles and image
	// pulls; the header and idle limits stop clients holding connections open.
	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}

	serveErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			serveErr <- err
			return
		}
		scheme := "http"
		if tlsConfig != nil {
			ln, scheme = tlscert.RedirectPlainHTTP(ln, tlsConfig), "https"
		}
		log.Printf("nanocloud %s listening on %s (%s)", version.Version, addr, scheme)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	if loop != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop.Run(ctx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		sampler.Run(ctx)
	}()

	if svc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Run(ctx)
		}()
	}

	select {
	case <-ctx.Done():
		log.Printf("shutting down: signal received")
	case err := <-serveErr:
		// The listener failed before any signal, so there is nothing to drain.
		_ = vmManager.Close()
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown did not finish within %s (%v) — forcing remaining connections closed", shutdownTimeout, err)
		_ = httpSrv.Close()
	}

	wg.Wait()

	if err := vmManager.Close(); err != nil {
		log.Printf("warning: closing hypervisor backend: %v", err)
	}

	return nil
}

func cmdInstall(_ []string) {
	if err := install.Run(serviceUnit()); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}
	// After Run, so the password is the last thing on screen. The daemon
	// re-reads auth.json when it changes, so it needs no restart to see it.
	store := authStoreFor(config.DefaultPath)
	if set, err := store.PasswordSet(); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	} else if !set {
		setPassword(store, auth.GeneratePassword(), true)
	}
}

// cmdPasswd sets the login password: a generated one by default, or a line
// read from stdin with --stdin. Every browser session ends; API tokens stay.
func cmdPasswd(args []string) {
	fset := flag.NewFlagSet("passwd", flag.ExitOnError)
	cfgPath := fset.String("config", config.DefaultPath, "path to config.yaml")
	fromStdin := fset.Bool("stdin", false, "read the new password from stdin instead of generating one")
	_ = fset.Parse(args)

	password := auth.GeneratePassword()
	if *fromStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintf(os.Stderr, "passwd: read stdin: %v\n", err)
			os.Exit(1)
		}
		password = strings.TrimRight(line, "\r\n")
		if len(password) < auth.MinPasswordLength {
			fmt.Fprintf(os.Stderr, "passwd: the password must be at least %d characters\n", auth.MinPasswordLength)
			os.Exit(1)
		}
	}
	setPassword(authStoreFor(*cfgPath), password, !*fromStdin)
}

// setPassword sets the password, printing it only when show is set: a
// generated one has to be shown, one the user typed must not be echoed.
func setPassword(store *auth.Store, password string, show bool) {
	if err := store.SetPassword(password); err != nil {
		fmt.Fprintf(os.Stderr, "set password: %v\n", err)
		os.Exit(1)
	}
	if !show {
		fmt.Println("Login password set. Every browser session has been signed out.")
		return
	}
	fmt.Println()
	fmt.Println("Login password set. Sign in with:")
	fmt.Println()
	fmt.Printf("    %s\n", password)
	fmt.Println()
	fmt.Println("It is not stored anywhere readable and will not be shown again;")
	fmt.Println("change it from Settings, or reset it with: sudo nanocloud passwd")
}

// authStoreFor is auth.json for the daemon configured at cfgPath, falling back
// to the default state directory when the config cannot be read.
func authStoreFor(cfgPath string) *auth.Store {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		cfg = &config.Config{}
	}
	return auth.New(filepath.Join(filepath.Dir(metadataPathFor(cfg)), "auth.json"))
}

// restrictVMFiles takes group and other access off the files of VMs created
// before disks and seeds were written private. When QEMU could not open such a
// file (usable is false) it changes nothing, so those VMs keep starting.
func restrictVMFiles(store *metadata.Store, usable bool) {
	if !usable {
		log.Printf("warning: libvirt does not hand VM disks to QEMU (dynamic_ownership is off), so new VMs cannot start and existing VMs' files keep their modes — see `sudo nanocloud doctor`")
		return
	}
	entries, err := store.Read()
	if err != nil {
		log.Printf("warning: could not read VM specs to restrict their files: %v", err)
		return
	}
	restricted := 0
	for _, e := range entries {
		disk, iso, ok := storage.PathsFor(e.PoolPath, e.ID)
		if !ok {
			continue
		}
		disks := []string{disk}
		for _, d := range e.Disks {
			disks = append(disks, d.Path)
		}
		paths := append([]string{iso}, disks...)
		// On macOS each snapshot is a whole copy of its disk.
		for _, d := range disks {
			snaps, _ := filepath.Glob(filepath.Join(storage.SnapshotDir(d), "*"))
			paths = append(paths, snaps...)
		}
		for _, p := range paths {
			changed, err := storage.Restrict(p)
			if err != nil {
				log.Printf("warning: %v", err)
			} else if changed {
				restricted++
			}
		}
	}
	if restricted > 0 {
		log.Printf("made %d disk, seed and snapshot files of existing VMs readable by their owner only", restricted)
	}
}

func metadataPathFor(cfg *config.Config) string {
	if cfg.MetadataPath != "" {
		return cfg.MetadataPath
	}
	return defaultMetadataPath
}

func cmdUninstall(args []string) {
	fset := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fset.Bool("purge", false, "also delete every VM, its disks, and all of nano-cloud's state")
	yes := fset.Bool("yes", false, "with -purge, do not ask for confirmation")
	_ = fset.Parse(args)
	if err := install.Uninstall(install.UninstallOptions{Purge: *purge, Yes: *yes, In: os.Stdin}); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
		os.Exit(1)
	}
}

// cmdDoctor reruns the host-readiness checks that install prints.
func cmdDoctor(args []string) {
	fset := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fset.String("config", config.DefaultPath, "path to config.yaml")
	_ = fset.Parse(args)

	libvirtURI := defaultLibvirtURI
	if cfg, err := config.Load(*cfgPath); err == nil && cfg.LibvirtURI != "" {
		libvirtURI = cfg.LibvirtURI
	}

	fmt.Println("Nano Cloud host readiness:")
	if !install.PrintChecks(install.RunChecks(libvirtURI)) {
		os.Exit(1)
	}
}
