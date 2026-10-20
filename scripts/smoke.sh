#!/usr/bin/env bash
# End-to-end smoke test of a built binary (used by CI on every OS):
# start it without elevation, log in with its token, list the real drives,
# scan a fixture image, and recover one file.
#   scripts/smoke.sh path/to/binary
set -euo pipefail
BIN=$1
PY=$(command -v python3 || command -v python)
python3() { "$PY" "$@"; }
# Git Bash on Windows: native programs need C:\ style paths.
native() { if command -v cygpath >/dev/null; then cygpath -w "$1"; else python3 -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$1"; fi; }
W=$(mktemp -d)
trap 'kill $PID 2>/dev/null || true; wait $PID 2>/dev/null || true; rm -rf "$W"' EXIT
gunzip -c testdata/hfs.img.gz > "$W/hfs.img"
"$BIN" --no-elevate --no-ui --port 18765 --data-dir "$(native "$W")/data" > "$W/out.txt" 2>&1 &
PID=$!
for i in $(seq 1 60); do grep -q 'http://' "$W/out.txt" 2>/dev/null && break; sleep 0.5; done
URL=$(grep -o 'http://[^ ]*' "$W/out.txt")
TOKEN=${URL#*t=}
B=http://127.0.0.1:18765
for i in $(seq 1 60); do curl -sf "$B/api/ping" >/dev/null && break; sleep 0.5; done
get()  { curl -sf --cookie "drl=$TOKEN" "$B$1"; }
post() { curl -sf --cookie "drl=$TOKEN" -H 'Content-Type: application/json' -d "$2" "$B$1"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

echo "status:  $(get /api/status | json 'd["features"]["os"], "admin=", d["root"]')"
echo "drives:  $(get /api/drives | json 'len(d), "drive(s):", ", ".join((x["media"] or x["id"]) + " " + x["device"] for x in d)')"
test "$(curl -s -o /dev/null -w '%{http_code}' "$B/api/status")" = 401

IMG=$(native "$W/hfs.img")
BODY=$(python3 -c 'import json,sys; print(json.dumps(dict(path=sys.argv[1], label="smoke", deep=True)))' "$IMG")
SID=$(post /api/scans "$BODY" | json 'd["id"]')
for i in $(seq 1 60); do [ "$(get /api/scans/$SID | json 'd["running"]')" = False ] && break; sleep 0.5; done
echo "scan:    $(get /api/scans/$SID | json 'd["state"], d["files"], "files"')"
test "$(get /api/scans/$SID | json 'd["state"]')" = done
ID=$(get "/api/scans/$SID/items?q=red.png" | json 'd["items"][0]["id"]')
mkdir -p "$W/out"
DEST=$(native "$W/out")
BODY=$(python3 -c 'import json,sys; print(json.dumps(dict(ids=[int(sys.argv[1])], dest=sys.argv[2])))' "$ID" "$DEST")
JOB=$(post /api/scans/$SID/recover "$BODY" | json 'd["id"]')
for i in $(seq 1 40); do [ "$(get /api/jobs/$JOB | json 'd["state"]')" != running ] && break; sleep 0.5; done
echo "recover: $(get /api/jobs/$JOB | json 'd["state"], d["result"]["files"], "file(s)"')"
python3 - "$DEST" <<'PY'
import os, sys
found = [os.path.join(r, f) for r, _, fs in os.walk(sys.argv[1]) for f in fs if f == "red.png"]
assert found, "red.png was not recovered"
assert open(found[0], "rb").read(8) == b"\x89PNG\r\n\x1a\n", "recovered file is not a PNG"
print("recovered file OK:", os.path.relpath(found[0], sys.argv[1]))
PY
post /api/quit '{}' >/dev/null
echo "smoke test passed"
