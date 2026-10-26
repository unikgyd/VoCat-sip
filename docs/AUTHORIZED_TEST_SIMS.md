# Explicit test SIM policy

An operator can explicitly configure complete subscriber identities that are
authorized for a controlled test, including a particular MCC 460 test card.
The default regional policy still applies to every unlisted SIM. This setting
does not grant software-license or carrier authorization and does not change
the configured-device limit, SIM identity checks or resource ownership rules.

Create a private JSON file outside the source repository:

```json
{"authorized_test_imsis":["<complete authorized test IMSI>"]}
```

Set `VOCAT_AUTHORIZED_TEST_SIMS_FILE` to its absolute path, or set
`authorized_test_sims_file` in the existing JSON process configuration. For
Docker, mount the file read-only and use its in-container path. Keep the file
and its parent private (0600/0700 on Linux); never commit subscriber identities.

The service validates and loads this file before discovering or operating any
modem. It accepts at most ten complete numeric identities, with no prefixes,
wildcards, placeholders or duplicates. A configured file that is missing or
invalid aborts startup before hardware access. The policy is immutable for that
process: restart after editing it. Removing the setting restores the default
regional policy at the next startup. Be aware that the existing regional
enforcer can put an unlisted restricted card into flight mode.

All normal Host IMS and VoWiFi readiness, ownership, per-SIM activation and
cleanup checks remain required. Explicit admission is not proof of carrier
registration, call support or simultaneous operation.
