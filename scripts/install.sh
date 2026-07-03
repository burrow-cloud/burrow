#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 Nicholas Phillips
#
# install.sh — the get.burrow.dev bootstrap script (ADR-0044).
#
# Run this on a bare VPS to turn it into a single-node Burrow cluster: it detects the
# platform, downloads and checksum-verifies the `burrow` release binary, installs it to
# /usr/local/bin, then runs `burrow cluster bootstrap` (installing k3s and the control
# plane, and printing the `burrow join <token>` line for your laptop).
#
# Usage (run on the VPS, as root):
#   curl -sfL https://get.burrow.dev | sh
#
# Pass flags through to `burrow cluster bootstrap` (e.g. name the public IP explicitly):
#   curl -sfL https://get.burrow.dev | sh -s -- --public-ip 203.0.113.10
#
# Pin a specific release instead of the latest with the BURROW_VERSION environment
# variable (a release tag, with or without the leading `v`):
#   curl -sfL https://get.burrow.dev | BURROW_VERSION=v0.8.0 sh
#
# This must run on the VPS itself as root (it installs to /usr/local/bin and installs
# k3s, both of which need root). Burrow never SSHes anywhere — you run this over your
# own SSH session. POSIX sh; no bash-only features.

set -eu

# --- constants ---------------------------------------------------------------

REPO="burrow-cloud/burrow"
INSTALL_DIR="/usr/local/bin"
BIN_NAME="burrow"

# --- helpers -----------------------------------------------------------------

# fatal prints an error to stderr and exits non-zero.
fatal() {
	echo "burrow install: error: $*" >&2
	exit 1
}

# info prints a concise progress line to stderr (stdout is reserved for the bootstrap's
# own output, which includes the join token).
info() {
	echo "burrow install: $*" >&2
}

# have reports whether a command exists.
have() {
	command -v "$1" >/dev/null 2>&1
}

# --- preflight: root ---------------------------------------------------------

require_root() {
	# id -u is POSIX and the portable way to check for the superuser.
	if [ "$(id -u)" -eq 0 ]; then
		return
	fi
	if have sudo; then
		fatal "this installer must run as root. Re-run it under sudo:
    curl -sfL https://get.burrow.dev | sudo sh"
	fi
	fatal "this installer must run as root (it installs to ${INSTALL_DIR} and installs k3s).
    Log in as root and run it again."
}

# --- preflight: tools --------------------------------------------------------

require_tools() {
	have curl || fatal "curl is required but was not found; install curl and retry."
	have tar || fatal "tar is required but was not found; install tar and retry."
	# A sha256 tool is needed to verify the download; both common names are accepted.
	if have sha256sum || have shasum; then
		return
	fi
	fatal "a sha256 tool is required (sha256sum or shasum); install coreutils and retry."
}

# --- platform detection ------------------------------------------------------

# detect_platform maps uname's OS/arch onto the goreleaser asset names. The release
# archives are named burrow_<version>_<os>_<arch>.tar.gz with os in {linux,darwin} and
# arch in {amd64,arm64}. A VPS bootstrap only needs linux/amd64 and linux/arm64; the
# supported set is stated in the error so an unsupported box fails clearly.
detect_platform() {
	os_raw="$(uname -s)"
	arch_raw="$(uname -m)"

	case "${os_raw}" in
	Linux) OS="linux" ;;
	Darwin) OS="darwin" ;;
	*) fatal "unsupported OS '${os_raw}'. Burrow bootstrap supports Linux (amd64 or arm64)." ;;
	esac

	case "${arch_raw}" in
	x86_64 | amd64) ARCH="amd64" ;;
	aarch64 | arm64) ARCH="arm64" ;;
	*) fatal "unsupported architecture '${arch_raw}'. Burrow bootstrap supports amd64 (x86_64) and arm64 (aarch64)." ;;
	esac

	# The bootstrap installs k3s, which is Linux-only. darwin binaries exist for the
	# laptop-side CLI but cannot host a cluster, so stop early with a clear message.
	if [ "${OS}" != "linux" ]; then
		fatal "the Burrow VPS bootstrap runs on Linux; '${os_raw}' cannot host a k3s cluster.
    Install the CLI on macOS with 'brew install burrow-cloud/tap/burrow' instead."
	fi
}

# --- version resolution ------------------------------------------------------

