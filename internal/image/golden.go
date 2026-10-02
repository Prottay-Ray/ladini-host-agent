package image

import (
	"fmt"
	"os"
	"path/filepath"
)

// GoldenUserData is the cloud-init program that turns a stock Ubuntu cloud
// image into our golden image. It runs exactly once, during `image build`.
//
// Everything the guest needs is baked here rather than delivered per-VM, so
// that the per-VM seed carries nothing but a hostname, an instance id and the
// per-boot handshake token.
//
// The hardening steps are not incidental -- each one closes a path that the
// isolation test matrix checks for:
//   - openssh-server is purged         (no network-reachable auth surface)
//   - getty on hvc0 is masked          (the console is output-only)
//   - IPv6 is disabled                 (pure extra surface for the MVP)
//   - nftables blocks RFC1918 egress   (defence in depth; see the note below)
const GoldenUserData = `#cloud-config
hostname: ladini-golden
preserve_hostname: false
ssh_pwauth: false
disable_root: true

write_files:
  - path: /etc/systemd/system/ladini-guestd.service
    permissions: '0644'
    content: |
      [Unit]
      Description=Ladini guest agent
      After=docker.service
      Requires=docker.service

      [Service]
      Type=simple
      ExecStart=/usr/local/bin/ladini-guestd
      Restart=always
      RestartSec=1

      [Install]
      WantedBy=multi-user.target

  # Defence in depth only. Guest root can flush this, so it is NOT the
  # control that makes the isolation claim true -- that is the host-side
  # userspace netstack at M5. What this does buy, today, is that a container
  # escaping to guest userspace cannot reach the host's own listening
  # services or scan the operator's LAN.
  #
  # DNS is allowed to any destination because under NAT the resolver IS the
  # vmnet gateway, and docker pull needs it. That single port is the
  # residual hole in this ruleset, and it closes at M5.
  - path: /etc/nftables.conf
    permissions: '0644'
    content: |
      #!/usr/sbin/nft -f
      flush ruleset
      table inet ladini {
        set private {
          type ipv4_addr
          flags interval
          elements = {
            10.0.0.0/8,
            172.16.0.0/12,
            192.168.0.0/16,
            169.254.0.0/16,
            100.64.0.0/10
          }
        }
        chain output {
          type filter hook output priority filter; policy accept;
          oif "lo" accept
          ct state established,related accept
          udp dport 53 accept
          tcp dport 53 accept
          udp dport { 67, 68 } accept
          oifname "docker0" accept
          oifname "br-*" accept
          ip daddr @private drop
        }
        chain forward {
          type filter hook forward priority filter; policy accept;
          ct state established,related accept
          udp dport 53 accept
          tcp dport 53 accept
          # Traffic heading INTO a container bridge is container networking,
          # not egress: by the time the forward hook sees a published port,
          # DNAT has already rewritten the destination to the container's
          # 172.17.0.0/16 address, which would otherwise match @private and
          # be dropped -- silently breaking every published port.
          oifname "docker0" accept
          oifname "br-*" accept
          ip daddr @private drop
        }
      }

  - path: /etc/docker/daemon.json
    permissions: '0644'
    content: |
      {
        "userns-remap": "default",
        "log-driver": "json-file",
        "log-opts": {"max-size": "10m", "max-file": "3"},
        "live-restore": false
      }

  - path: /etc/sysctl.d/99-ladini.conf
    permissions: '0644'
    content: |
      net.ipv6.conf.all.disable_ipv6 = 1
      net.ipv6.conf.default.disable_ipv6 = 1

runcmd:
  # The guest agent travels on the cidata ISO rather than being base64'd into
  # this file, which keeps the user-data readable and the ISO small.
  - mkdir -p /mnt/cidata
  - mount -o ro /dev/disk/by-label/cidata /mnt/cidata
  - install -m 0755 /mnt/cidata/ladini-guestd /usr/local/bin/ladini-guestd
  - install -d -m 0755 /usr/local/lib/ladini
  - install -m 0755 /mnt/cidata/isolation-check.sh /usr/local/lib/ladini/isolation-check.sh
  - umount /mnt/cidata
  - rmdir /mnt/cidata

  - export DEBIAN_FRONTEND=noninteractive
  - apt-get update
  - apt-get install -y ca-certificates curl nftables util-linux iproute2 iputils-ping netcat-openbsd python3

  # Docker from download.docker.com with a pinned keyring. Never curl|sh.
  - install -m 0755 -d /etc/apt/keyrings
  - curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  - chmod a+r /etc/apt/keyrings/docker.asc
  - echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
  - apt-get update
  - apt-get install -y docker-ce docker-ce-cli containerd.io

  # Hardening.
  - apt-get purge -y --auto-remove openssh-server || true
  - systemctl mask serial-getty@hvc0.service || true
  - sysctl -p /etc/sysctl.d/99-ladini.conf || true
  - systemctl enable nftables
  - systemctl enable ladini-guestd
  - systemctl enable docker

  - apt-get clean
  - rm -rf /var/lib/apt/lists/*
  - cloud-init clean --logs --seed

power_state:
  mode: poweroff
  timeout: 60
  condition: true
`

// GoldenMetaData is the NoCloud meta-data for the one-off bake boot.
const GoldenMetaData = `instance-id: ladini-golden-bake
local-hostname: ladini-golden
`

// Prepared is the result of the fetch/convert stage: a raw disk ready to be
// booted once with bakeSeed attached, after which it becomes the golden image.
type Prepared struct {
	RawPath    string
	SeedPath   string
	GoldenPath string
}

// Prepare performs every step of the golden build that does not require
// booting a VM: fetch, verify, convert qcow2 -> raw, size, and write the bake
// seed. The caller boots RawPath with SeedPath attached, waits for the guest
// to power itself off, then calls Seal.
func Prepare(m Manifest, imagesDir, cacheDir, workDir, guestdPath string, progress func(string)) (*Prepared, error) {
	qcow2, err := Fetch(m, cacheDir, progress)
	if err != nil {
		return nil, err
	}

	raw := filepath.Join(workDir, "root.raw")
	if err := ConvertToRaw(qcow2, raw, m.DiskSize, progress); err != nil {
		return nil, err
	}

	guestd, err := os.ReadFile(guestdPath)
	if err != nil {
		return nil, fmt.Errorf("read guest agent (did you run `make guestd`?): %w", err)
	}
	seed := filepath.Join(workDir, "seed.iso")
	if err := WriteSeedISO(seed, map[string]string{
		"user-data":          GoldenUserData,
		"meta-data":          GoldenMetaData,
		"ladini-guestd":      string(guestd),
		"isolation-check.sh": IsolationCheck,
	}); err != nil {
		return nil, err
	}
	progress("bake seed written")

	return &Prepared{
		RawPath:    raw,
		SeedPath:   seed,
		GoldenPath: filepath.Join(imagesDir, m.GoldenName()),
	}, nil
}

// Seal moves the baked disk into the images directory and makes it read-only,
// so that no later bug can mutate the base every VM clones from.
func Seal(p *Prepared, m Manifest, progress func(string)) error {
	if err := os.Rename(p.RawPath, p.GoldenPath); err != nil {
		return fmt.Errorf("install golden image: %w", err)
	}
	if err := os.Chmod(p.GoldenPath, 0o444); err != nil {
		return fmt.Errorf("seal golden image: %w", err)
	}
	if err := writeSidecar(p.GoldenPath, m); err != nil {
		return err
	}
	progress("golden image sealed read-only: " + p.GoldenPath)
	return nil
}
