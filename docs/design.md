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
| `index.age` | encrypted list of real file names, sizes, hashes and last-modified dates |
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
  Each file gets back the last-modified date it had when it was sealed. Seal
  records the date of the file it is given, so a backup script must keep the
  original dates when it copies files (`cp -p`, and `touch -r` after
  `sqlite3 .backup`). A file whose only change is its date keeps its
  ciphertext; only `index.age` is rewritten. Backups made before salt recorded
  dates restore with the time of the restore, and the first seal with a salt
  that records dates rewrites `index.age` once to add them.
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
  entries and 32 MB, so a crafted index can't use much memory. `salt verify`
  lists at most 50 problems, with long paths shortened.
- **Ignore rules and attributes.** `.gitignore` and `.gitattributes` are
  public files, so someone who can push can add a rule that makes git skip new
  encrypted files, or rewrite them when storing or checking them out (for
  example `*.age text`). The backup then looks fine locally but can't be
  restored from the remote. `salt seal` (after writing), `salt check` and
  `salt doctor` ask git directly, so rules from the global ignore file,
  `.git/info/exclude` and `core.attributesFile` count too. They refuse if git
  ignores any file salt wrote, or if any `.age` file does not have `text`
  unset or has an `eol`, `filter`, `working-tree-encoding` or `ident`
  attribute.
- **Interrupted restores.** A restore decrypts into a temporary folder next to
  the destination. Ctrl-C or SIGTERM removes it. A restore killed outright
  can't clean up, so the folder is recorded while the restore runs, and
  `salt doctor` and the next `salt restore` report any left behind.

Not yet covered: someone who can push can still plant a fake encrypted file.
The signed index (#7 on the board) will catch that.

## What the repo reveals

The backup repo can be private or public. A private repo is recommended, and
salt works the same way in either. In this document a "public" file means a
file salt leaves unencrypted, such as `.salt/recipients.txt`, not a file
anyone on the internet can see. Keeping the repo private means only you and
the people you give access to can see what is listed below.

Anyone who can read the repo cannot read your files, but they can learn some
things about them:

- **How many files there are.** Each file is one encrypted object, so the
  number of objects is the number of files. Symlinks are kept only in the
  index.
- **Roughly how big each file is.** Files are compressed, then encrypted, and
  encryption adds a small overhead (a short header and 16 bytes per 64 KiB).
  An object's size is therefore close to the file's compressed size, which
  also shows how well it compresses. The size of `index.age` roughly shows how
  many files there are and how long their names are.
- **What changed, and when.** An unchanged file keeps its encrypted object, so
  an unchanged backup makes no commit. A changed file's old object is removed
  and a new one added in the same commit. A commit that changes only
  `index.age` shows that a file's last-modified date or permissions changed
  but no file's contents did. From the history, a reader can see
  when backups ran, how many files changed each time, and, by matching sizes,
  how one file such as a growing database changes over time.
- **How many keys can decrypt the backups.** `.salt/recipients.txt` is public,
  so a reader can see how many keys the backups are encrypted to.
- **File names, with `--plain-paths`.** Objects are stored under their real
  names, so file names, folders and each named file's size are visible.
  `salt trust` warns about this.

Salt does not hide these. Hiding them would mean padding every file and
re-encrypting unchanged ones, which would make the repo larger and create a
commit every night.

## Process and memory safety

If salt starts git, and git runs the hook that starts salt again, each run can
start another until the machine runs out of memory. These rules prevent that:

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