# resolve_version sets TAG (e.g. v0.8.0, used in the download path) and VERSION (e.g.
# 0.8.0, used in the archive filename). BURROW_VERSION overrides the default, which is
# the latest release resolved by following the releases/latest redirect (no JSON parsing
# and no API token needed).
resolve_version() {
	req="${BURROW_VERSION:-}"
	if [ -n "${req}" ]; then
		TAG="${req}"
	else
		info "Resolving the latest release..."
		# releases/latest redirects to releases/tag/<tag>; the effective URL carries the tag.
		effective="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
		TAG="${effective##*/tag/}"
		case "${TAG}" in
		"" | *"/"* | http*)
			fatal "could not resolve the latest release. Set BURROW_VERSION to a release tag (e.g. v0.8.0) and retry."
			;;
		esac
	fi

	# Normalize: TAG keeps a leading 'v' for the download path; VERSION drops it for the
	# archive filename (goreleaser names archives with the bare version).
	case "${TAG}" in
	v*) VERSION="${TAG#v}" ;;
	*)
		VERSION="${TAG}"
		TAG="v${TAG}"
		;;
	esac
}

# --- download and verify -----------------------------------------------------

# sha256_of prints the hex sha256 of a file using whichever tool is present.
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

# download_and_verify downloads the platform archive and the checksums file into TMPDIR,
# verifies the archive's sha256 against the checksums file, and extracts the burrow
# binary to ${TMPDIR}/burrow. Any mismatch is fatal.
download_and_verify() {
	archive="${BIN_NAME}_${VERSION}_${OS}_${ARCH}.tar.gz"
	base="https://github.com/${REPO}/releases/download/${TAG}"

	info "Downloading ${BIN_NAME} ${TAG} (${OS}/${ARCH})..."
	curl -fsSL -o "${TMPDIR}/${archive}" "${base}/${archive}" ||
		fatal "downloading ${archive} from ${base} failed. Check that release ${TAG} exists for ${OS}/${ARCH}."
	curl -fsSL -o "${TMPDIR}/checksums.txt" "${base}/checksums.txt" ||
		fatal "downloading checksums.txt from ${base} failed."

	info "Verifying checksum..."
	# The checksums file lists "<sha256>  <filename>" lines for every asset; pull the
	# line for our archive and compare against the locally computed digest.
	expected="$(grep " ${archive}\$" "${TMPDIR}/checksums.txt" | awk '{print $1}')"
	[ -n "${expected}" ] || fatal "checksums.txt has no entry for ${archive}; refusing to install unverified."
	actual="$(sha256_of "${TMPDIR}/${archive}")"
	if [ "${expected}" != "${actual}" ]; then
		fatal "checksum mismatch for ${archive}:
    expected ${expected}
    actual   ${actual}
    Refusing to install a tampered or corrupt download."
	fi

	info "Extracting ${BIN_NAME}..."
	# The archive bundles burrow and burrow-mcp; extract only the burrow binary.
	tar -xzf "${TMPDIR}/${archive}" -C "${TMPDIR}" "${BIN_NAME}" ||
		fatal "extracting ${BIN_NAME} from ${archive} failed."
	[ -f "${TMPDIR}/${BIN_NAME}" ] || fatal "${BIN_NAME} binary not found in ${archive}."
}

# --- install -----------------------------------------------------------------

# install_binary places the verified binary at ${INSTALL_DIR}/${BIN_NAME}, atomically:
# it copies into a temp file on the same filesystem, makes it executable, then renames it
# over any existing binary so a concurrent invocation never sees a half-written file.
install_binary() {
	info "Installing ${BIN_NAME} to ${INSTALL_DIR}/${BIN_NAME}..."
	mkdir -p "${INSTALL_DIR}" || fatal "creating ${INSTALL_DIR} failed."
	tmp_target="${INSTALL_DIR}/.${BIN_NAME}.install.$$"
	cp "${TMPDIR}/${BIN_NAME}" "${tmp_target}" || fatal "copying the binary into ${INSTALL_DIR} failed."
	chmod 0755 "${tmp_target}" || fatal "setting permissions on the binary failed."
	mv -f "${tmp_target}" "${INSTALL_DIR}/${BIN_NAME}" || fatal "installing the binary into ${INSTALL_DIR} failed."
}

# --- main --------------------------------------------------------------------

main() {
	require_root
	require_tools
	detect_platform
	resolve_version

	# A temp dir for the download, cleaned on exit (success or failure).
	TMPDIR="$(mktemp -d 2>/dev/null || mktemp -d -t burrow-install)"
	trap 'rm -rf "${TMPDIR}"' EXIT INT TERM

	download_and_verify
	install_binary

	# The binary is installed; the download temp dir is no longer needed. Clean it now and
	# clear the trap: exec replaces this process, so the EXIT trap would not otherwise fire.
	rm -rf "${TMPDIR}"
	trap - EXIT INT TERM

	info "Bootstrapping..."
	# Hand off to the CLI: run the on-VPS bootstrap, passing through every script arg
	# (e.g. --public-ip) so the flags flow to `burrow cluster bootstrap`. exec replaces
	# this shell so the bootstrap's stdout (including the join token) is the user's.
	exec "${INSTALL_DIR}/${BIN_NAME}" cluster bootstrap "$@"
}

main "$@"
