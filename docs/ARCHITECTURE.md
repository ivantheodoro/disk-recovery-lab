# Architecture

```
cmd/disk-recovery-lab   launcher: elevation, token, opens the window
internal/server         HTTP API + embedded web UI (web/index.html)
internal/scan           scan sessions (saved as gzip JSON), pipeline, selective recovery
internal/hfs            HFS+/HFSX reader (catalog, extents overflow, bitmap, journal, stale records)
internal/carve          signature carvers ("deep scan")
internal/detect         partition tables, filesystem signatures, diagnosis, lost partitions, imaging, repair
internal/rawdisk        read-only, sector-aligned access to devices and images (per-OS size/open)
internal/platform       per-OS: drive listing, mount, fsck, elevation, opening the UI, destinations
internal/plist          tiny XML plist decoder (macOS diskutil/hdiutil output)
```

## Process model

1. The launcher (running as the user) picks a free port and a random token, writes the token to a private temp
   file, and starts a second copy of itself with administrator rights: `--serve --token-file …` via osascript on
   macOS, `ShellExecute runas` on Windows, pkexec on Linux.
2. The elevated server listens on `127.0.0.1:<port>`. The launcher waits for `/api/ping`, then opens
   `http://127.0.0.1:<port>/?t=<token>` as the user and exits.
3. The UI keeps working through restarts: **Restart** re-executes the server with the same port and token, and the
   page reloads.

## Scan pipeline (`scan.Run`)

1. Read the partition map (GPT/MBR). If it is unusable, search the disk for volume headers (`detect.SearchLostPartitions`).
2. For each HFS+ volume:
   - read the extents overflow file, then the catalog B-tree;
   - collect records from live leaf nodes, from the slack after live records, from unlinked (freed) nodes, and from old
     node copies in the journal. Stale copies of deleted files keep their names and extents;
   - if the B-tree header is damaged, guess the node size and walk every node.
3. Build the tree. Each `Item` stores byte ranges on the source, so recovery and preview are simple reads.
4. Deep scan carves the allocation bitmap's free runs (or the whole partition for unsupported filesystems).
5. The scan is checkpointed every 30 seconds and saved at the end. Item IDs are indices, so the UI pages through
   folders cheaply.

## Saved data

| OS | Location |
|---|---|
| macOS | `~/Library/Application Support/Disk Recovery Lab` |
| Windows | `%LOCALAPPDATA%\Disk Recovery Lab` |
| Linux | `~/.local/share/disk-recovery-lab` |

Each location contains `scans/` (`<id>.json.gz` plus `<id>.meta.json`) and `images/` (practice disks and byte-to-byte images).
