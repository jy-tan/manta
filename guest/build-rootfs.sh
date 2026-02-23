#!/usr/bin/env bash
set -euo pipefail

# Builds an Alpine ext4 rootfs image for Firecracker and an SSH keypair
# in ./guest-artifacts by default.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

if [[ "${EUID}" -ne 0 ]]; then
  echo "run as root: sudo ./guest/build-rootfs.sh"
  exit 1
fi

OUT_DIR="${OUT_DIR:-${ROOT_DIR}/guest-artifacts}"
STAGING="${STAGING:-/tmp/manta-rootfs-staging}"
ROOTFS_PATH="${ROOTFS_PATH:-${OUT_DIR}/rootfs.ext4}"
ROOTFS_SIZE_MB="${ROOTFS_SIZE_MB:-1024}"
TMP_KEY_PATH="${TMP_KEY_PATH:-${OUT_DIR}/sandbox_key}"
ALPINE_MIRROR="${ALPINE_MIRROR:-https://dl-cdn.alpinelinux.org/alpine}"
ALPINE_BRANCH="${ALPINE_BRANCH:-v3.20}"
ALPINE_ARCH="${ALPINE_ARCH:-x86_64}"

require_cmds=(curl tar mkfs.ext4 resize2fs chroot sed ssh-keygen go)
for cmd in "${require_cmds[@]}"; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "missing required command: ${cmd}" >&2
    exit 1
  fi
done

mkdir -p "${OUT_DIR}" "${STAGING}"
rm -rf "${STAGING:?}/"*
mkdir -p "${STAGING}"/{bin,sbin,proc,sys,dev,etc,root,tmp,usr,var}
chmod 1777 "${STAGING}/tmp"

# Build the in-guest agent as a static binary so it runs on Alpine (musl) even
# when built on a glibc host.
case "${ALPINE_ARCH}" in
  x86_64) GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  *)
    echo "unsupported alpine arch for agent build: ${ALPINE_ARCH} (expected x86_64 or aarch64)" >&2
    exit 1
    ;;
esac

echo "building manta-agent (GOARCH=${GOARCH})..."
(cd "${ROOT_DIR}" && CGO_ENABLED=0 GOOS=linux GOARCH="${GOARCH}" go build -trimpath -ldflags="-s -w" -o "${OUT_DIR}/manta-agent" ./cmd/agent)

MINIROOTFS_INDEX="${ALPINE_MIRROR}/${ALPINE_BRANCH}/releases/${ALPINE_ARCH}/"
MINIROOTFS_NAME="$(curl -fsSL "${MINIROOTFS_INDEX}" | rg -o "alpine-minirootfs-[0-9]+\\.[0-9]+\\.[0-9]+-${ALPINE_ARCH}\\.tar\\.gz" | sort -u | tail -n 1)"

if [[ -z "${MINIROOTFS_NAME}" ]]; then
  echo "unable to resolve alpine minirootfs archive from ${MINIROOTFS_INDEX}" >&2
  exit 1
fi

MINIROOTFS_URL="${MINIROOTFS_INDEX}${MINIROOTFS_NAME}"
MINIROOTFS_TAR="${OUT_DIR}/${MINIROOTFS_NAME}"

if [[ ! -f "${MINIROOTFS_TAR}" ]]; then
  echo "downloading ${MINIROOTFS_URL}..."
  curl -fL "${MINIROOTFS_URL}" -o "${MINIROOTFS_TAR}"
fi

tar -xzf "${MINIROOTFS_TAR}" -C "${STAGING}"

cat > "${STAGING}/etc/apk/repositories" <<EOF
${ALPINE_MIRROR}/${ALPINE_BRANCH}/main
${ALPINE_MIRROR}/${ALPINE_BRANCH}/community
EOF

cp -f /etc/resolv.conf "${STAGING}/etc/resolv.conf"

mount --bind /dev "${STAGING}/dev"
mount -t proc proc "${STAGING}/proc"
mount -t sysfs sys "${STAGING}/sys"
cleanup() {
  if mountpoint -q "${STAGING}/sys"; then
    umount -lf "${STAGING}/sys" || true
  fi
  if mountpoint -q "${STAGING}/proc"; then
    umount -lf "${STAGING}/proc" || true
  fi
  if mountpoint -q "${STAGING}/dev"; then
    umount -lf "${STAGING}/dev" || true
  fi
}
trap cleanup EXIT

