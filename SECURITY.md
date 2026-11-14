# Security

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/nanocloud-io/nanocloud/security/advisories/new),
not in a public issue. Include the version (`nanocloud version`), the
platform and the steps to reproduce.

You'll get an answer within a week. Every merge to `main` is released, so a
fix ships as a normal release, and the advisory is published once it's out.
Only the latest release receives fixes.

## Threat model

Nano Cloud manages VMs on the machine it runs on, so the daemon runs as
**root**: it defines domains, writes disks and creates network interfaces.
Whoever controls the daemon controls the host, and everything below follows
from that.

It's built for one administrator: one password and any number of API tokens,
all with full access. There are no users, roles or read-only access, and
there won't be. Don't give a login to anyone you wouldn't give root on the
host.

### What the login protects

- Every API route and console needs a login, except `/health`, the login
  endpoints and the static UI. Until `sudo nanocloud passwd` sets a password,
  the daemon refuses everything.
- The password is stored as a bcrypt hash, and sessions and API tokens only
  as SHA-256 hashes of 32 random bytes, so reading `auth.json` (mode 0600)
  doesn't let anyone sign in.
- A browser session is split in two. A cookie (`HttpOnly`, `Secure`,
  `SameSite=Strict`) carries one half; the page keeps the other in its own
  storage and sends it with each request. Browsers send a cookie to every
  port on the host, including the service ports forwarded into your VMs, so
  the cookie alone signs nothing in. A session lasts 30 days, renewed while
  it's used.
- State-changing requests and every console WebSocket are rejected when
  `Sec-Fetch-Site` or `Origin` says they came from another site. A web page
  you visit can't drive the API or open a console with your cookie.
- Creating or revoking an API token and changing the password need a browser
  session, so a leaked token can't mint its own replacements. Revoke tokens
  in Settings → Account & security.
- Password guessing is slowed per client address, and an IPv6 client counts
  as its whole /64. After a few failures each one doubles the wait, up to 15
  minutes, and only a few passwords are checked at once.
- Traffic is encrypted. The daemon serves HTTPS with a certificate it
  generates on first start, or one you configure (`cert` and `key` under
  `tls:` in `config.yaml`). The installer prints the self-signed
  certificate's fingerprint, and Settings shows it, so you can check it
  against what your browser is shown.
- `nanocloud update` and `install.sh` check a release's GPG signature against
  a key built into them, then each binary's SHA-256, before replacing
  anything.

### Files on the host

Other local accounts can't read what a VM holds.

- VM disks and cloud-init seed ISOs are created mode 0600, owned by root.
  That matters most for a service, whose seed carries its database
  passwords, S3 keys or cluster token in plain text. `metadata.json` and
  `templates.json`, which repeat a VM's cloud-init files and commands, are
  0600 too.
- The directories nano-cloud creates for them are 0711: the state directory
  (`/var/lib/nano-cloud`, or `/Library/Application Support/nano-cloud` on a
  Mac), its image cache and default pool, and a pool directory that Settings
  creates. QEMU runs as its own user and opens a disk by its path, so other
  accounts can still pass through, as with `/var/lib/libvirt/images`, but
  can't list or read anything.
- On Linux, libvirt hands each disk and seed to QEMU's user (`libvirt-qemu`
  or `qemu`) while the VM runs, and gives a disk back to root when the VM
  stops. libvirt never hands a seed back, since it's attached read-only, so a
  seed stays with QEMU's user, still 0600. This is libvirt's
  `dynamic_ownership`, on by default. If `/etc/libvirt/qemu.conf` turns it
  off, QEMU can't open these files and VMs can't start; `nanocloud doctor`
  reports it.
- VMs created before this protection are tightened when the daemon starts:
  their disks, seeds and, on a Mac, snapshots lose group and other access,
  and `nanocloud install`, which `nanocloud update` runs, makes the
  directories 0711. With `dynamic_ownership` off, the daemon leaves those
  files as they are, so the VMs keep starting.
- Cached cloud images stay readable by everyone (0644). They're the
  distributions' public images, and on Linux every VM disk is an overlay
  that QEMU reads one through.

### What it does not protect

Anyone with root owns the box. Run Nano Cloud on a host where every login
belongs to someone you'd trust with root. Disks aren't encrypted at rest, so
root, a backup of the pool or a stolen drive can read every VM.

Also outside what it protects:

- A storage pool in a directory you created yourself keeps that directory's
  mode. The disks in it are 0600 all the same, but if the directory is
  world-readable, anyone can list them, and so learn your VMs' names.

- A guest isn't a sandbox from its own network. VMs on the NAT network can
  reach each other, and a bridged VM is on your LAN like any other machine.
- The VMs themselves. Their passwords, SSH keys, open ports and updates are
  yours to manage. A VM's login password is hashed before it's written to the
  seed ISO, though the plaintext passes through the API; a service's
  generated passwords aren't hashed (see above).
- With `tls: {enabled: false}`, meant for a reverse proxy or local
  development, the password and session cookie cross the network in clear
  unless something in front adds TLS.
- A self-signed certificate protects against eavesdropping, but only checking
  its fingerprint protects against an attacker in the middle on first use.
- The daemon listens on every interface and isn't hardened for the open
  internet. Keep port 8080 to your LAN or a VPN.

### Release signing key

Releases are signed with this key. `install.sh` and `nanocloud update` carry
it and check every release against it.

```
nano-cloud releases <releases@nanocloud.io>
ed25519, fingerprint 5289 9456 DD0B C728 383E  1910 39DE 3648 2CBD 9253
expires 2028-07-03
```

The public key is in the repository at `internal/update/release.asc`.

### Outbound connections

The daemon checks GitHub for a newer release at most every 12 hours (turn it
off under Settings → Updates). Anything else it fetches, you asked for: cloud
images from each distribution's mirror, and SSH keys from GitHub when you
name a GitHub user. There's no telemetry.
