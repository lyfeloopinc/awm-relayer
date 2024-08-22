#!/usr/bin/env bash
# Copyright (C) 2024, Ava Labs, Inc. All rights reserved.
# See the file LICENSE for licensing terms.

set -e errexit

# Root directory
root=$(
    cd "$(dirname "${BASH_SOURCE[0]}")"
    cd .. && pwd
)

"$root"/scripts/generate.sh
"$root"/scripts/build_relayer.sh
"$root"/scripts/build_signature_aggregator.sh
