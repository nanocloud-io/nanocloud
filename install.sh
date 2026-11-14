#!/bin/sh
# nanocloud installer
# Usage: curl -fsSL https://raw.githubusercontent.com/nanocloud-io/nanocloud/main/install.sh | sudo sh
#
#   Pin a release:  ... | sudo NANOCLOUD_VERSION=v0.2.0 sh
#   Uninstall:      ... | sudo sh -s -- uninstall            (keeps VMs and their disks)
#   Remove all:     ... | sudo sh -s -- uninstall --purge    (deletes VMs, disks and state)
#
# Linux (amd64, arm64): libvirt/QEMU, installed from the distro's packages.
# macOS (Apple silicon): Apple's Virtualization.framework, built into the OS;
#   nothing else is installed. gpg is needed only to verify the download.
set -eu

REPO="nanocloud-io/nanocloud"
BINARY_NAME="nanocloud"
INSTALL_DIR="/usr/local/bin"


RELEASE_PUBKEY='-----BEGIN PGP PUBLIC KEY BLOCK-----

mDMEaki4rxYJKwYBBAHaRw8BAQdAfQ0WbfeWU/DL+gUktL+OdQASwLWFwIVbxIhn
WPU3JWG0K25hbm8tY2xvdWQgcmVsZWFzZXMgPHJlbGVhc2VzQG5hbm9jbG91ZC5p
bz6ItQQTFgoAXRYhBFKJlFbdC8coOD4ZEDneNkgsvZJTBQJqSLivGxSAAAAAAAQA
Dm1hbnUyLDIuNSsxLjEyLDAsMwIbAwUJA8JnAAULCQgHAgIiAgYVCgkICwIEFgID
AQIeBwIXgAAKCRA53jZILL2SU7bPAQDP7zyVVZWM7Z345HxieYn8jpxbYqb6w7yC
Rxvl8lLnRwEA0jaZFsjGgk+wXvd5egdOEotQCGKvNyzKFjDSMrHSsAY=
=N6xL
-----END PGP PUBLIC KEY BLOCK-----'

#
# Helpers
#

die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

warn() {
    printf 'warning: %s\n' "$*" >&2
}

info() {
    printf '%s\n' "$*" >&2
}

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

#
# Arch detection
#

detect_arch() {
    case "$(uname -m)" in
        x86_64)          printf 'amd64' ;;
        aarch64 | arm64) printf 'arm64' ;;
        *)               die "unsupported architecture: $(uname -m) — only x86_64 and aarch64 are supported" ;;
    esac
}

#
# OS checks
#

detect_os() {
    case "$(uname -s)" in
        Linux)  printf 'linux' ;;
        Darwin) printf 'darwin' ;;
        *)      die "nanocloud requires Linux or macOS; detected: $(uname -s)" ;;
    esac
}

# check_macos: Apple silicon and a macOS whose Virtualization.framework has
# EFI boot (13+). Everything else nanocloud needs ships with macOS.
check_macos() {
    [ "$(uname -m)" = "arm64" ] || die "nanocloud on macOS needs an Apple silicon Mac; detected: $(uname -m)"
    major=$(sw_vers -productVersion | cut -d. -f1)
    [ "$major" -ge 13 ] 2>/dev/null || die "nanocloud needs macOS 13 or later; detected: $(sw_vers -productVersion)"
}

# check_root_only: the service runs the binary as root, so anyone who can
# change it, or rename a directory above it, gets root at the next restart.
# The fix is printed, not applied, because Homebrew may own /usr/local/bin.
# stat -L doesn't follow a symlink's parents, so a directory's resolved path
# is walked too. `nanocloud install` runs the same check.
check_root_only() {
    if [ "$(uname -s)" = Darwin ]; then
        # staff is every account's primary group on macOS, so it is not trusted.
        service_manager=launchd root_group=wheel staff_gid=
    else
        # Debian lays out /usr/local as root:staff 2775 and documents staff as
        # able to modify it, so its members can already replace binaries on
        # root's PATH. An unresolved staff group trusts no group.
        service_manager=systemd root_group=root
        staff_gid=$(getent group staff 2>/dev/null | cut -d: -f3)
    fi
    root_only_chain "$1"
    if [ -d "$1" ]; then
        resolved=$(cd -P "$1" && pwd -P) || die "cannot resolve $1"
        if [ "$resolved" != "$1" ]; then
            root_only_chain "$resolved"
        fi
    fi
}

