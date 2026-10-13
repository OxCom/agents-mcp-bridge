#!/usr/bin/env bash
# Stage docs/ into a Jekyll source tree for GitHub Pages.
#
# Usage: .github/scripts/stage-pages.sh <out-dir>
#
# The docs are written to be read on github.com, where `{{placeholder}}` and
# `{%` are literal text. Jekyll renders every page through Liquid first and
# would silently turn them into empty strings, and the github-pages gem
# (Jekyll 3.10) has no render_with_liquid switch. So each published page is
# wrapped in a raw block here, at build time, and the sources stay untouched.
set -euo pipefail

repo_root=$(cd "$(dirname "$0")/../.." && pwd)
blob="https://github.com/OxCom/agents-mcp-bridge/blob/master"

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <out-dir>" >&2
  exit 2
fi
out=$1

# Refuse to write into a directory that already holds something: the script
# never deletes, so a stale tree cannot leak pages into the site.
if [ -e "$out" ] && [ -n "$(ls -A "$out")" ]; then
  echo "error: $out exists and is not empty" >&2
  exit 1
fi
mkdir -p "$out"

# kb/ is SwarmVault-owned and plan/ is working notes; neither is site content.
tar -C "$repo_root/docs" --exclude=./kb --exclude=./plan -cf - . | tar -C "$out" -xf -

# stage_page <source.md> <dest.md> <mode>
# mode "docs": the page lives in docs/; "root": the page lives at the repo root.
stage_page() {
  local src=$1 dest=$2 mode=$3 title

  if grep -q 'endraw' "$src"; then
    echo "error: $src contains 'endraw', which would end the raw block early" >&2
    exit 1
  fi

  # jekyll-titles-from-headings only looks at the start of the file, and the
  # raw tag below would sit in front of the heading.
  title=$(sed -n 's/^# //p' "$src" | head -n 1)
  if [ -z "$title" ]; then
    echo "error: $src has no '# ' heading to take a title from" >&2
    exit 1
  fi
  title=${title//\\/\\\\}
  title=${title//\"/\\\"}

  {
    printf -- '---\ntitle: "%s"\n---\n{%% raw %%}\n' "$title"
    # Links that leave the published tree go to the file on GitHub instead.
    if [ "$mode" = root ]; then
      # A root page links into docs/ as docs/NN-x.md; published, those pages
      # are siblings. kb/ and plan/ are not published, so they go to GitHub.
      perl -pe '
        s{\]\(docs/((?:kb|plan)/[^)]*)\)}{]('"$blob"'/docs/$1)}g;
        s{\]\(docs/([^)]*)\)}{]($1)}g;
        s{\]\((?![a-z][a-z0-9+.-]*:|#|/|[0-9]{2}-)([^)]+)\)}{]('"$blob"'/$1)}g;
      ' "$src"
    else
      perl -pe '
        s{\]\(\.\./([^)]*)\)}{]('"$blob"'/$1)}g;
        s{\]\(((?:kb|plan)/[^)]*)\)}{]('"$blob"'/docs/$1)}g;
      ' "$src"
    fi
    printf '\n{%% endraw %%}\n'
  } > "$dest"
}

for src in "$repo_root"/docs/[0-9][0-9]-*.md; do
  stage_page "$src" "$out/$(basename "$src")" docs
done
stage_page "$repo_root/SECURITY.md" "$out/security.md" root
