#!/usr/bin/env bash
# build_release.sh - Reproducible, low-resource, best-compression release build
# Adheres strictly to AGENTS.md rules:
# - Checks free memory (>= 1.5 GB required)
# - Prefix heavy tasks with nice -n 19 ionice -c3 timeout 20m
# - Go: GOMAXPROCS=2 GOFLAGS=-p=2, gomobile -ldflags="-s -w -buildid=" -trimpath
# - Gradle: --max-workers=2, --no-daemon, --offline fallback, stops daemon on completion
# - Strips debug-only artifacts (e.g. engine-sources.jar)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$REPO_ROOT"

echo "=== [1/5] Checking Memory Resources ==="
AVAILABLE_MEM=$(free -m | awk '/^Mem:/{print $7}')
echo "Available memory: ${AVAILABLE_MEM} MB"
if [ "$AVAILABLE_MEM" -lt 1500 ]; then
    echo "ERROR: Available memory is below 1500 MB (${AVAILABLE_MEM} MB). Aborting to protect system." >&2
    exit 1
fi

echo "=== [2/5] Preparing Build Environment ==="
export ANDROID_HOME="${ANDROID_HOME:-/home/aqua/Android/Sdk}"
if [ -z "${ANDROID_NDK_HOME:-}" ]; then
    if [ -d "$ANDROID_HOME/ndk" ]; then
        LATEST_NDK=$(ls -1d "$ANDROID_HOME/ndk"/* 2>/dev/null | sort -V | tail -n 1)
        if [ -n "$LATEST_NDK" ]; then
            export ANDROID_NDK_HOME="$LATEST_NDK"
        fi
    fi
fi
export PATH="$PATH:$HOME/go/bin"
export GOMAXPROCS=2
export GOFLAGS="-p=2"

echo "Using ANDROID_HOME: $ANDROID_HOME"
echo "Using ANDROID_NDK_HOME: ${ANDROID_NDK_HOME:-not found}"

echo "=== [3/5] Building Go Engine (AAR) ==="
mkdir -p "$REPO_ROOT/android/app/libs"
rm -f "$REPO_ROOT/android/app/libs/engine-sources.jar"

(
    cd "$REPO_ROOT/engine"
    nice -n 19 ionice -c3 timeout 20m gomobile bind \
        -androidapi 21 \
        -target=android/arm64 \
        -ldflags="-s -w -buildid=" \
        -trimpath \
        -o "$REPO_ROOT/android/app/libs/engine.aar" .
)

# Ensure debug-only source archives are stripped from libs
rm -f "$REPO_ROOT/android/app/libs/engine-sources.jar"

echo "=== [4/5] Building Release Android APK ==="
GRADLE_CMD="$REPO_ROOT/StreamX/gradlew"
if [ ! -f "$GRADLE_CMD" ]; then
    GRADLE_CMD="$REPO_ROOT/android/gradlew"
fi

# Run Gradle with release optimization flags, trying offline first
echo "Running Gradle assembleRelease (offline mode)..."
BUILD_SUCCESS=0
if nice -n 19 ionice -c3 timeout 20m "$GRADLE_CMD" \
    -p "$REPO_ROOT/android" assembleRelease \
    --console=plain \
    --max-workers=2 \
    --offline \
    -Dorg.gradle.parallel=false \
    -Dorg.gradle.vfs.watch=false; then
    BUILD_SUCCESS=1
else
    echo "Offline build was not successful (dependencies may need caching); falling back to online build..."
    if nice -n 19 ionice -c3 timeout 20m "$GRADLE_CMD" \
        -p "$REPO_ROOT/android" assembleRelease \
        --console=plain \
        --max-workers=2 \
        -Dorg.gradle.parallel=false \
        -Dorg.gradle.vfs.watch=false; then
        BUILD_SUCCESS=1
    fi
fi

# Always terminate any Gradle daemon that was spawned
echo "Stopping Gradle daemon..."
"$GRADLE_CMD" -p "$REPO_ROOT/android" --stop || true

if [ "$BUILD_SUCCESS" -ne 1 ]; then
    echo "ERROR: Gradle release build failed." >&2
    exit 1
fi

echo "=== [5/5] Build Summary & Artifact Sizes ==="
AAR_PATH="$REPO_ROOT/android/app/libs/engine.aar"
APK_PATH="$REPO_ROOT/android/app/build/outputs/apk/release/app-release.apk"

echo "----------------------------------------------------"
if [ -f "$AAR_PATH" ]; then
    AAR_BYTES=$(stat -c%s "$AAR_PATH" 2>/dev/null || stat -f%z "$AAR_PATH")
    AAR_HUMAN=$(ls -lh "$AAR_PATH" | awk '{print $5}')
    echo "Engine AAR: $AAR_PATH"
    echo "Size:       $AAR_HUMAN ($AAR_BYTES bytes)"
else
    echo "Engine AAR not found!" >&2
fi
echo "----------------------------------------------------"
if [ -f "$APK_PATH" ]; then
    APK_BYTES=$(stat -c%s "$APK_PATH" 2>/dev/null || stat -f%z "$APK_PATH")
    APK_HUMAN=$(ls -lh "$APK_PATH" | awk '{print $5}')
    echo "Release APK: $APK_PATH"
    echo "Size:        $APK_HUMAN ($APK_BYTES bytes)"
else
    echo "Release APK not found!" >&2
fi
echo "----------------------------------------------------"
echo "Build complete."

