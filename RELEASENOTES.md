# vmsh v0.6.1

## Fixes

- Fixed SquadVM startup on slower Windows hosts. The desktop readiness check now
  stays connected for its full startup timeout instead of failing after the
  HTTP client's 30-second response-header deadline.
