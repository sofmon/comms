# Migrating a local `save` installation to `comms`

The product, command, Go module, application directories, environment variables,
launchd job and logs are now named `comms`. Provider instance ids such as
`gmail:work`, account labels, archive filenames and the archive database schema do
not change.

Do not run the old and new daemons together. Stop the old job before moving the
state directory so SQLite's `state.db`, `state.db-wal` and `state.db-shm` files move
as one unit.

## 1. Stop the old daemon

If the launchd job is installed:

```sh
launchctl bootout "gui/$(id -u)/com.user.save"
```

If it is not installed or is already stopped, launchctl may report that no service
was found; that is harmless. Also stop any manually running `save run`, `save sync`,
`save triage` or `save refetch` process.

## 2. Rename the source checkout

Run this from the directory containing the checkout, not from inside it:

```sh
cd ~/dev
if [ -e comms ]; then
  echo "~/dev/comms already exists; stop and resolve it before moving the checkout"
else
  mv save comms
fi
cd comms
```

The physical checkout name does not affect Go, but renaming it keeps local paths
consistent with the product name.

## 3. Move configuration and credentials

For the default locations, first confirm that the new destination does not already
exist, then move the whole directory:

```sh
if [ -e "$HOME/.config/comms" ]; then
  echo "~/.config/comms already exists; stop and resolve it before moving configuration"
else
  mv "$HOME/.config/save" "$HOME/.config/comms"
  chmod 700 "$HOME/.config/comms"
fi
```

Moving the whole directory preserves `config.toml`, `triage.toml`, OAuth client
files, Google refresh-token files and Fastmail token files together. The credential
filenames themselves have not changed.

If `XDG_CONFIG_HOME` was set, move `$XDG_CONFIG_HOME/save` to
`$XDG_CONFIG_HOME/comms` instead. If the old installation used `SAVE_CONFIG_DIR`,
you may keep that physical directory but must export its path as `COMMS_CONFIG_DIR`.

Edit `~/.config/comms/config.toml` and replace only explicit application-directory
references:

```toml
rules_file = "~/.config/comms/triage.toml"
client_file = "~/.config/comms/google-client-work.json"
```

Do not change `archive_root`, `spam_root`, `timezone`, account labels, account
addresses, or the `gmail`/`chat` switches merely because of this rename.

## 4. Move the SQLite state directory

This step preserves cursors, deduplication rows, Chat history, attachment state,
triage decisions and failure history. Move the directory as a unit:

```sh
if [ -e "$HOME/.local/state/comms" ]; then
  echo "~/.local/state/comms already exists; stop and resolve it before moving state"
else
  mv "$HOME/.local/state/save" "$HOME/.local/state/comms"
  chmod 700 "$HOME/.local/state/comms"
fi
```

If `XDG_STATE_HOME` was set, move `$XDG_STATE_HOME/save` to
`$XDG_STATE_HOME/comms` instead. Do not move `state.db` alone while leaving its WAL
or SHM sidecars behind.

The actual message archive and spam trees do not need to be renamed or moved. Their
paths are read from the migrated config, and the database deliberately keeps the
same `gmail:<label>`, `gchat:<label>` and `fastmail:<label>` keys.

## 5. Rename environment variables

Update shell profiles, launchd environment entries and wrapper scripts:

| Old | New |
|---|---|
| `SAVE_CONFIG_DIR` | `COMMS_CONFIG_DIR` |
| `SAVE_ARCHIVE_ROOT` | `COMMS_ARCHIVE_ROOT` |
| `SAVE_SPAM_ROOT` | `COMMS_SPAM_ROOT` |
| `SAVE_GOOGLE_CLIENT_FILE[_<LABEL>]` | `COMMS_GOOGLE_CLIENT_FILE[_<LABEL>]` |
| `SAVE_GOOGLE_TOKEN_FILE[_<LABEL>]` | `COMMS_GOOGLE_TOKEN_FILE[_<LABEL>]` |
| `SAVE_FASTMAIL_TOKEN[_<LABEL>]` | `COMMS_FASTMAIL_TOKEN[_<LABEL>]` |

The old names are no longer read. Check the current shell before starting `comms`:

```sh
env | grep '^SAVE_'
```

## 6. Build and install the renamed binary

Build from the renamed checkout. Replace `~/dev/bin` if the old binary lived
somewhere else:

```sh
go build -o "$HOME/dev/bin/comms" ./cmd/comms
"$HOME/dev/bin/comms" version
```

Keep the old `save` binary until the verification below succeeds; it can then be
removed or retained temporarily as an emergency rollback binary.

## 7. Replace the launchd job

Copy the new template, then edit its absolute binary path and `CHANGE_ME` log paths:

```sh
cp -i docs/launchd/com.user.comms.plist "$HOME/Library/LaunchAgents/com.user.comms.plist"
```

Validate and load it:

```sh
plutil -lint "$HOME/Library/LaunchAgents/com.user.comms.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.user.comms.plist"
```

After verification, the old `com.user.save.plist` and `save.log` may be kept as
history or removed manually. Never bootstrap both plist files.

## 8. Verify before the first sync

These commands should find the migrated credentials and existing state rather than
starting a fresh archive:

```sh
comms version
comms doctor
comms status
comms verify
```

`comms status` should show the existing per-account cursors and message counts. If
it says there is no state database, stop: the state directory was not moved to the
location selected by `XDG_STATE_HOME` and the default. Fix that before running
`comms sync`, otherwise a full re-enumeration will begin.

No Google or Fastmail reauthorization is required solely because of the rename.
