#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p bin
go build -o bin/dug ./cmd/dug
go build -o bin/dug-web ./cmd/dug-web
go build -o bin/dug-s3local ./integration/cmd/dug-s3local
go build -o bin/dug-wordpress ./sources/wordpress/cmd/dug-wordpress
go build -o bin/dug-wordpress-themes ./sources/wordpress/cmd/dug-wordpress-themes
go build -o bin/dug-chrome ./sources/chrome/cmd/dug-chrome
go build -o bin/dug-firefox ./sources/firefox/cmd/dug-firefox
go build -o bin/dug-github ./sources/github/cmd/dug-github
go build -o bin/dug-n8n ./sources/n8n/cmd/dug-n8n
go build -o bin/dug-ansible-galaxy ./sources/ansible-galaxy/cmd/dug-ansible-galaxy
go build -o bin/dug-obsidian ./sources/obsidian/cmd/dug-obsidian
go build -o bin/dug-helm ./sources/helm/cmd/dug-helm
go build -o bin/dug-joomla ./sources/joomla/cmd/dug-joomla