chroot "${STAGING}" /bin/sh -c '
  set -eux
  apk add --no-cache \
    alpine-base openrc openssh-server iproute2 python3 nodejs npm curl git bash
'

mkdir -p "${STAGING}/root/.ssh"
if [[ ! -f "${TMP_KEY_PATH}" ]]; then
  ssh-keygen -t ed25519 -f "${TMP_KEY_PATH}" -N "" -q
fi
cp -f "${TMP_KEY_PATH}.pub" "${STAGING}/root/.ssh/authorized_keys"
chmod 700 "${STAGING}/root/.ssh"
chmod 600 "${STAGING}/root/.ssh/authorized_keys"

sed -i 's/^#\?PermitRootLogin.*/PermitRootLogin yes/' "${STAGING}/etc/ssh/sshd_config"
sed -i 's/^#\?PubkeyAuthentication.*/PubkeyAuthentication yes/' "${STAGING}/etc/ssh/sshd_config"
if ! grep -q '^PasswordAuthentication no' "${STAGING}/etc/ssh/sshd_config"; then
  echo "PasswordAuthentication no" >> "${STAGING}/etc/ssh/sshd_config"
fi

chroot "${STAGING}" /usr/bin/ssh-keygen -A
echo "sandbox" > "${STAGING}/etc/hostname"

# Install manta-agent and enable it at boot.
install -D -m 0755 "${OUT_DIR}/manta-agent" "${STAGING}/usr/local/bin/manta-agent"
cat > "${STAGING}/etc/init.d/manta-agent" <<'EOF'
#!/sbin/openrc-run

name="manta-agent"
command="/usr/local/bin/manta-agent"
command_background=true
pidfile="/run/manta-agent.pid"

depend() {
  need devfs procfs sysfs
  after bootmisc
}
EOF
chmod +x "${STAGING}/etc/init.d/manta-agent"

cat > "${STAGING}/etc/network/interfaces" <<EOF
auto lo
iface lo inet loopback

iface eth0 inet manual
EOF
echo "nameserver 1.1.1.1" > "${STAGING}/etc/resolv.conf"

mkdir -p "${STAGING}/etc/runlevels/sysinit" "${STAGING}/etc/runlevels/boot" "${STAGING}/etc/runlevels/default"
ln -sf /etc/init.d/devfs "${STAGING}/etc/runlevels/sysinit/devfs"
ln -sf /etc/init.d/procfs "${STAGING}/etc/runlevels/sysinit/procfs"
ln -sf /etc/init.d/sysfs "${STAGING}/etc/runlevels/sysinit/sysfs"
ln -sf /etc/init.d/hostname "${STAGING}/etc/runlevels/boot/hostname"
ln -sf /etc/init.d/networking "${STAGING}/etc/runlevels/boot/networking"
ln -sf /etc/init.d/bootmisc "${STAGING}/etc/runlevels/boot/bootmisc"
ln -sf /etc/init.d/sshd "${STAGING}/etc/runlevels/default/sshd"
ln -sf /etc/init.d/manta-agent "${STAGING}/etc/runlevels/default/manta-agent"
ln -sf /etc/init.d/agetty "${STAGING}/etc/init.d/agetty.ttyS0"
if ! grep -q '^ttyS0$' "${STAGING}/etc/securetty"; then
  echo "ttyS0" >> "${STAGING}/etc/securetty"
fi
ln -sf /etc/init.d/agetty.ttyS0 "${STAGING}/etc/runlevels/default/agetty.ttyS0"

# Important: remove pseudo-fs mounts before mkfs -d population.
cleanup
mkdir -p "${STAGING}/proc" "${STAGING}/sys" "${STAGING}/dev"

# Keep rootfs smaller for faster copy/boot in this naive baseline.
rm -rf "${STAGING}/var/cache/apk" \
       "${STAGING}/usr/share/man" \
       "${STAGING}/usr/share/doc" \
       "${STAGING}/usr/share/locale" || true

truncate -s "${ROOTFS_SIZE_MB}M" "${ROOTFS_PATH}"
mkfs.ext4 -F -d "${STAGING}" "${ROOTFS_PATH}" >/dev/null
resize2fs -M "${ROOTFS_PATH}" >/dev/null

echo "rootfs image ready: ${ROOTFS_PATH}"
echo "ssh key: ${TMP_KEY_PATH}"
