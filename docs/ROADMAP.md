# Roadmap

Ideas, roughly in order of usefulness. Pick one and open an issue to discuss before starting big ones.

## Filesystem readers (quick scan with names)
- [ ] **NTFS**: MFT records, including deleted ones (in-use flag cleared) and their data runs
- [ ] **exFAT**: directory entry sets, including deleted entries
- [ ] **FAT12/16/32**: directory entries starting with 0xE5, cluster chains
- [ ] **APFS**: checkpoints, object map, file-system tree; encrypted volumes with the user's password
- [ ] **ext4**: inodes and extents; deleted entries from journal copies
- [ ] HFS+ compressed files (decmpfs in resource forks or attributes)

## Deep scan
- [ ] More signatures: HEIC/RAW camera formats, MP3, FLAC, MKV, Office 97, RAR, 7z, VMDK/VHDX headers
- [ ] Fragmented JPEG reassembly

## App
- [ ] Mount scan results as a read-only virtual drive
- [ ] Translations
- [ ] Signed and notarized macOS builds, signed Windows builds
- [ ] Linux AppImage
