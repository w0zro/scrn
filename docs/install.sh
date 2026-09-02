#!/bin/sh
# Install scrn: fetch the build made for this machine and put it on the path.
#
#   curl -fsSL https://scrn.w0zro.com/install.sh | sh
#
# It lives in docs/ because that is what scrn.w0zro.com serves: one script,
# one address, for macOS and Linux alike.
#
# SCRN_VERSION pins a release instead of taking the latest. SCRN_INSTALL_DIR
# says where the binary goes.
set -eu

repo=w0zro/scrn
dir="${SCRN_INSTALL_DIR:-$HOME/.local/bin}"

die() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || die "$1 is needed and was not found"
}

# platform names the release built for this machine, in the os_arch form the
# assets are named with.
platform() {
	os=$(uname -s)
	case "$os" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "there is no scrn build for $os" ;;
	esac

	arch=$(uname -m)
	case "$arch" in
	arm64 | aarch64) arch=arm64 ;;
	x86_64 | amd64) arch=amd64 ;;
	*) die "there is no scrn build for $arch" ;;
	esac

	printf '%s_%s\n' "$os" "$arch"
}

# latest asks which release is current by following the redirect that
# /releases/latest answers with, which names the tag. It is a plain request to
# github.com rather than the API, so it does not spend an API rate limit.
latest() {
	# Deliberately not -f: a repo with no releases answers 404, and letting
	# curl fail on it would report not reaching github when github answered
	# plainly. A request that never arrives still fails, and is told apart
	# from an answer of "nothing here" by the tag the redirect landed on.
	url=$(curl -sSLI -o /dev/null -w '%{url_effective}' \
		"https://github.com/$repo/releases/latest") ||
		die "could not reach github to ask for the latest release"
	tag=${url##*/}
	[ "$tag" != latest ] ||
		die "$repo has no releases yet; name one with SCRN_VERSION"
	printf '%s\n' "$tag"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "neither sha256sum nor shasum is here to check the download with"
	fi
}

need curl
need tar

target=$(platform)
tag=${SCRN_VERSION:-$(latest)}
case "$tag" in v*) ;; *) tag="v$tag" ;; esac
version=${tag#v}

asset="scrn_${version}_${target}.tar.gz"
base="https://github.com/$repo/releases/download/$tag"

# Staged beside where the binary will live, not in /tmp: the move into place
# below is a rename only within one filesystem, and /tmp is often another.
mkdir -p "$dir" || die "could not make $dir"
tmp=$(mktemp -d "$dir/.scrn-install.XXXXXX")
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'downloading scrn %s for %s\n' "$version" "$target"
curl -fsSL -o "$tmp/$asset" "$base/$asset" ||
	die "no $asset in release $tag"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" ||
	die "release $tag has no checksums.txt to check the download against"

want=$(grep " $asset\$" "$tmp/checksums.txt" | cut -d' ' -f1) ||
	die "checksums.txt does not mention $asset"
got=$(sha256 "$tmp/$asset")
[ -n "$want" ] || die "checksums.txt does not mention $asset"
[ "$want" = "$got" ] || die "$asset did not arrive intact: expected $want, got $got"

tar -xzf "$tmp/$asset" -C "$tmp"
[ -f "$tmp/scrn" ] || die "$asset does not hold a scrn binary"

chmod 755 "$tmp/scrn"

# Renamed into place rather than written over, so that a scrn already running
# from this path keeps the binary it started with. A rename only, because the
# staging directory is on the same filesystem as the destination.
mv -f "$tmp/scrn" "$dir/scrn" ||
	die "could not put scrn in $dir"

printf 'installed scrn %s to %s/scrn\n' "$version" "$dir"

# The manpage goes beside the binary in the way man expects: for a binary in
# ~/.local/bin, man derives ~/.local/share/man from PATH on its own, so
# `man scrn` works with nothing configured. Releases from before the manpage
# simply do not have one in the archive, and that is not a failure.
if [ -f "$tmp/scrn.1" ]; then
	mandir="${SCRN_MAN_DIR:-${dir%/bin}/share/man}"
	if mkdir -p "$mandir/man1" && mv -f "$tmp/scrn.1" "$mandir/man1/scrn.1"; then
		printf 'installed scrn.1 to %s/man1\n' "$mandir"
	else
		printf 'could not install the manpage to %s/man1; the binary is unaffected\n' "$mandir" >&2
	fi
fi

case ":$PATH:" in
*":$dir:"*) ;;
*)
	# shellcheck disable=SC2016 # $PATH is meant to reach the reader unexpanded
	printf '\n%s is not on your PATH. Add it:\n\n    export PATH="%s:$PATH"\n\n' \
		"$dir" "$dir"
	;;
esac
