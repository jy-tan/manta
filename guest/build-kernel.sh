#!/usr/bin/env bash
set -euo pipefail

# Builds a Firecracker-compatible Linux kernel and copies vmlinux to
# ./guest-artifacts/vmlinux by default.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

KERNEL_VERSION="${KERNEL_VERSION:-linux-6.1.y}"
WORK_DIR="${WORK_DIR:-/tmp/manta-kernel-build}"
OUT_DIR="${OUT_DIR:-${ROOT_DIR}/guest-artifacts}"
LINUX_DIR="${LINUX_DIR:-$WORK_DIR/linux-6.1}"
CONFIG_URL="${CONFIG_URL:-https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config}"
AUTO_INSTALL_DEPS="${AUTO_INSTALL_DEPS:-1}"
JOBS="${JOBS:-}"
KERNEL_BUILD_IN_CONTAINER="${KERNEL_BUILD_IN_CONTAINER:-}"
PREFER_CONTAINER_BUILD="${PREFER_CONTAINER_BUILD:-auto}"

detect_jobs() {
  local cpu_count mem_gib cap_by_mem suggested
  cpu_count="$(nproc)"
  mem_gib="$(awk '/MemTotal/ {print int($2/1024/1024)}' /proc/meminfo 2>/dev/null || echo 2)"
  # Kernel builds can OOM if -j is too high. Keep a conservative default:
  # roughly 2 GiB RAM per concurrent job, max 8 jobs.
  cap_by_mem=$((mem_gib / 2))
  if ((cap_by_mem < 1)); then
    cap_by_mem=1
  fi
  suggested=$cpu_count
  if ((suggested > cap_by_mem)); then
    suggested=$cap_by_mem
  fi
  if ((suggested > 8)); then
    suggested=8
  fi
  echo "$suggested"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

container_runtime() {
  if need_cmd podman; then
    echo "podman"
    return
  fi
  if need_cmd docker; then
    echo "docker"
    return
  fi
  echo ""
}

install_deps() {
  if [[ "${AUTO_INSTALL_DEPS}" != "1" ]]; then
    return
  fi

  if need_cmd dnf; then
    # Dependencies needed by Linux kernel build on Fedora/RHEL family.
    local pkgs=(git curl make gcc flex bison bc perl elfutils-libelf-devel openssl-devel dwarves)
    if [[ "${EUID}" -eq 0 ]]; then
      dnf install -y "${pkgs[@]}"
    elif need_cmd sudo; then
      sudo dnf install -y "${pkgs[@]}"
    fi
  elif need_cmd apt-get; then
    local pkgs=(git curl build-essential flex bison bc libelf-dev libssl-dev dwarves)
    if [[ "${EUID}" -eq 0 ]]; then
      apt-get update
      apt-get install -y "${pkgs[@]}"
    elif need_cmd sudo; then
      sudo apt-get update
      sudo apt-get install -y "${pkgs[@]}"
    fi
  fi
}

find_missing_deps() {
  local missing=()
  local required=(git curl make gcc flex bison bc perl)
  for cmd in "${required[@]}"; do
    if ! need_cmd "$cmd"; then
      missing+=("$cmd")
    fi
  done
  echo "${missing[*]}"
}

run_in_container() {
  local runtime="$1"
  local jobs_for_container="$2"
  echo "running kernel build in container via ${runtime}"

  if [[ "${runtime}" == "podman" ]]; then
    exec podman run --rm \
      -e KERNEL_BUILD_IN_CONTAINER=1 \
      -e KERNEL_VERSION="${KERNEL_VERSION}" \
      -e WORK_DIR="${WORK_DIR}" \
      -e OUT_DIR="${OUT_DIR}" \
      -e LINUX_DIR="${LINUX_DIR}" \
      -e CONFIG_URL="${CONFIG_URL}" \
      -e AUTO_INSTALL_DEPS=1 \
      -e JOBS="${jobs_for_container}" \
      -e PREFER_CONTAINER_BUILD=never \
      -v "${ROOT_DIR}:${ROOT_DIR}:Z" \
      -v "${WORK_DIR}:${WORK_DIR}:Z" \
      -w "${ROOT_DIR}" \
      fedora:43 bash -lc '
        set -euo pipefail
        dnf -y install git curl make gcc flex bison bc perl elfutils-libelf-devel openssl-devel dwarves coreutils diffutils
        '"${SCRIPT_DIR}"'/build-kernel.sh
      '
  fi

  exec docker run --rm \
    -e KERNEL_BUILD_IN_CONTAINER=1 \
    -e KERNEL_VERSION="${KERNEL_VERSION}" \
    -e WORK_DIR="${WORK_DIR}" \
    -e OUT_DIR="${OUT_DIR}" \
    -e LINUX_DIR="${LINUX_DIR}" \
    -e CONFIG_URL="${CONFIG_URL}" \
    -e AUTO_INSTALL_DEPS=1 \
    -e JOBS="${jobs_for_container}" \
    -e PREFER_CONTAINER_BUILD=never \
    -v "${ROOT_DIR}:${ROOT_DIR}" \
    -v "${WORK_DIR}:${WORK_DIR}" \
    -w "${ROOT_DIR}" \
    fedora:43 bash -lc '
      set -euo pipefail
      dnf -y install git curl make gcc flex bison bc perl elfutils-libelf-devel openssl-devel dwarves coreutils diffutils
      '"${SCRIPT_DIR}"'/build-kernel.sh
    '
}

check_deps_or_fallback() {
  local missing=("$@")
  if ((${#missing[@]} == 0)); then
    return
  fi

  local runtime
  runtime="$(container_runtime)"

  if [[ "${KERNEL_BUILD_IN_CONTAINER}" != "1" ]] && [[ "${PREFER_CONTAINER_BUILD}" != "never" ]] && [[ -n "${runtime}" ]]; then
    echo "missing kernel build tools (${missing[*]}), using ${runtime} fallback"
    local jobs_for_container="${JOBS}"
    if [[ -z "${jobs_for_container}" ]]; then
      jobs_for_container=2
    fi
    run_in_container "${runtime}" "${jobs_for_container}"
  fi

  echo "missing required build tools: ${missing[*]}" >&2
  echo "install dependencies or install podman/docker for containerized fallback" >&2
  exit 1
}

if [[ -z "${JOBS}" ]]; then
  JOBS="$(detect_jobs)"
fi

if [[ "${PREFER_CONTAINER_BUILD}" == "always" ]] && [[ "${KERNEL_BUILD_IN_CONTAINER}" != "1" ]]; then
  runtime="$(container_runtime)"
  if [[ -z "${runtime}" ]]; then
    echo "PREFER_CONTAINER_BUILD=always set, but no podman/docker found" >&2
    exit 1
  fi
  run_in_container "${runtime}" "${JOBS}"
fi

mkdir -p "$WORK_DIR" "$OUT_DIR"
install_deps
missing_tools="$(find_missing_deps)"
if [[ -n "${missing_tools}" ]]; then
  # shellcheck disable=SC2206
  missing_arr=(${missing_tools})
  check_deps_or_fallback "${missing_arr[@]}"
fi

if [[ ! -d "$LINUX_DIR/.git" ]]; then
  rm -rf "$LINUX_DIR"
  git clone --depth 1 --branch "$KERNEL_VERSION" \
    git://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git "$LINUX_DIR"
else
  (
    cd "$LINUX_DIR"
    git fetch --depth 1 origin "$KERNEL_VERSION"
    git checkout --detach FETCH_HEAD
  )
fi

pushd "$LINUX_DIR" >/dev/null
curl -fsSL -o .config "$CONFIG_URL"
make olddefconfig
# Ensure the guest can mount and network with Firecracker virtio devices
# without initramfs or runtime modules.
./scripts/config \
  --disable CONFIG_WERROR \
  --disable CONFIG_DEBUG_KERNEL \
  --disable CONFIG_KALLSYMS \
  --disable CONFIG_MODULES \
  --disable CONFIG_BLK_DEV_INITRD \
  --disable CONFIG_ACPI \
  --enable CONFIG_BLK_DEV \
  --enable CONFIG_VIRTIO \
  --enable CONFIG_VIRTIO_BLK \
  --enable CONFIG_VIRTIO_NET \
  --enable CONFIG_VSOCKETS \
  --enable CONFIG_VIRTIO_VSOCKETS \
  --enable CONFIG_VIRTIO_MMIO \
  --enable CONFIG_VIRTIO_MMIO_CMDLINE_DEVICES \
  --enable CONFIG_NET \
  --enable CONFIG_INET \
  --enable CONFIG_DEVTMPFS \
  --enable CONFIG_DEVTMPFS_MOUNT \
  --enable CONFIG_EXT4_FS \
  --enable CONFIG_BINFMT_SCRIPT \
  --set-str CONFIG_DEFAULT_HOSTNAME sandbox
make olddefconfig
echo "building kernel with JOBS=${JOBS}"
if ! make vmlinux -j"${JOBS}"; then
  echo "kernel build failed." >&2
  echo "if you saw a generic 'Makefile: ... Error 2', try lower parallelism: JOBS=2 ./guest/build-kernel.sh" >&2
  echo "if it still fails, rerun and capture full output to a log for the first real error line." >&2
  exit 1
fi
cp -f vmlinux "$OUT_DIR/vmlinux"
popd >/dev/null

echo "kernel built: $OUT_DIR/vmlinux"
