# Import a carrier IPCC bundle

`vocat carrier import-ipcc` converts a local, user-provided Apple carrier bundle
into a reviewable VoCat carrier profile using the supported field allow-list.
The default invocation prints the import result and warnings without installing:

```sh
vocat carrier import-ipcc ./carrier.ipcc
vocat carrier import-ipcc --document-only ./carrier.ipcc > carrier-profile.json
```

If the archive contains multiple carrier bundles, select one with `--bundle`.
Use `--id` to override the generated profile identifier. Review the selected
carrier, matching rules, generated document and warnings before installation.

```sh
vocat carrier import-ipcc --install --profile-dir ./data/carrier-profiles.d ./carrier.ipcc
```

Without `--profile-dir`, installation uses `carrier-profiles.d` beside the
configured database. Installation is atomic and reports `installed_path` and
`restart_required`; apply a restart during the deployment's normal maintenance
window. `--document-only` and `--install` are mutually exclusive, and
`--profile-dir` requires `--install`.

An imported profile supplies configuration data. Existing authorization and SIM,
region, transport and carrier compatibility checks still apply. CLI conversion
and explicit installation are covered by the software regression suite; live
carrier compatibility requires separate acceptance.
