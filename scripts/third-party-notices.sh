#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

go mod download
main=$(go list -m)

{
  printf 'Third-party software in 5Stack Demo Parser\n\nThe Go standard library and the Go modules compiled into the demo-parser\nbinary, and their licenses.\n'
  printf '\n-----------\n\nGo standard library\n\n'
  cat "$(go env GOROOT)/LICENSE"
  echo
  go list -deps -f '{{if and .Module (not .Standard)}}{{.Module.Path}}{{"\t"}}{{if .Module.Replace}}{{.Module.Replace.Dir}}{{else}}{{.Module.Dir}}{{end}}{{end}}' ./... |
    sort -u |
    grep -v "^${main}"$'\t' |
    while IFS=$'\t' read -r module dir; do
      printf '\n-----------\n\n%s\n\n' "$module"
      ls "$dir" | grep -iE '^(licen[cs]e|copying|notice|patents)([.-].*)?$' | sort | while read -r file; do
        cat "$dir/$file"
        echo
      done
    done
} > THIRD_PARTY_NOTICES.txt
