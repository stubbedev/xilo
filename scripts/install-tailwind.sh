#!/usr/bin/env bash
set -euo pipefail

version=v4.3.2

die() {
	echo "install-tailwind: $*" >&2
	exit 1
}

[[ $# -eq 1 ]] || die "usage: install-tailwind.sh <destination>"
dest=$1

machine=$(uname -m)
case "$machine" in
x86_64) arch=x64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "no pinned Tailwind CLI for $machine" ;;
esac

case "$(uname -s)" in
Linux)
	libc=
	for loader in /lib/ld-musl-*.so.1; do
		[[ -e $loader ]] && libc=-musl
	done
	asset=tailwindcss-linux-$arch$libc
	;;
Darwin) asset=tailwindcss-macos-$arch ;;
*) die "no pinned Tailwind CLI for $(uname -s)" ;;
esac

case "$asset" in
tailwindcss-linux-x64) want=5036c4fb4328e0bcdbb6065c70d8ac9452e0d4c947113a788a8f94fd390425c1 ;;
tailwindcss-linux-x64-musl) want=ae828e9e989ecbddb2bef856af8b0308ba162583b4922b3a065b5e26f86b0691 ;;
tailwindcss-linux-arm64) want=394ddccc2402cfa3abd97dfba56f3587781a3d6e6ce66e65ceada14beb7664b8 ;;
tailwindcss-linux-arm64-musl) want=24a0dd39cbbced9d94f6313a747cc29ab2523a6a7b69204f2151e0af6aad6eef ;;
tailwindcss-macos-x64) want=cef8f110471e889c3c4409055cf8aff33076f58a081867b0dfc6534b290bfbb0 ;;
tailwindcss-macos-arm64) want=b800b0659dc64b9f03ede5660244d9415d777d5739ae2889280877ca37be742a ;;
*) die "no pinned checksum for $asset" ;;
esac
url=https://github.com/tailwindlabs/tailwindcss/releases/download/$version/$asset

sha256() {
	if command -v sha256sum >/dev/null; then
		sha256sum "$1"
	else
		shasum -a 256 "$1"
	fi
}

tmp=$(mktemp "$dest.XXXXXX")
trap 'rm -f "$tmp"' EXIT

curl -fsSL --proto '=https' --connect-timeout 30 --max-time 300 -o "$tmp" "$url" ||
	die "download of $url failed"

got=$(sha256 "$tmp")
got=${got%% *}
[[ $got == "$want" ]] || die "$url has sha256 $got, expected $want"

chmod 0755 "$tmp"
mv -f "$tmp" "$dest"
trap - EXIT
echo "install-tailwind: installed $asset $version to $dest"
