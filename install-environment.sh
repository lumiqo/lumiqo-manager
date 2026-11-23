#!/bin/bash
set -euo pipefail
umask 022

root=$1
inputs=$2
revision=$3
arch=$4
analysis_smoke=$5
shift 5

uv_version=0.13.9
mise_version=2026.10.24
duckdb_version=1.4.5
node_version=24.22.3
python_version=3.13.18

case "$arch" in
    amd64)
        uv_arch=x86_64
        mise_arch=x64
        uv_sha=a7fbaff2b9a271d85c1bdb540941090fd1ce011797b4abeb48e4dcd39c2471d5
        mise_sha=9a6c9c55f5c6372d0af70a9979fe6d742aea3793267a929e1738022143c466cc
        duckdb_sha=ff4ef9ec59fe3e1a1f3dd1004c6218d1fd59c0533c185c968c4403fd0240d02b
        ;;
    arm64)
        uv_arch=aarch64
        mise_arch=arm64
        uv_sha=0797542018e541a9288855df555c8077eed06f7a47d206362030480e8ecc9f3d
        mise_sha=8c8f5d7f748bf3aef431dc50f0ad776270f36025c788aa21c74e2fabdbbd6d3e
        duckdb_sha=c6d1c19631bb4d7a2a5dcf30586d888e167ce6fb22396060110c7a32e2bfc298
        ;;
    *) echo "Unsupported environment architecture: $arch" >&2; exit 1 ;;
esac

release="$root/versions/$revision"
mkdir -p "$root/versions"
staging=$(mktemp -d "$root/.setup-XXXXXX")
cleanup() {
    rm -rf -- "$staging"
    if [[ ! -f "$release/.complete" ]]; then
        rm -rf -- "$release"
    fi
}
trap cleanup EXIT

download() {
    curl --fail --silent --show-error --location --retry 2 --connect-timeout 15 --max-time 180 \
        --output "$staging/archive" "$1"
    printf '%s  %s\n' "$2" "$staging/archive" | sha256sum --check --status || {
        echo "SHA-256 mismatch for $1" >&2
        return 1
    }
}

check_managed_tools() (
    cd /
    export PATH="$release/bin:/usr/sbin:/usr/bin:/sbin:/bin"
    for name in uv mise node npm npx duckdb python-analysis fd bat; do
        [[ -x "$release/bin/$name" ]] || return 1
    done
    "$release/bin/uv" --version && "$release/bin/mise" --version &&
    runuser -u lumiqo -- env -i PATH="$PATH" HOME=/home/lumiqo LANG=C.UTF-8 node --version &&
    runuser -u lumiqo -- env -i PATH="$PATH" HOME=/home/lumiqo LANG=C.UTF-8 npm --version &&
    runuser -u lumiqo -- env -i PATH="$PATH" HOME=/home/lumiqo LANG=C.UTF-8 npx --version &&
    runuser -u lumiqo -- env -i PATH="$PATH" HOME=/home/lumiqo LANG=C.UTF-8 duckdb -csv -c 'SELECT 1' &&
    runuser -u lumiqo -- env -i PATH="$PATH" HOME=/home/lumiqo LANG=C.UTF-8 python-analysis -I -B -c "$analysis_smoke"
)

if [[ -f "$release/.complete" ]] && ! check_managed_tools; then
    # Only the versioned Manager payload is rebuilt. User files live outside it.
    rm -f -- "$release/.complete"
fi

