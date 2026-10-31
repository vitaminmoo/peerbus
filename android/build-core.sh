#!/bin/sh
# Builds app/libs/peerbus.aar from ../mobile with gomobile. Run from
# anywhere; Gradle runs it before compiling (see app/build.gradle.kts).
set -eu
cd "$(dirname "$0")"
: "${ANDROID_HOME:=$HOME/Android/Sdk}"
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
	ANDROID_NDK_HOME=$(ls -d "$ANDROID_HOME"/ndk/* | sort -V | tail -1)
fi
export ANDROID_HOME ANDROID_NDK_HOME
# gomobile builds from a temporary directory outside the repository.
export GOFLAGS="${GOFLAGS:-} -buildvcs=false"
# gomobile runs gobind from PATH; put the pinned one there.
export PATH="$(dirname "$(go tool -n gobind)"):$PATH"
mkdir -p app/libs
# gomobile and gobind are pinned as tool dependencies in ../go.mod.
go tool gomobile bind -target=android/arm64,android/amd64 -androidapi 26 -javapkg=us.peerb \
	-ldflags='-s -w' -o app/libs/peerbus.aar ../mobile
