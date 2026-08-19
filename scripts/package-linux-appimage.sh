#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  printf 'usage: %s VERSION SOURCE_DATE_EPOCH BINARY OUTPUT\n' "$0" >&2
  exit 2
fi

readonly version="${1#v}"
readonly source_date_epoch="$2"
readonly binary="$3"
readonly output="$4"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(dirname "$script_dir")"
readonly repo_root

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  printf 'Launcher version must be semantic versioning: %s\n' "$version" >&2
  exit 2
fi
if [[ ! "$source_date_epoch" =~ ^[0-9]+$ ]] || [[ "$source_date_epoch" == 0 ]]; then
  printf 'SOURCE_DATE_EPOCH must be a positive Unix timestamp\n' >&2
  exit 2
fi
if [[ ! -x "$binary" ]]; then
  printf 'Linux Launcher binary is missing or not executable: %s\n' "$binary" >&2
  exit 2
fi
if [[ "$(uname -s)-$(uname -m)" != "Linux-x86_64" ]]; then
  printf 'Linux AppImage packaging requires a Linux x86_64 host\n' >&2
  exit 2
fi

for command in curl file find install ldd patchelf sha256sum; do
  command -v "$command" >/dev/null || {
    printf '%s is required to package the Linux Launcher\n' "$command" >&2
    exit 2
  }
done

work_root="$(mktemp -d "${TMPDIR:-/tmp}/cineko-launcher-appimage.XXXXXX")"
readonly work_root
trap 'rm -rf "$work_root"' EXIT
readonly tools_dir="$work_root/tools"
readonly app_dir="$work_root/CinekoLauncher.AppDir"
readonly verify_dir="$work_root/verify"
mkdir -p "$tools_dir" "$app_dir/usr/bin" "$verify_dir" "$(dirname "$output")"

download_tool() {
  local url="$1"
  local sha256="$2"
  local destination="$3"
  curl --fail --silent --show-error --location --retry 5 --retry-all-errors \
    --output "$destination" "$url"
  printf '%s  %s\n' "$sha256" "$destination" | sha256sum --check --status || {
    printf 'tool checksum mismatch: %s\n' "$url" >&2
    return 1
  }
  chmod 0755 "$destination"
}

readonly linuxdeploy="$tools_dir/linuxdeploy-x86_64.AppImage"
readonly gtk_plugin="$tools_dir/linuxdeploy-plugin-gtk.sh"
readonly appimagetool="$tools_dir/appimagetool-x86_64.AppImage"
readonly appimage_runtime="$tools_dir/runtime-x86_64"

download_tool \
  'https://github.com/linuxdeploy/linuxdeploy/releases/download/1-alpha-20251107-1/linuxdeploy-x86_64.AppImage' \
  'c20cd71e3a4e3b80c3483cef793cda3f4e990aca14014d23c544ca3ce1270b4d' \
  "$linuxdeploy"
download_tool \
  'https://raw.githubusercontent.com/linuxdeploy/linuxdeploy-plugin-gtk/7a3fbc31a9e5075073ff8790f26effbac5f84453/linuxdeploy-plugin-gtk.sh' \
  'b0f4cbc684a0103a9651f0955b635eaea0096b3a66c0f5a2c2aa337960375171' \
  "$gtk_plugin"
download_tool \
  'https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-x86_64.AppImage' \
  'ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0' \
  "$appimagetool"
download_tool \
  'https://github.com/AppImage/type2-runtime/releases/download/20251108/runtime-x86_64' \
  '2fca8b443c92510f1483a883f60061ad09b46b978b2631c807cd873a47ec260d' \
  "$appimage_runtime"

install -Dm755 "$binary" "$app_dir/usr/bin/cineko-launcher"
install -Dm644 "$repo_root/build/linux/cineko-launcher.desktop" \
  "$app_dir/usr/share/applications/cineko-launcher.desktop"
install -Dm644 "$repo_root/build/appicon.svg" \
  "$app_dir/usr/share/icons/hicolor/scalable/apps/cineko-launcher.svg"

