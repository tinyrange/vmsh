# vmsh v0.4.1

## Fixed

- Fixed an Apple Silicon guest boot delay caused by the virtio-balloon device
  raising a configuration interrupt before the guest driver was ready. Linux
  could report `irq 15: nobody cared`, disable the interrupt, and spend several
  seconds recovering while loading the balloon driver.
- Stopped classifying ordinary IPv6 multicast traffic from Linux guests as an
  IPv4 source-identity violation. Unsupported IPv6 traffic remains blocked, but
  no longer emits duplicate warnings suggesting that the guest is spoofing its
  network identity.

## Validation

- Verified the interactive `@alpine` context-selection workflow on Apple
  Silicon macOS without the interrupt storm or invalid-source warnings.
- Re-ran the cc and vmsh unit, integration, cross-platform, KVM, formatting,
  and static-analysis test suites.