# root_only_chain refuses $1 or any directory above it that is not owned by
# root, is writable by others, or is writable by a group other than staff on
# Linux. A sticky bit does not excuse one. A path that does not exist yet is
# skipped: the installer creates it as root.
root_only_chain() {
    p="$1"
    while :; do
        if [ -e "$p" ]; then
            st=$(owner_of "$p") || die "cannot stat $p"
            uid=${st%% *}
            gid=${st#* }
            gid=${gid% *}
            mode=$(printf '%03o' $((0${st##* } & 0777)))
            if [ "$uid" -ne 0 ]; then
                unsafe_path "$p" "is owned by uid $uid, not root"
            elif [ $((0$mode & 002)) -ne 0 ]; then
                unsafe_path "$p" "is writable by others (mode $mode)"
            elif [ $((0$mode & 020)) -ne 0 ] && [ "$gid" != "$staff_gid" ]; then
                unsafe_path "$p" "is writable by group $gid (mode $mode)"
            fi
        fi
        if [ "$p" = / ]; then
            return 0
        fi
        p=$(dirname "$p")
    done
}

# owner_of prints "uid gid mode" for $1, following symlinks.
owner_of() {
    if [ "$(uname -s)" = Darwin ]; then
        # Not plain stat: GNU coreutils, often first in PATH, reads -f differently.
        /usr/bin/stat -L -f '%u %g %Lp' "$1"
    else
        stat -L -c '%u %g %a' "$1"
    fi
}

unsafe_path() {
    staff_note=
    if [ "$service_manager" = systemd ]; then
        staff_note=" Group write is accepted only for staff, which Debian treats as equivalent to root."
    fi
    die "$1 $2.
${service_manager} runs ${INSTALL_DIR}/${BINARY_NAME} as root, so anyone who can modify $1 can swap in their own binary and gain root at the next restart.${staff_note}
Fix it, then run the install again:
  sudo chown root:${root_group} $1 && sudo chmod 755 $1"
}

check_os() {
    case "$(uname -s)" in
        Linux) ;;
        *) die "nanocloud requires Linux; detected: $(uname -s)" ;;
    esac

    if [ -f /etc/debian_version ]; then
        :
    elif [ -f /etc/fedora-release ] || [ -f /etc/redhat-release ]; then
        :
    else
        warn "unrecognised Linux distribution — install may succeed but is untested on this OS"
    fi
}

#
# Host dependency checks
#

check_kvm() {
    if [ ! -c /dev/kvm ]; then
        warn "/dev/kvm not found — VMs will run without hardware acceleration (very slow)"
        warn "Enable KVM in your hypervisor settings or check that the kvm kernel module is loaded"
    fi
}

pkg_hint() {
    # $1 = debian package, $2 = fedora/rhel package
    if [ -f /etc/debian_version ]; then
        printf '  Install with: sudo apt-get install -y %s\n' "$1" >&2
    elif [ -f /etc/fedora-release ] || [ -f /etc/redhat-release ]; then
        printf '  Install with: sudo dnf install -y %s\n' "$2" >&2
    fi
}

# install_pkg installs a package quietly, printing the package manager's
# output only on failure.
# $1 = human label, $2 = debian package name, $3 = fedora/rhel package name.
# Returns: 0 installed, 1 install command failed, 2 unsupported distro.
install_pkg() {
    label="$1"
    debian_pkg="$2"
    fedora_pkg="$3"
    pkg_output=$(mktemp)

    if [ -f /etc/debian_version ]; then
        info "Installing ${label} via apt-get..."
        if DEBIAN_FRONTEND=noninteractive apt-get install -y "$debian_pkg" >"$pkg_output" 2>&1; then
            rm -f "$pkg_output"
            return 0
        fi
        warn "apt-get install ${debian_pkg} failed:"
        sed 's/^/    /' "$pkg_output" >&2
        rm -f "$pkg_output"
        return 1
    fi

    if [ -f /etc/fedora-release ] || [ -f /etc/redhat-release ]; then
        info "Installing ${label} via dnf..."
        if dnf install -y "$fedora_pkg" >"$pkg_output" 2>&1; then
            rm -f "$pkg_output"
            return 0
        fi
        warn "dnf install ${fedora_pkg} failed:"
        sed 's/^/    /' "$pkg_output" >&2
        rm -f "$pkg_output"
        return 1
    fi

    rm -f "$pkg_output"
    return 2 # unsupported distro — caller prints the manual-install hint
}

check_host_deps() {
    missing=0

    if ! command -v virsh >/dev/null 2>&1; then
        if install_pkg "libvirt" "libvirt-daemon-system" "libvirt"; then
            :
        else
            status=$?
            if [ "$status" -eq 2 ]; then
                printf 'error: libvirt is not installed.\n' >&2
                pkg_hint "libvirt-daemon-system" "libvirt"
            fi
            missing=1
        fi
        if [ "$missing" -eq 0 ] && ! command -v virsh >/dev/null 2>&1; then
            printf 'error: libvirt installation attempt failed; virsh still not found.\n' >&2
            missing=1
        fi
    fi

    if ! command -v xorriso >/dev/null 2>&1; then
        if install_pkg "xorriso" "xorriso" "xorriso"; then
            :
        else
            status=$?
            if [ "$status" -eq 2 ]; then
                printf 'error: xorriso is not installed (required for cloud-init ISO generation).\n' >&2
                pkg_hint "xorriso" "xorriso"
            fi
            missing=1
        fi
        if [ "$missing" -eq 0 ] && ! command -v xorriso >/dev/null 2>&1; then
            printf 'error: xorriso installation attempt failed; xorriso still not found.\n' >&2
            missing=1
        fi
    fi

    [ "$missing" -eq 0 ] || exit 1
}

#
# Version / download / verify
#

# is_version: a strict vMAJOR.MINOR.PATCH, checked before a version is ever
# interpolated into a download URL. (A case glob would not do: its * matches /.)
is_version() {
    printf '%s' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'
}

fetch_latest_version() {
    need_cmd curl
    version=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' \
        | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
    [ -n "$version" ] || die "could not determine latest release version from GitHub API"
    is_version "$version" || die "unexpected release version format from GitHub API: '$version'"
    printf '%s' "$version"
}

# resolve_version: NANOCLOUD_VERSION when set, else the latest release.
resolve_version() {
    if [ -n "${NANOCLOUD_VERSION:-}" ]; then
        is_version "$NANOCLOUD_VERSION" || die "NANOCLOUD_VERSION must look like v0.2.0, not '$NANOCLOUD_VERSION'"
        printf '%s' "$NANOCLOUD_VERSION"
    else
        fetch_latest_version
    fi
}

# Verify the authenticity of sha256sums.txt (already downloaded into $tmpdir)
# using the embedded release key. Fails closed.
verify_signature() {
    tmpdir="$1"
    version="$2"
    base_url="https://github.com/${REPO}/releases/download/${version}"

    if [ "${NANOCLOUD_SKIP_GPG:-0}" = "1" ]; then
        warn "GPG signature verification skipped (NANOCLOUD_SKIP_GPG=1)."
        warn "The SHA-256 check alone does NOT protect against a compromised download path."
        return 0
    fi

    if [ "$(uname -s)" = "Darwin" ] && ! command -v gpg >/dev/null 2>&1; then
        die "gpg is needed to verify the release signature and is not installed.
  Install it (for example: brew install gnupg) and re-run, or, at your own risk:
      curl -fsSL .../install.sh | sudo NANOCLOUD_SKIP_GPG=1 sh"
    fi
    need_cmd gpg
    curl -fsSL "${base_url}/sha256sums.txt.sig" -o "${tmpdir}/sha256sums.txt.sig" \
        || die "could not download release signature (sha256sums.txt.sig)"

    # Use a throwaway keyring so we never touch the operator's GPG state.
    gnupghome=$(mktemp -d)
    if ! printf '%s\n' "$RELEASE_PUBKEY" | GNUPGHOME="$gnupghome" gpg --quiet --import 2>/dev/null; then
        rm -rf "$gnupghome"
        die "failed to import embedded release signing key"
    fi
    if ! GNUPGHOME="$gnupghome" gpg --quiet --verify \
            "${tmpdir}/sha256sums.txt.sig" "${tmpdir}/sha256sums.txt" 2>/dev/null; then
        rm -rf "$gnupghome"
        die "signature verification of sha256sums.txt FAILED — refusing to install"
    fi
    rm -rf "$gnupghome"
    info "Signature verified."
}

# Download the binary + checksums into $tmpdir, verify the checksum file's
# signature, then verify the binary against it. Prints only diagnostics (to
# stderr); leaves the verified binary at "$tmpdir/$binary".
download_and_verify() {
    arch="$1"
    version="$2"
    tmpdir="$3"
    os="${4:-linux}"
    binary="${BINARY_NAME}-${os}-${arch}"
    base_url="https://github.com/${REPO}/releases/download/${version}"

    info "Downloading ${BINARY_NAME} ${version} (${arch})..."
    curl -fsSL "${base_url}/${binary}" -o "${tmpdir}/${binary}" \
        || die "could not download ${binary} ${version} — does that release exist?"
    curl -fsSL "${base_url}/sha256sums.txt" -o "${tmpdir}/sha256sums.txt" \
        || die "could not download sha256sums.txt for ${version}"

    verify_signature "$tmpdir" "$version"

    grep "^[0-9a-f]\{64\}  ${binary}\$" "${tmpdir}/sha256sums.txt" > "${tmpdir}/check.txt" \
        || die "checksum entry for ${binary} not found in sha256sums.txt"
    if [ "$os" = "darwin" ]; then
        need_cmd shasum
        ( cd "$tmpdir" && shasum -a 256 -c check.txt >/dev/null 2>&1 ) \
            || die "checksum verification failed for ${binary}"
    else
        need_cmd sha256sum
        ( cd "$tmpdir" && sha256sum -c check.txt >/dev/null 2>&1 ) \
            || die "checksum verification failed for ${binary}"
    fi
    info "Checksum verified."
}

#
# Install
#

do_install() {
    # Fail fast before any network activity if we are not root.
    [ "$(id -u)" -eq 0 ] || die "installer must be run as root (try: curl ... | sudo sh)"

    arch=$(detect_arch)
    os=$(detect_os)
    if [ "$os" = "darwin" ]; then
        check_macos
        check_root_only "$INSTALL_DIR"
    else
        check_os
        check_root_only "$INSTALL_DIR"
        check_kvm
        check_host_deps
    fi

    version=$(resolve_version)

    tmpdir=$(mktemp -d)
    # Trap lives in the top-level shell (not a command-substitution subshell) so
    # the download directory survives until the binary has been installed.
    trap 'rm -rf "$tmpdir"' EXIT INT TERM

    download_and_verify "$arch" "$version" "$tmpdir" "$os"

    binary="${BINARY_NAME}-${os}-${arch}"
    mkdir -p "$INSTALL_DIR"
    install -m 0755 "${tmpdir}/${binary}" "${INSTALL_DIR}/${BINARY_NAME}"
    # Again for what mkdir and install just created, before it runs as root.
    check_root_only "${INSTALL_DIR}/${BINARY_NAME}"
    info "Binary installed to ${INSTALL_DIR}/${BINARY_NAME}"

    # install prints where to open the UI and the certificate's fingerprint.
    "${INSTALL_DIR}/${BINARY_NAME}" install
}

#
# Uninstall
#

# do_uninstall runs a freshly downloaded, verified release's own uninstall
# rather than the installed binary's: one older than --purge would ignore the
# flag and quietly keep everything.
do_uninstall() {
    [ "$(id -u)" -eq 0 ] || die "uninstall must be run as root (try: curl ... | sudo sh -s -- uninstall)"

    arch=$(detect_arch)
    os=$(detect_os)
    version=$(resolve_version)

    tmpdir=$(mktemp -d)
    trap 'rm -rf "$tmpdir"' EXIT INT TERM

    download_and_verify "$arch" "$version" "$tmpdir" "$os"
    binary="${tmpdir}/${BINARY_NAME}-${os}-${arch}"
    chmod 0755 "$binary"

    # stdin is this script when piped from curl, so --purge's confirmation is
    # read from the terminal instead.
    if [ -r /dev/tty ]; then
        "$binary" uninstall "$@" </dev/tty
    else
        "$binary" uninstall "$@"
    fi
}

case "${1:-install}" in
    install)   do_install ;;
    uninstall) shift; do_uninstall "$@" ;;
    *)         die "usage: install.sh [install | uninstall [--purge] [--yes]]" ;;
esac
