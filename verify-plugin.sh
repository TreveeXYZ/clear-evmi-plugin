#!/usr/bin/env bash
# Compare the pkghash of every package a plugin .so shares with an EVMI server binary.
# plugin.Open() does exactly this comparison at load time, so a clean run here means
# the .so will load; a MISMATCH line names the package that would be rejected.
#
#   usage: verify-plugin.sh <server-binary> <plugin.so>
#
# Needs binutils (readelf/objdump/xxd) — run it inside the golang builder image.
set -euo pipefail

srv="$1"; plg="$2"

# print "<symbol> <8-byte hex value>" for each go:link.pkghashbytes.* symbol.
# $3 (optional) restricts the dump to the symbols listed in that file.
dump() {
  local f="$1" filter="${2:-}" secs sym vaddr t a s o off
  # objdump -h columns: idx name size vma lma fileoff  (hex, no 0x) -> "vma size fileoff"
  secs="$(objdump -h "$f" | awk '$1 ~ /^[0-9]+$/ {print $4, $3, $6}')"
  readelf -sW "$f" | awk '$8 ~ /^go:link\.pkghashbytes\./ {print $8, $2}' | sort -u |
  while read -r sym vaddr; do
    if [ -n "$filter" ] && ! grep -qxF "$sym" "$filter"; then continue; fi
    t=$((16#$vaddr)); off=""
    while read -r a s o; do
      a=$((16#$a)); s=$((16#$s)); o=$((16#$o))
      if [ "$s" -gt 0 ] && [ "$t" -ge "$a" ] && [ "$t" -lt "$((a + s))" ]; then
        off=$((o + t - a)); break
      fi
    done <<< "$secs"
    [ -n "$off" ] && printf '%s %s\n' "$sym" "$(dd if="$f" bs=1 skip="$off" count=8 2>/dev/null | od -An -tx1 | tr -d "[:space:]")"
  done
}

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
dump "$plg" > "$tmp/plg"
cut -d' ' -f1 "$tmp/plg" > "$tmp/names"
dump "$srv" "$tmp/names" > "$tmp/srv"

shared=0; bad=0
while read -r sym val; do
  sval="$(awk -v s="$sym" '$1 == s {print $2; exit}' "$tmp/srv")"
  [ -z "$sval" ] && continue          # package not linked into the server; nothing to match
  shared=$((shared + 1))
  if [ "$sval" != "$val" ]; then
    bad=$((bad + 1))
    echo "  MISMATCH ${sym#go:link.pkghashbytes.}  server=$sval plugin=$val"
  fi
done < "$tmp/plg"

echo "checked $shared shared packages, $bad mismatched"
[ "$shared" -gt 0 ] || { echo "found NO shared packages - the comparison did not work; refusing to call this OK"; exit 1; }
[ "$bad" -eq 0 ] || { echo "this .so would FAIL plugin.Open"; exit 1; }
echo "OK - this .so will load"