readonly webkit_dir='/usr/lib/x86_64-linux-gnu/webkit2gtk-4.1'
readonly webkit_helpers=(
  WebKitNetworkProcess
  WebKitWebProcess
  injected-bundle/libwebkit2gtkinjectedbundle.so
)
deploy_dependencies=()
for relative_path in "${webkit_helpers[@]}"; do
  source_path="$webkit_dir/$relative_path"
  if [[ ! -f "$source_path" ]]; then
    printf 'required WebKitGTK runtime helper is missing: %s\n' "$source_path" >&2
    exit 1
  fi
  destination="$app_dir$source_path"
  mkdir -p "$(dirname "$destination")"
  cp --archive "$source_path" "$destination"
  deploy_dependencies+=("--deploy-deps-only=$destination")
done
if [[ -f "$webkit_dir/WebKitGPUProcess" ]]; then
  destination="$app_dir$webkit_dir/WebKitGPUProcess"
  cp --archive "$webkit_dir/WebKitGPUProcess" "$destination"
  deploy_dependencies+=("--deploy-deps-only=$destination")
fi

APPIMAGE_EXTRACT_AND_RUN=1 NO_STRIP=1 "$linuxdeploy" \
  --appimage-extract-and-run \
  --appdir "$app_dir" \
  --executable "$app_dir/usr/bin/cineko-launcher" \
  --desktop-file "$app_dir/usr/share/applications/cineko-launcher.desktop" \
  --icon-file "$app_dir/usr/share/icons/hicolor/scalable/apps/cineko-launcher.svg" \
  "${deploy_dependencies[@]}" \
  --plugin gtk

install -Dm755 "$repo_root/build/linux/AppRun" "$app_dir/AppRun"
ln -sfn usr/share/applications/cineko-launcher.desktop "$app_dir/cineko-launcher.desktop"
ln -sfn usr/share/icons/hicolor/scalable/apps/cineko-launcher.svg "$app_dir/cineko-launcher.svg"
ln -sfn cineko-launcher.svg "$app_dir/.DirIcon"

find "$app_dir" -depth -exec touch --no-dereference --date="@$source_date_epoch" {} +
SOURCE_DATE_EPOCH="$source_date_epoch" ARCH=x86_64 APPIMAGE_EXTRACT_AND_RUN=1 \
  "$appimagetool" --appimage-extract-and-run \
  --runtime-file "$appimage_runtime" \
  --comp zstd \
  --no-appstream \
  "$app_dir" "$output"

if [[ ! -x "$output" ]]; then
  printf 'AppImage output is missing or not executable: %s\n' "$output" >&2
  exit 1
fi
magic="$(dd if="$output" bs=1 skip=8 count=3 status=none | od -An -tx1 | tr -d ' \n')"
if [[ "$magic" != '414902' ]]; then
  printf 'output is not a type-2 AppImage: %s\n' "$output" >&2
  exit 1
fi

output_path="$(realpath "$output")"
(cd "$verify_dir" && "$output_path" --appimage-extract >/dev/null)
readonly extracted="$verify_dir/squashfs-root"
for required_path in \
  AppRun \
  usr/bin/cineko-launcher \
  usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/WebKitNetworkProcess \
  usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/WebKitWebProcess \
  usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/injected-bundle/libwebkit2gtkinjectedbundle.so; do
  if [[ ! -e "$extracted/$required_path" ]]; then
    printf 'AppImage is missing required runtime file: %s\n' "$required_path" >&2
    exit 1
  fi
done
for library in 'libgtk-3.so*' 'libwebkit2gtk-4.1.so*' 'libjavascriptcoregtk-4.1.so*'; do
  if ! find "$extracted/usr/lib" -name "$library" -print -quit | grep -q .; then
    printf 'AppImage is missing required runtime library: %s\n' "$library" >&2
    exit 1
  fi
done

readonly library_path="$extracted/usr/lib:$extracted/usr/lib/x86_64-linux-gnu"
for executable_path in \
  usr/bin/cineko-launcher \
  usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/WebKitNetworkProcess \
  usr/lib/x86_64-linux-gnu/webkit2gtk-4.1/WebKitWebProcess; do
  if ! dependency_report="$(LD_LIBRARY_PATH="$library_path" ldd "$extracted/$executable_path")"; then
    printf 'could not inspect AppImage runtime dependencies: %s\n' "$executable_path" >&2
    exit 1
  fi
  missing="$(grep 'not found' <<< "$dependency_report" || true)"
  if [[ -n "$missing" ]]; then
    printf 'AppImage runtime dependency is missing for %s:\n%s\n' "$executable_path" "$missing" >&2
    exit 1
  fi
done

printf 'packaged and verified Linux AppImage: %s\n' "$output"
