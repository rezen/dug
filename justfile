set shell := ["sh", "-eu", "-c"]
set positional-arguments

# List available development commands.
default:
    @just --list

# Build the host, local S3 server, and all registry plugins into bin/.
build:
    sh scripts/build.sh

# Check justfile formatting, workspace race tests, vet, and module boundaries.
check:
    just --fmt --check
    sh scripts/check.sh

# Run race tests for every module, or one module such as sdk or sources/github.
test module="":
    sh scripts/check.sh test "$1"

# Run go vet for every module, or one module.
vet module="":
    sh scripts/check.sh vet "$1"

# Format Go code in every module and the justfile.
fmt:
    sh scripts/check.sh fmt
    just --fmt

# Verify independent builds without go.work and enforce package boundaries.
boundaries:
    sh scripts/check-boundaries.sh

# Regenerate protobuf and Connect code with the pinned tool versions.
generate:
    sh scripts/generate.sh

# Run the host CLI; forwards arguments unchanged, including quoted filters.
dug *args:
    go run ./cmd/dug "$@"

# Ensure local RustFS and the bucket exist, then run the host CLI.
local *args:
    go run ./cmd/dug local "$@"

# Run one registry plugin in the foreground: wordpress, wordpress-themes, chrome, firefox, github, n8n, ansible-galaxy, obsidian, or helm.
plugin source *args:
    @case "$1" in wordpress|wordpress-themes) module=wordpress ;; chrome|firefox|github|n8n|ansible-galaxy|obsidian|helm) module="$1" ;; *) echo "Unknown source: $1" >&2; exit 2;; esac; source_name="$1"; shift; exec go run "./sources/$module/cmd/dug-$source_name" "$@"

# One command: start the source's plugin and local RustFS, list it, stop the plugin. Extra args are host flags, e.g. --limit 20 --option namespaces=community.
list source *args:
    @just _with-plugin list "$@"

# One command: start the source's plugin and local RustFS, list and download it, stop the plugin. Extra args are host flags, e.g. --version-depth unlimited --list-limit 20.
sync source *args:
    @just _with-plugin sync "$@"

# Run a host command (list or sync) against a source with its plugin started and stopped around it.
_with-plugin command source *args:
    #!/bin/sh
    set -eu
    command_name="$1"; shift
    case "$1" in
        wordpress) module=wordpress port=7781 ;;
        chrome) module=chrome port=7782 ;;
        firefox) module=firefox port=7783 ;;
        github) module=github port=7784 ;;
        wordpress-themes) module=wordpress port=7785 ;;
        n8n) module=n8n port=7786 ;;
        ansible-galaxy) module=ansible-galaxy port=7787 ;;
        *) echo "Unknown source: $1" >&2; exit 2 ;;
    esac
    source_name="$1"; shift
    if nc -z 127.0.0.1 "$port" 2>/dev/null; then
        echo "port $port is already in use; stop the running $source_name plugin first" >&2
        exit 1
    fi
    tmp=$(mktemp -d)
    go build -o "$tmp/plugin" "./sources/$module/cmd/dug-$source_name"
    "$tmp/plugin" --listen "127.0.0.1:$port" &
    pid=$!
    trap 'kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; rm -rf "$tmp"' EXIT INT TERM
    until nc -z 127.0.0.1 "$port" 2>/dev/null; do
        kill -0 "$pid" 2>/dev/null || { echo "plugin $source_name exited before listening" >&2; exit 1; }
        sleep 0.2
    done
    go run ./cmd/dug local "$command_name" "$source_name" --plugin-url "http://127.0.0.1:$port" "$@"

# Start RustFS in Docker on loopback with persistent data/ and logs/ directories.
s3:
    mkdir -p data logs
    docker run -d --name dug-rustfs -p 127.0.0.1:9000:9000 -p 127.0.0.1:9001:9001 -v "$(pwd)/data:/data" -v "$(pwd)/logs:/logs" rustfs/rustfs:1.0.0

# Run persistent local GoFakeS3 in the foreground; accepts server flags.
s3-fake *args:
    go run ./integration/cmd/dug-s3local "$@"

# Live WordPress plugin top-100 download, S3 integrity checks, and resume verification.
e2e-wordpress:
    bash scripts/e2e-wordpress.sh wordpress

# Live WordPress theme top-100 download, S3 integrity checks, and resume verification.
e2e-wordpress-themes:
    bash scripts/e2e-wordpress.sh wordpress-themes

# Live Firefox version downloads, S3 integrity checks, and resume verification.
e2e-firefox:
    bash scripts/e2e-firefox.sh
