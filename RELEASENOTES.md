# vmsh v0.8.0

## Highlights

- Made NeurodeskAppX container launches substantially faster by serving CVMFS
  from the host, selecting a responsive mirror before boot, and reusing indexed
  catalogs. The first tested `niimath` launch fell from about 15 seconds to
  about 1.2 seconds.
- Added registry-compatible compressed image storage, range-based delta
  downloads, and background image staging for NeurodeskAppX.
- Fixed persistent-home stalls and recovery failures affecting VS Code Remote
  SSH and other metadata-heavy SquadVM workloads.

## Desktop applications

- Added an expandable Advanced section with persistent memory and vCPU controls
  bounded by the host's available resources.
- Improved integrated window chrome, title placement, maximize and restore
  behavior, and per-monitor DPI sizing on Windows while retaining native macOS
  window controls.
- Defaulted Windows desktop VMs to four vCPUs and kept selected resource values
  across launches.
- Corrected image disk-space estimates and showed concurrent transfer progress
  without hiding active downloads between short demand-read gaps.

## NeurodeskAppX

- Moved `neurodesk.ardc.edu.au` CVMFS access to a long-lived host service and
  disabled the redundant guest CVMFS daemon.
- Benchmarked configured mirrors before boot, automatically preferred healthy
  fast mirrors, and added a persistent manual mirror selector.
- Added visible CVMFS transfer counts, rates, and expandable logical-path
  progress while bounding the shared host cache to 5 GiB.
- Stored OCI layers in compressed form and reused unchanged compressed byte
  ranges when enhanced eStargz images change.
- Staged image updates in the background, validated them completely, and
  activated them atomically on the next requested refresh.
- Kept the enhanced format compatible with ordinary OCI registries and retained
  conventional image tags for clients that do not use the optimized path.

## SquadVM

- Made file durability selective so one `fsync` no longer synchronizes every
  dirty persistent-home inode or blocks unrelated SSH and filesystem traffic.
- Added separate file, directory, and whole-filesystem durability, advisory
  range locks, concurrent virtio-fs request handling, and improved persistent
  home caching.
- Recovered cleanly from interrupted persistent-home attachments, discarded
  unreachable orphaned metadata trees, and stopped replaying completed recovery
  warnings on later starts.
- Verified a fresh VS Code Remote SSH server download, 3,601-file unpack,
  integrity check, server startup, and extension-host connection.

## Reliability

- Parallelized outstanding persistent-file synchronization during clean
  shutdown and avoided re-syncing already durable home data.
- Preserved incomplete or unreferenced persistent data in recovery quarantine
  while keeping successful recovery informational rather than fatal.
- Kept the conventional and enhanced image publishing paths side by side so
  existing OCI consumers remain backwards compatible.

## Release artifacts

- SquadVM for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- NeurodeskAppX for Linux AMD64, Windows AMD64, and Apple Silicon macOS.
- vmsh command-line builds for all supported release platforms.
- SHA-256 checksums for every published artifact.
