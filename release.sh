#! /bin/bash

set -e

if [[ "$1" == "" ]]; then
  echo "Usage:"
  echo "  $0 VERSION"
  echo
  echo "Example:"
  echo "  $0 0.1.0"
  echo
  echo "Last 5 releases are:"
  git tag --sort=version:refname | tail -n5 | awk '{print "  ", $0}' | sort -r
  echo
  exit 1
fi

PV="${1}"


if git status | grep -q "modified:"; then
  echo "Sorry, commit your changes first."
  echo
  git status
  exit 1
fi

if git tag | grep -q "^${PV}\$"; then
  echo "Sorry, that version ${PV} does already exist as a git tag."
  echo
  echo "Last 5 releases are:"
  git tag --sort=version:refname | tail -n5 | awk '{print "  ", $0}' | sort -r
  echo
  exit 1
fi

echo "Using version: $PV"
sed -i -e 's/\(const appVersion = \)".*"$/\1"'${PV}'"/' main.go

git ci main.go -m "release $PV"
git tag $PV
git push --tags
