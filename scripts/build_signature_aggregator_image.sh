#!/usr/bin/env bash

# Use this script to build the approver docker image in your local working copy
# of the repository. Be prepared to repeatedly hit your YubiKey during the `go
# mod download` step of the image build, just like you need to do for a
# non-Docker build.

set -o errexit
set -o nounset
set -o xtrace

REPO_PATH=$(git rev-parse --show-toplevel)
source "$REPO_PATH/scripts/versions.sh"
source "$REPO_PATH/scripts/constants.sh"

docker_repo=avaplatform/signature-aggregator

docker --debug build \
	--build-arg="GO_VERSION=${GO_VERSION}" \
	--build-arg="API_PORT=${SIGNATURE_AGGREGATOR_API_PORT}" \
	--build-arg="METRICS_PORT=${SIGNATURE_AGGREGATOR_METRICS_PORT}" \
	--file "$REPO_PATH/signature-aggregator/Dockerfile" \
	--ssh default \
	--tag "${docker_repo}:$(git rev-parse HEAD)" \
	--tag "${docker_repo}:$(git rev-parse --abbrev-ref HEAD | sed 's/\//-/g')" \
	.
