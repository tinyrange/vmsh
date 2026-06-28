#!/bin/sh
set -eu

repo="${VMSH_REPO:-tinyrange/vmsh}"
version="${VMSH_VERSION:-latest}"

say() {
	printf '%s\n' "$*" >&2
}

die() {
	say "vmsh install: $*"
	exit 1
}

if [ -n "${VMSH_INSTALL_DIR:-}" ]; then
	install_dir="$VMSH_INSTALL_DIR"
else
	home="${HOME:-}"
	if [ -z "$home" ]; then
		die "HOME is not set; set VMSH_INSTALL_DIR explicitly"
	fi
	install_dir="$home/.local/bin"
fi

has() {
	command -v "$1" >/dev/null 2>&1
}

download_to() {
	url="$1"
	dst="$2"
	if has curl; then
		curl -fL --retry 3 --proto '=https' --tlsv1.2 -o "$dst" "$url"
	elif has wget; then
		wget -O "$dst" "$url"
	else
		die "curl or wget is required"
	fi
}

fetch_text() {
	url="$1"
	if has curl; then
		curl -fsSL --retry 3 --proto '=https' --tlsv1.2 "$url"
	elif has wget; then
		wget -qO- "$url"
	else
		die "curl or wget is required"
	fi
}

sha256_file() {
	file="$1"
	if has sha256sum; then
		sha256sum "$file" | awk '{ print $1 }'
	elif has shasum; then
		shasum -a 256 "$file" | awk '{ print $1 }'
	else
		die "sha256sum or shasum is required for checksum verification"
	fi
}

detect_os() {
	case "$(uname -s)" in
		Linux) printf 'linux' ;;
		Darwin) printf 'darwin' ;;
		*) die "unsupported OS: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64 | amd64) printf 'amd64' ;;
		arm64 | aarch64) printf 'arm64' ;;
		*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

latest_version() {
	tag="$(fetch_text "https://api.github.com/repos/${repo}/releases/latest" |
		sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' |
		head -n 1)"
	if [ -z "$tag" ]; then
		die "could not determine latest release for ${repo}"
	fi
	printf '%s' "$tag"
}

os_name="${VMSH_OS:-$(detect_os)}"
arch="${VMSH_ARCH:-$(detect_arch)}"

case "${os_name}/${arch}" in
	linux/amd64 | linux/arm64 | darwin/arm64) ;;
	darwin/amd64) die "macOS amd64 is not published; supported targets are darwin/arm64, linux/arm64, linux/amd64" ;;
	*) die "unsupported target ${os_name}/${arch}; supported targets are darwin/arm64, linux/arm64, linux/amd64" ;;
esac

if [ "$version" = "latest" ]; then
	version="$(latest_version)"
fi

asset="vmsh_${version}_${os_name}_${arch}"
base_url="https://github.com/${repo}/releases/download/${version}"

if has mktemp; then
	tmp="$(mktemp -d "${TMPDIR:-/tmp}/vmsh-install.XXXXXX")"
else
	tmp="${TMPDIR:-/tmp}/vmsh-install.$$"
	mkdir -p "$tmp"
fi
tmp_target=""
cleanup() {
	rm -rf "$tmp"
	if [ -n "$tmp_target" ]; then
		rm -f "$tmp_target"
	fi
}
trap cleanup EXIT INT HUP TERM

binary="$tmp/$asset"
checksums="$tmp/checksums.txt"

say "Downloading ${asset}"
download_to "${base_url}/${asset}" "$binary"

say "Verifying checksum"
download_to "${base_url}/checksums.txt" "$checksums"
expected="$(awk -v name="$asset" '$2 == name { print $1 }' "$checksums" | head -n 1)"
if [ -z "$expected" ]; then
	die "checksums.txt does not contain ${asset}"
fi
actual="$(sha256_file "$binary")"
if [ "$actual" != "$expected" ]; then
	die "checksum mismatch for ${asset}"
fi

mkdir -p "$install_dir"
target="$install_dir/vmsh"
tmp_target="${target}.tmp.$$"
cp "$binary" "$tmp_target"
chmod 755 "$tmp_target"
mv "$tmp_target" "$target"

say "Installed vmsh ${version} to ${target}"
say "Make sure ${install_dir} is on your PATH."
