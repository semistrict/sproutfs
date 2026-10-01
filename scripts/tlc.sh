#!/usr/bin/env bash
# Model-checks a TLA+ spec with TLC under one configuration.
#
#   scripts/tlc.sh spec/ownership/Ownership.tla spec/ownership/MCTakeover.cfg
#
# TLC comes from the pinned tla2tools.jar, MIT licensed, which is
# fetched once into ~/.cache/sproutfs and checked against its digest. Java
# comes from JAVA, or a Homebrew openjdk, or the PATH.
set -euo pipefail

version=1.7.4
digest=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88

if [ "$#" -lt 2 ]; then
    echo "usage: $0 <module.tla> <config.cfg> [tlc options]" >&2
    exit 2
fi
module=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
config=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
shift 2

cache=${XDG_CACHE_HOME:-$HOME/.cache}/sproutfs
jar=$cache/tla2tools-$version.jar
if [ ! -f "$jar" ]; then
    mkdir -p "$cache"
    curl -fsSL -o "$jar.partial" \
        "https://github.com/tlaplus/tlaplus/releases/download/v$version/tla2tools.jar"
    mv "$jar.partial" "$jar"
fi
actual=$(shasum -a 256 "$jar" | cut -d' ' -f1)
if [ "$actual" != "$digest" ]; then
    echo "$jar has digest $actual, want $digest" >&2
    exit 1
fi

java=${JAVA:-}
if [ -z "$java" ] && [ -x /opt/homebrew/opt/openjdk/bin/java ]; then
    java=/opt/homebrew/opt/openjdk/bin/java
fi
java=${java:-java}

states=$(mktemp -d)
trap 'rm -rf "$states"' EXIT
cd "$(dirname "$module")"
"$java" -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -cleanup \
    -metadir "$states" -config "$config" "$@" "$(basename "$module")"
