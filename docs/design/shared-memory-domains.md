# Shared Memory Domains

Shared memory is optional VM creation state. In an interactive `vmsh` session,
select or create each participating system with the same domain:

```sh
@sender --from alpine --shmem packets,0xd1000000
@receiver --from alpine --shmem packets,0xd1000000
```

The address is the guest physical base of a 4 KiB MMIO control window. It must
be non-zero and page-aligned. The host rejects a control window or shared region
that overlaps guest RAM, another mapped region, or the platform device aperture.

Domains are owned by the VM manager. The first attached VM creates the domain;
later VMs with the same domain name attach to it. A VM crash releases its
attachment. Regions and their contents remain available while at least one VM
is attached and are discarded after the last attachment is released.

Shared-memory VMs do not support startup snapshot capture or restore.

## Backend rollout

The initial vertical slice supports managed Linux guests on Linux/amd64 and
Linux/arm64 KVM hosts. Other backends reject the option during VM startup rather
than creating a VM without a working device. This is an explicit staged rollout:
dynamic borrowed mappings and teardown must be implemented and exercised
independently for HVF and WHP before those backends claim the attachment.

Hardware validation requires a Linux amd64 or Linux arm64 machine with KVM,
access to `/dev/kvm`, and enough capacity to run two VMs concurrently.

## Guest ABI

All registers are little-endian. The control window begins with:

| Offset | Size | Access | Meaning |
| --- | ---: | --- | --- |
| `0x00` | 8 | read | Magic bytes `shmem\0\0\1` |
| `0x08` | 8 | read | Descriptor count in bits 0–31; page shift in bits 32–63 |
| `0x10` | 32 | read/write | Descriptor 0 |
| `0x30` | 32 | read/write | Descriptor 1 |
| ... | | | 15 descriptors total |

Each descriptor is:

| Offset | Size | Access | Meaning |
| --- | ---: | --- | --- |
| `0x00` | 4 | read/write | Non-zero region ID |
| `0x04` | 4 | read/write | Status |
| `0x08` | 8 | read/write | Region size |
| `0x10` | 8 | read/write | Guest physical mapping address |
| `0x18` | 4 | read | Error code |
| `0x1c` | 4 | reserved | Reads as zero |

The guest writes the ID, size, and mapping address, then commits the request by
writing `REQUESTED` to status. Processing completes before that MMIO write
returns. The guest must read status and error before accessing the mapping.

Statuses:

| Value | Name | Meaning |
| ---: | --- | --- |
| 0 | `EMPTY` | Descriptor can be populated |
| 1 | `REQUESTED` | Commit value written by the guest |
| 2 | `MAPPED` | Mapping succeeded and is immutable |
| 3 | `ERROR` | Mapping failed; fields may be corrected and retried |

Errors:

| Value | Name |
| ---: | --- |
| 0 | `NONE` |
| 1 | `INVALID_ID` |
| 2 | `INVALID_SIZE` |
| 3 | `SIZE_CONFLICT` |
| 4 | `INVALID_GPA` |
| 5 | `MAPPING_FAILED` |

IDs are scoped to a domain. The first request for an ID allocates zeroed,
page-aligned backing. An identical ID and size returns the same backing to every
attached VM. Reusing an ID with a different size returns `SIZE_CONFLICT`.

Region sizes and mapping addresses must be page-aligned. A region is limited to
1 GiB, a domain to 4 GiB, the daemon registry to 16 GiB, and a domain to 15
regions. Successful mappings cannot be changed or removed during the lifetime
of the VM.
