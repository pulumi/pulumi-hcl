#!/usr/bin/env bash

# Generate a changie fragment for a dependency bump that Renovate applied.
#
# Invoked as `make renovate DEP=<name> VERSION=<new version> FILE=<package file>` from a
# renovate postUpgradeTasks hook (see renovate.json5), which fills the arguments from its
# {{{depName}}}/{{{newVersion}}}/{{{packageFile}}} template variables, once per updated
# dependency. Only the root Go module and the terraform-provider pin in
# pkg/server/server.go ship in the released binaries, so anything else (GitHub
# Actions, test-only modules) is a no-op.
#
# Runs before the PR exists, so fragments carry no PR number or component; the
# `dependencies` kind in .changie.yaml is formatted accordingly.

set -euo pipefail

dep="$1"
version="$2"
file="$3"

case "$file" in
    go.mod|pkg/server/server.go) ;;
    *) exit 0 ;;
esac

slug=$(printf '%s-%s' "$dep" "$version" | tr '[:upper:]' '[:lower:]' | tr -cs 'a-z0-9' '-')
slug=${slug#-}
slug=${slug%-}

# Pinned changie version; `go run` because the renovate container has Go but not our
# mise toolchain.
go run github.com/miniscruff/changie@v1.25.1 new \
    --dry-run \
    --kind dependencies \
    --body "Update $dep to $version" \
    > ".changes/unreleased/dependencies-$slug.yaml"
