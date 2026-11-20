#!/usr/bin/env bash
# check-no-private.sh — fail if anything from the maintainer's private
# infrastructure is about to land in this repository.
#
#   scripts/check-no-private.sh            scan every file git would commit
#                                          (tracked + untracked-not-ignored)
#   scripts/check-no-private.sh PATH...    scan exactly these files
#
# Exit 0: clean.  Exit 1: a forbidden string was found, or an allowlist entry
# is stale.  Exit 2: the scan itself could not run (no files, unreadable
# allowlist) — a scan of nothing is a failure, never a pass.
#
# Only the file path, line number and the MATCHED TEXT are printed, never the
# whole line: the line may hold a secret, and CI logs are not the place for it.
#
# ALLOWLIST: scripts/private-allowlist.txt holds exact `path<TAB>line` pairs
# that are permitted (e.g. docs/PLAN.md stating the rule itself names the
# patterns). It is the ONE path excluded from the scan, every entry must still
# match a real line (a stale entry fails), and a forbidden string on any OTHER
# line of an allowlisted file still fails.
set -euo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
allowlist_rel="scripts/private-allowlist.txt"
allowlist="${root}/${allowlist_rel}"

# The patterns are assembled from pieces so that this file does not match
# itself. Case-insensitive (hostnames are), extended regex.
patterns=(
  '10\.42\.'          # the private mesh address range
  'homelab\.lan'      # internal DNS zone
  'zacx\.dev'         # the maintainer's private hostnames
  'ENC\[AES256_GCM'   # a SOPS-encrypted value
  'sops'':'           # a SOPS metadata block
)
re=$(IFS='|'; printf '%s' "${patterns[*]}")

files=()
if (($#)); then
  files=("$@")
else
  while IFS= read -r -d '' f; do
    [[ "$f" == "$allowlist_rel" ]] && continue
    [[ -f "${root}/${f}" ]] || continue   # tracked but deleted in the tree
    files+=("${root}/${f}")
  done < <(git -C "$root" ls-files -z --cached --others --exclude-standard)
fi
if ((${#files[@]} == 0)); then
  echo "check-no-private: no files to scan — refusing to report a clean result" >&2
  exit 2
fi

declare -A allowed=() used=()
if [[ -f "$allowlist" ]]; then
  while IFS= read -r entry || [[ -n "$entry" ]]; do
    [[ -z "$entry" || "$entry" == \#* ]] && continue
    if [[ "$entry" != *$'\t'* ]]; then
      echo "check-no-private: malformed allowlist entry (want path<TAB>line): ${entry%%$'\t'*}" >&2
      exit 2
    fi
    allowed["$entry"]=1
  done <"$allowlist"
fi

findings=0
for f in "${files[@]}"; do
  rel=${f#"${root}/"}
  # -I skips binary files; -n line numbers. A read error is fatal (set -e
  # does not see it inside the substitution, so check grep's status).
  set +e
  hits=$(grep -I -n -i -E -- "$re" "$f")
  rc=$?
  set -e
  if ((rc > 1)); then
    echo "check-no-private: could not read ${rel}" >&2
    exit 2
  fi
  ((rc == 1)) && continue
  while IFS= read -r hit; do
    lineno=${hit%%:*}
    line=${hit#*:}
    key="${rel}"$'\t'"${line}"
    if [[ -n "${allowed[$key]:-}" ]]; then
      used["$key"]=1
      continue
    fi
    match=$(printf '%s\n' "$line" | grep -o -i -E -- "$re" | head -n1)
    echo "FORBIDDEN ${rel}:${lineno}: ${match}"
    findings=$((findings + 1))
  done <<<"$hits"
done

stale=0
if (($# == 0)); then
  for key in "${!allowed[@]}"; do
    if [[ -z "${used[$key]:-}" ]]; then
      echo "STALE allowlist entry for ${key%%$'\t'*} (no such line any more — delete it)"
      stale=$((stale + 1))
    fi
  done
fi

echo "check-no-private: scanned ${#files[@]} file(s), ${findings} forbidden, ${stale} stale"
((findings == 0 && stale == 0))
