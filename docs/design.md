# Salt design

Salt encrypts AI-agent memory (Hermes and Mnemosyne today; Honcho, Hindsight
and OpenViking later) before it is backed up to Git.

## Principles

- **Nothing unencrypted enters the backup repo.** `salt seal SRC REPO` writes
  only encrypted files. The pre-commit hook `salt check` blocks any staged file
  that is not encrypted, except a short list of public files.
- **Encrypting never needs anything secret.** Sealing uses only the public
  key. The private key is only needed to restore.
- **File names are hidden by default.** Turn this off with
  `salt init --plain-paths`.
- **Files are processed a piece at a time**, never loaded whole into memory.
- **Unchanged files are not re-encrypted**, so an unchanged snapshot makes no
  commit.

## Example usage

A backup script saves a snapshot of the agent files, then runs `salt seal` to
encrypt them before they reach the git repo:

```bash
salt seal --prune "$STAGE" "$REPO" 2>>"$LOG" || die "salt seal failed"
```

`--prune` removes files in the repo that salt did not write. Salt prints
nothing to stdout, so a Hermes `--no-agent` job stays silent on success.

## Backup repo layout

| Path | Contents |
|---|---|
| `.salt/format.json` | settings (public) |
| `.salt/recipients.txt` | public keys every file is encrypted to |
| `.salt/key.age` | passphrase-locked private key (passphrase recovery only) |
| `index.age` | encrypted list of real file names, sizes and hashes |
| `objects/…` | encrypted files under random names (default) |
| `files/…` | encrypted files under real names (`--plain-paths`) |

## Keys and recovery

The private key is kept in the OS keychain. `SALT_KEYSTORE=file` uses a
private file instead, for machines without a keychain. At `salt init` the user
picks how to recover the key if the laptop is lost:

- **Recovery phrase (recommended):** 12 words that are the key. Nothing
  secret is stored in the repo. Setup shows the words, hides them, and asks for
  all 12 back. A mistake restarts the step, and nothing is saved until it
  passes. `salt recovery show` shows the phrase again later.
- **Passphrase:** chosen by the user and stored as `.salt/key.age`. Weak
  passphrases are rejected.

### Onboarding copy (use verbatim)

```
How do you want to recover your backups if this laptop is lost?

  1) Recovery phrase  (recommended)
     salt generates 12 words for you to write down.
     The words are your decryption key. With this method, no decryption key
     is stored in your backup repo.

  2) Passphrase
     You choose a passphrase. An encrypted copy of your key is stored in your
     backup repo. Ensure it is strong because anyone with access to the repo
     can try to guess your password. Consider using a password manager like
     Bitwarden.
```

```
Write these down, in order.
Anybody with these words can decrypt and read your backups.
If you lose them and this laptop, your backups cannot be recovered.
```

## Restore and checks

- `salt restore` decrypts into a temporary folder, checks every file, then
  moves it into place. An existing folder is moved aside, never overwritten.
- `salt verify` decrypts everything without writing it to disk, and reports
  any file that cannot be restored. It needs the key.
- `salt doctor` checks the hook, the key, the repo and the last backup. It
  needs no key.

## Security

Salt protects backups from anyone who can read the repo. It also guards
against someone who can push to it:

- **Added keys.** The repo's key list and settings are public files. `salt
  init` saves an approved copy on the user's machine, and `salt seal` refuses
  if the repo's copy differs. `salt trust` approves a genuine change, and
  approves a repo cloned onto a new machine. It warns before asking about any
  key not stored on the machine, and about visible file names.
- **Hiding plaintext from the hook.** `salt check` reads staged files as
  `:0:<path>`, so a file named like `0:x` can't hide behind `x`.
- **Symlinks.** Salt never creates symlinks where it keeps data (`.salt/`,
  `index.age`, `objects/`, `files/`). Seal, restore and verify refuse to run
  if one is there, whether it points outside the repo or back inside it. As a
  second guard, every read and write goes through `os.Root`, which refuses
  paths that lead outside the repo.
- **Tampered index.** The index is read one entry at a time, capped at 100,000
  entries and 32 MB, so a crafted index can't use much memory.

Not yet covered: someone who can push can still plant a fake encrypted file.
The signed index (#7 on the board) will catch that.

## Process and memory safety

An earlier attempt crashed the machine when tests kept restarting themselves.
These rules prevent that:

1. The hook runs `salt` by name, never a file path. `os.Executable()` is
   banned.
2. Salt refuses to start inside another salt (`SALT_ACTIVE`).
3. Every git command salt runs has git hooks switched off (`internal/gitx`).
4. Only `internal/gitx` and `internal/source` may start other programs. Unit
   tests never start any. `internal/rules` enforces rules 1 and 4.
5. End-to-end tests run only through `make e2e`. It builds salt once, caps
   the number of processes, and keeps the tests away from the real keychain.
6. At most 4 files are worked on at once, with a 512 MB soft memory limit.

## Databases

Salt encrypts any file you give it, and that includes database files. Mnemosyne
keeps its memory in a SQLite file (`mnemosyne.db`). Salt encrypts that file
today, the same way it encrypts a Markdown file.

A database must be backed up before it is encrypted. For now the user must do
this themselves, using the database's own backup tool, before running
`salt seal`:

- SQLite (Mnemosyne, Hermes): `sqlite3 live.db ".backup 'copy.db'"`
- Postgres (Honcho, Hindsight): `pg_dump`

A plain `cp` of a database in use can give a broken copy with no warning.
This is a problem because Salt encrypts the broken copy exactly as it is.
Decrypting it later gives back the same broken database, and you only find
out when you try to restore it. For safety, Salt will soon make these backups
itself. Salt only encrypts the
copies it gets from the databases.

## Out of scope

Touch ID, and switching recovery method.

## Still to build

- Salt making safe copies of live databases by itself (SQLite and Postgres),
  so a backup script no longer has to. Salt already encrypts database files.
- OpenViking support. Its data format has not been checked yet.
- a one-command `salt backup`
- splitting files over GitHub's 100 MB limit
- a signed index, to detect planted files
