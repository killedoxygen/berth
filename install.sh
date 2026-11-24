#!/bin/sh
# Install berth: download the release binary for this machine, verify it, run `berth setup`.
#
#   curl -fsSL https://raw.githubusercontent.com/killedoxygen/berth/main/install.sh | sh
#   curl -fsSL .../install.sh | sh -s -- --system        # system-wide (uses sudo)
#   curl -fsSL .../install.sh | sh -s -- --no-setup      # only install the binary
#
# Environment: BERTH_VERSION (default: latest), BERTH_BIN_DIR (default ~/.local/bin, or /usr/local/bin with --system).
set -eu

repo="killedoxygen/berth"
system=0
setup=1
for arg in "$@"; do
	case "$arg" in
	--system) system=1 ;;
	--no-setup) setup=0 ;;
	-h | --help)
		sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "berth install: unknown option $arg" >&2
		exit 2
		;;
	esac
done

say() { printf 'berth install: %s\n' "$*" >&2; }
die() {
	say "$*"
	exit 1
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in linux | darwin) ;; *) die "unsupported OS $os (Linux and macOS are supported)" ;; esac
arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $arch" ;;
esac

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	die "need curl or wget"
fi

version=${BERTH_VERSION:-latest}
if [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi
asset="berth_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
say "downloading $asset ($version)"
fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "download failed: checksums.txt"
expected=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1)
[ -n "$expected" ] || die "no checksum for $asset"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$asset" | cut -d' ' -f1)
else
	actual=$(shasum -a 256 "$tmp/$asset" | cut -d' ' -f1)
fi
[ "$expected" = "$actual" ] || die "checksum mismatch for $asset"
tar -xzf "$tmp/$asset" -C "$tmp" berth

if [ "$system" = 1 ]; then
	dir=${BERTH_BIN_DIR:-/usr/local/bin}
	sudo=""
	[ "$(id -u)" = 0 ] || sudo="sudo"
	$sudo install -m 0755 "$tmp/berth" "$dir/berth"
else
	dir=${BERTH_BIN_DIR:-$HOME/.local/bin}
	mkdir -p "$dir"
	install -m 0755 "$tmp/berth" "$dir/berth"
fi
say "installed $dir/berth ($("$dir/berth" version))"

if [ "$setup" = 1 ]; then
	if [ "$system" = 1 ]; then
		$sudo "$dir/berth" setup --system
	else
		"$dir/berth" setup
	fi
else
	say "run \`$dir/berth setup\` to finish"
fi
