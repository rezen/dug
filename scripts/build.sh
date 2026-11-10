#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
# Source version from its module tag (sources/NAME/vX.Y.Z); untagged builds
# report "devel", and commits after the tag add git describe's suffix.
stamp() {
	version=$(git describe --tags --match "sources/$1/v*" 2>/dev/null | sed "s|^sources/$1/||") || version=
	echo "-X github.com/rezen/dug/sdk/services/pluginhost.version=${version:-devel}"
}
go build -o bin/dug ./cmd/dug
go build -o bin/dug-web ./cmd/dug-web
go build -o bin/dug-s3local ./integration/cmd/dug-s3local
go build -ldflags "$(stamp wordpress)" -o bin/dug-wordpress ./sources/wordpress/cmd/dug-wordpress
go build -ldflags "$(stamp wordpress)" -o bin/dug-wordpress-themes ./sources/wordpress/cmd/dug-wordpress-themes
go build -ldflags "$(stamp chrome)" -o bin/dug-chrome ./sources/chrome/cmd/dug-chrome
go build -ldflags "$(stamp firefox)" -o bin/dug-firefox ./sources/firefox/cmd/dug-firefox
go build -ldflags "$(stamp github)" -o bin/dug-github ./sources/github/cmd/dug-github
go build -ldflags "$(stamp n8n)" -o bin/dug-n8n ./sources/n8n/cmd/dug-n8n
go build -ldflags "$(stamp ansible-galaxy)" -o bin/dug-ansible-galaxy ./sources/ansible-galaxy/cmd/dug-ansible-galaxy
go build -ldflags "$(stamp obsidian)" -o bin/dug-obsidian ./sources/obsidian/cmd/dug-obsidian
go build -ldflags "$(stamp helm)" -o bin/dug-helm ./sources/helm/cmd/dug-helm
go build -ldflags "$(stamp joomla)" -o bin/dug-joomla ./sources/joomla/cmd/dug-joomla
go build -ldflags "$(stamp oci)" -o bin/dug-oci ./sources/oci/cmd/dug-oci