if [[ ! -f "$release/.complete" ]]; then
    rm -rf -- "$release"
    mkdir -p "$release/bin" "$staging/home"
    export HOME="$staging/home"
    export MISE_DATA_DIR="$release/mise"
    export MISE_NODE_VERIFY=true
    export UV_PYTHON_INSTALL_DIR="$release/python"
    export UV_CACHE_DIR="$staging/uv-cache"
    export UV_LINK_MODE=copy
    export UV_NO_PROGRESS=1

    uv_target="$uv_arch-unknown-linux-gnu"
    download "https://github.com/astral-sh/uv/releases/download/$uv_version/uv-$uv_target.tar.gz" "$uv_sha"
    tar -xOzf "$staging/archive" "uv-$uv_target/uv" > "$release/bin/uv"
    download "https://github.com/jdx/mise/releases/download/v$mise_version/mise-v$mise_version-linux-$mise_arch.tar.gz" "$mise_sha"
    tar -xOzf "$staging/archive" mise/bin/mise > "$release/bin/mise"
    download "https://github.com/duckdb/duckdb/releases/download/v$duckdb_version/duckdb_cli-linux-$arch.zip" "$duckdb_sha"
    unzip -p "$staging/archive" duckdb > "$release/bin/duckdb"
    chmod 0755 "$release/bin/"{uv,mise,duckdb}

    # Keep mise configuration and caches independent of the user's projects.
    cd "$staging"
    "$release/bin/mise" install "node@$node_version"
    node_dir=$("$release/bin/mise" where "node@$node_version")
    ln -s "$node_dir/bin/node" "$release/bin/node"
    # npm and npx locate their bundled modules relative to the directory they run from.
    for name in npm npx; do
        printf '#!/bin/sh\nexport PATH="%s/bin:$PATH"\nexec "%s/bin/%s" "$@"\n' \
            "$node_dir" "$node_dir" "$name" > "$release/bin/$name"
        chmod 0755 "$release/bin/$name"
    done

    UV_PROJECT_ENVIRONMENT="$release/analysis" "$release/bin/uv" sync \
        --project "$inputs" --locked --managed-python --python "$python_version" --no-build
    printf '#!/bin/sh\nexec "%s/analysis/bin/python" "$@"\n' "$release" > "$release/bin/python-analysis"
    chmod 0755 "$release/bin/python-analysis"
    ln -s /usr/bin/fdfind "$release/bin/fd"
    ln -s /usr/bin/batcat "$release/bin/bat"
fi

cd /
export PATH="$release/bin:/usr/sbin:/usr/bin:/sbin:/bin"
for name in git curl wget cc rg fd bat tree jq yq shellcheck file unzip 7z zstd tar rsync \
    sqlite3 mlr csvcut duckdb pandoc pdftotext pdfinfo exiftool ffmpeg convert gh git-lfs python3 uv mise node npm npx; do
    command -v "$name" >/dev/null || { echo "Required command missing: $name" >&2; exit 1; }
done
check_managed_tools

{
    printf '# Lumiqot CLI environment\n\n'
    printf 'Managed command path\n\n    %s/current/bin\n\n' "$root"
    cat <<'MARKDOWN'
Use `python-analysis` or `~/.venvs/analysis/bin/python` for the shared read-only analysis environment.
Use `uv` to manage project dependencies. Ubuntu manages the system `python3`.

Ubuntu provides `convert` (ImageMagick 6) and `yq` (the jq-based implementation).
The Manager provides aliases for `fd` and `bat`.

MARKDOWN
    printf '## Managed versions\n\n'
    printf -- '- uv %s\n- mise %s\n- Node.js %s\n- DuckDB CLI %s\n- Analysis Python %s\n' \
        "$uv_version" "$mise_version" "$node_version" "$duckdb_version" "$python_version"
    printf '\n## apt packages\n\n```text\n'
    dpkg-query -W -f='${binary:Package} ${Version}\n' "$@"
    printf '```\n'
} > "$staging/ENVIRONMENT.md"
mv -f "$staging/ENVIRONMENT.md" "$release/ENVIRONMENT.md"
touch "$release/.complete"
ln -s "versions/$revision" "$staging/current"
mv -Tf "$staging/current" "$root/current"

# Keep existing commands and user documentation in place.
mkdir -p /usr/local/bin
for name in uv mise duckdb node npm npx python-analysis fd bat; do
    destination="/usr/local/bin/$name"
    if [[ ! -e "$destination" && ! -L "$destination" ]]; then
        ln -sT "$root/current/bin/$name" "$destination"
    fi
done
runuser -u lumiqo -- /bin/sh -eu -s -- "$root" <<'SH'
    docs_dir=/home/lumiqo/.local/share/lumiqot/docs
    mkdir -p "$docs_dir" /home/lumiqo/.venvs
    link_if_missing() {
        if [ ! -e "$2" ] && [ ! -L "$2" ]; then
            ln -sT "$1" "$2"
        fi
    }
    link_if_missing "$1/current/ENVIRONMENT.md" "$docs_dir/ENVIRONMENT.md"
    link_if_missing "$1/current/analysis" /home/lumiqo/.venvs/analysis
SH
