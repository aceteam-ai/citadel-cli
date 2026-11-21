#!/bin/bash
# release.sh (retired)
#
# This legacy release script published releases without the checksums
# signature that Citadel's updater requires, so a release made with it could
# never be installed by nodes. It now refuses to run.
#
# Use scripts/release.sh instead. See docs/release-signing.md.

echo "release.sh is retired: it cannot sign releases, and Citadel refuses unsigned updates." >&2
echo "Use ./scripts/release.sh instead (see docs/release-signing.md)." >&2
exit 1
