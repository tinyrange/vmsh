# vmsh v0.5.1

## Fixed

- Preserved the `com.apple.security.hypervisor` entitlement on the final signed
  NeurodeskAppX bundle. The v0.5.0 macOS app lost this entitlement when its
  outer bundle was signed, then exited on first launch when App Translocation
  prevented it from repairing its own executable signature.
- Added a release-time assertion that reads the entitlement from the finished
  app bundle, preventing a signed and notarized macOS artifact from being
  published without Hypervisor.framework access.

## Validation

- Verified the original v0.5.0 failure through a normal quarantined launch from
  Downloads.
- Verified the corrected signing sequence retains the hypervisor entitlement.
- Re-ran the cross-platform release build, macOS Developer ID signing,
  notarization, strict signature verification, and signed payload smoke test.
