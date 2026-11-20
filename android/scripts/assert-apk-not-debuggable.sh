#!/usr/bin/env bash
#
# Assert that an APK is NOT debuggable, by reading the BUILT APK's manifest.
#
# 🔴 WHY: the public promptshipper build must not be debuggable. A debuggable
# APK lets anyone with USB access `adb shell run-as dev.zacx.promptshipper`
# and read the app's private files — the dictation history (audio and
# transcripts) and the encrypted token blobs — and attach a debugger to the
# running process. The gradle file says `isDebuggable = false`; this checks
# the artifact, because the artifact is what ships.
#
# Usage:
#   assert-apk-not-debuggable.sh <apk> [--expect-debuggable]
#
# `--expect-debuggable` inverts the verdict. CI runs it against the DEBUG APK
# as the negative control: if this script cannot see `application-debuggable`
# in an APK that is debuggable, its "not debuggable" verdict on the release
# APK means nothing.
#
# Exit codes:
#   0  verdict as expected
#   2  usage error, or the APK does not exist
#   3  aapt2 could not be located           (NOT a verdict)
#   4  aapt2 ran but printed no package line (NOT a verdict)
#   5  the verdict is the wrong one
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
    echo "usage: $(basename "$0") <apk> [--expect-debuggable]" >&2
    exit 2
fi
apk=$1
expect_debuggable=0
if [[ $# -eq 2 ]]; then
    [[ $2 == --expect-debuggable ]] || { echo "error: unknown flag $2" >&2; exit 2; }
    expect_debuggable=1
fi
[[ -f $apk ]] || { echo "error: no such APK: $apk" >&2; exit 2; }

find_aapt2() {
    if [[ -n ${AAPT2:-} ]]; then
        [[ -x $AAPT2 ]] && { printf '%s\n' "$AAPT2"; return 0; }
        return 1
    fi
    local root bt
    for root in "${ANDROID_HOME:-}" "${ANDROID_SDK_ROOT:-}"; do
        [[ -z $root ]] && continue
        for bt in "$root/build-tools/34.0.0" \
                  $(ls -d "$root"/build-tools/*/ 2>/dev/null | sort -rV); do
            if [[ -x "${bt%/}/aapt2" ]]; then
                printf '%s\n' "${bt%/}/aapt2"
                return 0
            fi
        done
    done
    command -v aapt2 2>/dev/null
}

if ! aapt2=$(find_aapt2) || [[ -z $aapt2 ]]; then
    echo "error: aapt2 NOT FOUND — a tooling failure, not a verdict." \
         "Set ANDROID_HOME or AAPT2." >&2
    exit 3
fi

badging=$("$aapt2" dump badging "$apk")
# Positive control on the instrument: a dump that printed nothing would read
# as "no application-debuggable line" — i.e. as a pass.
if ! grep -q "^package: name='" <<<"$badging"; then
    echo "error: aapt2 dump badging printed no package line; nothing verified." >&2
    exit 4
fi

if grep -q '^application-debuggable' <<<"$badging"; then
    debuggable=1
else
    debuggable=0
fi

echo "apk:         $apk"
echo "debuggable:  $debuggable (expected $expect_debuggable)"
if [[ $debuggable -ne $expect_debuggable ]]; then
    echo "error: wrong verdict — refusing." >&2
    exit 5
fi
echo "OK"
