# WASM changelog

Changes to `ext/wasm`, newest first. A user-visible change adds its line under `## Unreleased` in the same commit.
Each PocketOne release raises this extension's `version:` when there's something under `## Unreleased` and renames the
section to that version ([Changelogs](../../docs/extensions.md#changelogs)).

## Unreleased

### Added

- `ext/wasm`: WASI plugins (Go, Rust, Zig, ...) as `pb_hooks/<name>.pb.wasm`, called from pb_hooks with `await $wasm.run.async(name, input)` and `await $wasm.call.async(name, fn, input)` (async only: a call runs off the app's JS VM); each describes itself (`<name> --describe`). Input in, output out, no access to the database, files, network, environment or processes.
