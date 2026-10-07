# Salt design

Salt encrypts AI-agent memory (Hermes and Mnemosyne today; Honcho, Hindsight
and OpenViking later) before it is backed up to Git.

## Principles

- **Nothing unencrypted enters the backup repo.** `salt seal SRC REPO` writes
  only encrypted files. The pre-commit hook `salt check` blocks any staged file
  that is not encrypted, except a short list of public files.
- **Encrypting never needs the decryption key.** Sealing uses the public key
  and a signing key that can sign but not decrypt. Only restoring needs the
  private key.
- **File names are hidden by default.** Turn this off with
  `salt init --plain-paths`.
- **Files are processed a piece at a time**, never loaded whole into memory.
- **Unchanged files are not re-encrypted**, so an unchanged snapshot makes no
  commit. A large file is sealed in chunks, and only the chunks that changed
  are encrypted again.

## Example usage

A backup script saves a snapshot of the agent files, then runs `salt seal` to
encrypt them before they reach the git repo:

```bash
salt seal --prune "$STAGE" "$REPO" 2>>"$LOG" || die "salt seal failed"
```

`--prune` removes files in the repo that salt did not write. Salt prints
nothing to stdout, so a Hermes `--no-agent` job stays silent on success.

After committing, the script runs `salt prune "$REPO"` to drop old backups
from the history, then pushes with `git push --force-with-lease` (see
"Keeping only recent backups").

## Backup repo layout

| Path | Contents |
|---|---|
| `.salt/format.json` | settings (public) |
| `.salt/recipients.txt` | public keys every file is encrypted to |
| `.salt/key.age` | passphrase-locked private key (passphrase recovery only) |
| `index.age` | encrypted, signed list of real file names, sizes, hashes and last-modified dates |
| `objects/…` | encrypted files under random names (default), and the chunks of a large file (see "Large files") |
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

### Signing key

Anyone can encrypt to the public key, so salt signs every index it writes
(see "Planted files" under "Security"). The signing key is an Ed25519 key
derived from the age private key with HKDF-SHA256 (`keys.SigningKey`). The
derivation is one-way. The signing key can sign but not decrypt, and does not
reveal the private key. Its salt and info strings are pinned by a test, like
the recovery phrase's, because changing them would make every existing backup
fail its signature check.

The signing key is kept apart from the private key, in a 0600 file per age
key in the OS config folder (`salt/signing/`). A scheduled `salt seal` reads
only that file, never the keychain. `salt init` saves it. On another machine,
`salt trust` saves it, deriving it from the private key in the keychain or,
if there is none, from the recovery phrase or passphrase it asks for. Without
it, `salt seal` refuses and says to run `salt trust`, and `salt doctor` warns.

Because it is derived from the private key, a new machine needs only the
recovery phrase or passphrase to check a backup. The key that decrypts a
backup also gives the public key that checks its signature.

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
- Each file gets back the last-modified date it had when sealed. Seal records
  the date of the file it is given, so a backup script must keep the original
  dates when it copies files (`cp -p`). `--sqlite` databases get the live
  database file's date. `--postgres` dumps have no date of their own, so they
  restore with the time of the restore (see "Databases"). A file whose only
  change is its date keeps its ciphertext, and only `index.age` is rewritten.
  Backups made before salt recorded dates restore with the time of the
  restore. The first seal by a salt that records dates rewrites `index.age`
  once to add them.
- `salt restore` also takes the backup repo's https or ssh URL, or its SSH
  form (`git@github.com:you/backup.git`). Other URLs are refused. `http://`
  would send a password or token in the clear, and a folder is given by its
  path. Salt downloads only the latest backup on the repo's default branch
  (`git clone --depth 1`, hooks off) into a private `salt-download-*` folder
  in the home folder. It restores from that as from a local repo, then
  removes it, whether the restore worked or not. The download holds only
  encrypted files, and nothing is decrypted until it is complete. Messages
  never show a user name or password given in the URL. They name the URL,
  not the download's folder, which is gone by the time they are read. A
  download that could not be removed, or that a killed salt left behind, is
  reported by the next restore from a URL and by `salt doctor`.
- `salt restore` and `salt verify` first check that the index is signed by
  the signing key of one of the keys that decrypts it, and refuse it if not.
  `--allow-unsigned` goes ahead with a warning, for example to look at a
  backup someone else replaced. Every file is still checked against the
  index.
- `salt verify` decrypts everything without writing it to disk, and reports
  any file that cannot be restored. It needs the key.
- `salt doctor` checks the hook, the key, the repo and the last backup. It
  needs no key.

## Large files

GitHub refuses any file over 100 MiB and warns about any over 50 MB, so salt
keeps every file it writes far below both.

- **Up to 16 MiB:** one encrypted object, as always.
- **Over 16 MiB:** the file is cut into chunks of plaintext, and each chunk
  is compressed and encrypted as its own object under `objects/`, even with
  `--plain-paths`. A chunk is 1 to 16 MiB, about 4 MiB on average.

A file is measured each time it is sealed, so one that shrinks to 16 MiB or
less is one object again, and one that grows past 16 MiB is chunked.

Chunk boundaries follow the content (FastCDC, with a rolling hash over the
last 64 bytes). A change in one place changes only the chunk around it, and
bytes inserted or removed move only the boundaries near them. So both a
database whose pages change in place and a SQL dump with rows added keep
most of their chunks. Salt's change cache records the SHA-256 of each
chunk's plaintext. The next seal reads the file once, and any chunk it has
sealed before, in this file or another, keeps its object. Only new chunks
are encrypted and pushed. In tests, a change of a few KiB added 1.1 new
chunks on average and never more than 4.

A change to a database is rarely in one place. In a 320 MB SQLite test
database, updating one row of an indexed table rewrote 19 pages spread
across the file, the header included. Such a change adds several chunks,
still far less than the whole file.

The rolling hash adds up numbers from a table derived from the signing key
(HKDF-SHA256), so where the boundaries fall differs for every key, and a
reader of the repo cannot work it out. Without this, the sizes of a file's
chunks could help tell which known file it is. Changing the derivation
would move every boundary, so each large file would be encrypted again in
full once. Restore never needs the table.

Each chunk is a complete zstd frame in its own age file, and zstd reads
frames one after another as one stream. The encrypted index lists each
file's objects in order, and restore and verify decrypt them in turn,
opening one at a time, so memory stays flat. Sealing holds one chunk in
memory per worker, and reuses zstd encoders from one chunk to the next. An
index with a file in parts or chunks is written as version 2, so a salt from
before parts, which would read only the first object, refuses it. Any other
index stays version 1. A salt that reads parts reads chunks too, the same
way.

Older salts split a file over 99 MiB into parts of 45 MiB of compressed
data. Salt still restores and verifies them,
and keeps an unchanged file's parts. Once such a file changes, it is sealed
in chunks. A file of 16 MiB or less that grows past 99.5 MiB of compressed
data while it is sealed, as a live file can, still has the rest put into a
second part. A cached object over 100 MiB, which an older salt wrote for a
large file, is never reused, so that file is sealed again even if it has
not changed.

The change-detection cache lists every object of a file in parts or chunks
and leaves its single-object fields empty. An older salt using the same
cache then finds nothing to reuse and encrypts the file again, rather than
keeping only its first object.

A file's own chunks from its last seal are looked up before those of other
files, so an unchanged file always keeps its own objects, even when another
file holds the same chunks under other objects. A chunk lost from the repo
is the only one encrypted again on the next seal. A seal that fails part way
through a file removes the chunks it wrote for it, so a full disk is not
left fuller for the next try. The files it finished before the failure keep
what they wrote: salt saves their entries in the change cache, so the next
seal reuses a large file's chunks instead of encrypting it again. Nothing
else is removed, and the next seal that succeeds removes what no index
needs. The saved entries also record what a `--plain-paths` object replaced
in place now holds, so it is never kept for the content it held before.

`salt doctor` warns about any file over 100 MiB in the repo, which only an
older salt or a person could have put there.

Every git command salt runs turns off git's compression and its delta search
for similar objects to store as differences (`core.looseCompression=0`,
`pack.compression=0` and `pack.window=0`). Encrypted data never shrinks, and
age encrypts each file with a new random key, so two versions of a file share
nothing. Both would only cost time, most of all when committing and pushing a
large changed file. The settings are passed on the command line, so they also
reach the programs git starts during a push. Each kind of compression is set
by name, because a person's own setting for one would otherwise override
`core.compression`. Nothing in the repo changes, so existing repos get this at
once, and git reads objects stored either way. git commands run by hand in
the repo keep git's defaults.

Each backup that changes a large file still adds its changed chunks to
history. `salt backup` and `salt prune` keep every backup made on the latest
day with a change, and only the last backup of each earlier day (see
"Keeping only recent backups"), so backups made many times a day do not
multiply what history holds. Before pushing, `salt backup` warns when the
push would send more than 1 GiB, or when a file added more than 500 MiB of
new encrypted data, naming the file and never showing what it holds. The
push still goes ahead. The push is measured (`git rev-list --disk-usage`)
before old backups are dropped, while the commit origin holds is sure to be
in the local repo. So after pushes that failed it can also count backups
prune is about to drop. Measuring the push needs git 2.31 or later. When git
cannot measure it, only the files are named.

GitHub refuses a push over 2 GB, recommends keeping a repo under 1 GB, and
strongly recommends keeping it under 5 GB (its figures as of October 2026).
A first backup of several large databases can go over the push limit, and
a large database that changes in many places every day can go past the
recommended size in a few days. This applies to any large database, whatever
tool made it. Back up a large database once a day.

## Keeping only recent backups

Encrypted files can't be compressed against their earlier versions, so every
change adds the changed file's full size to the repo, or for a large file
the size of its changed chunks. `salt prune REPO` stops
the repo growing forever by dropping old backups from its history.

### What "5 days" means

`salt prune` keeps the backups from the last **5 days with a change**
(`--keep-days N`, 5 by default). This is not the same as the last 5 calendar
days:

- **Days are counted for the whole repo, never per file.** A backup is a
  commit, and every commit is a complete snapshot of every file, so salt
  cannot keep one file's history longer than another's.
- **A day counts only if something in the repo changed that day.** A change
  to any file, or to any file's last-modified date or permissions, makes a
  commit. A night with no change makes no commit, so that day does not count.
- **So each day with no change makes the window one calendar day longer.**
  With one quiet day, 5 days with a change reach back 6 calendar days. With 3
  quiet days they reach back 8.
- **Several backups on the same day count as one day.** All are kept on the
  latest day with a change. On each earlier day only the last one is kept,
  so backups made many times a day do not multiply what history holds.
- A backup's date is its commit date, in the time zone of the machine that
  made the commit.

An example, keeping 5 days, with the backup running every morning:

| Day | 10th | 11th | 12th | 13th | 14th | 15th |
|---|---|---|---|---|---|---|
| Anything changed? | yes | yes | yes | **no** | yes | yes |
| Commit made? | yes | yes | yes | no | yes | yes |
| Kept on the 15th? | yes | yes | yes | (none) | yes | yes |

The 5 days kept are the 10th, 11th, 12th, 14th and 15th. That is 6 calendar
days, because nothing changed on the 13th. Anything older is dropped.

What this means for a file that changes once a week (`USER.md` changed on the
9th, then on the 16th), while `MEMORY.md` changes every day:

- The current version of every file is always kept, because it is in the
  latest backup, which is never dropped.
- An older version stays restorable as long as a kept backup still holds it.
  On the 18th, with daily changes, the kept backups are the 14th to the 18th.
  The 14th and 15th hold `USER.md` from the 9th, and the 16th to the 18th hold
  the version from the 16th. On the 20th, every kept backup holds the version
  from the 16th, and the version from the 9th is gone.
- If nothing else in the repo changes, there is a commit only when `USER.md`
  changes. The last 5 days with a change are then the last 5 weeks, and all 5
  versions are kept.

Counting days with a change, rather than calendar days, means a quiet spell
or a stalled backup job never leaves only one backup. The first backup after
a two-week gap still keeps the 4 days with a change before the gap. The
trade-off is that the span kept is not fixed. It grows when the repo changes
less often.

`--keep-days 1` keeps only the backups from the latest day with a change.
The latest backup is never dropped.

To keep a database for longer than prune does, keep a full copy under a dated
name in the folder that is backed up, such as `memory-2026-09-30.db` or a
Postgres dump `memory-2026-09-30.sql`. Salt encrypts whatever files are
there, so this works for any database.

### How it works

Each kept backup's commit is copied exactly, with the same files, author,
dates and message. Only its parent changes, so the oldest kept backup becomes
the first commit, and every kept backup restores exactly as before. Commit
signatures are removed from the copies, because they no longer match. A merge
commit among the kept backups is copied onto the line kept, so the history it
merged in is dropped but its files are kept.

Salt then deletes the dropped backups from the local repo. It empties git's
reflogs (except the stash's), which would otherwise keep them for 30 to 90
days, and runs `git gc --prune=now`. This also removes git's local undo for
them. Git can't delete what `origin/<branch>` still points at until the force
push updates it, so those go on the next prune.

A prune is all or nothing. Every copied commit is written first. The branch
then moves to the copies in one step, and only if it still points where it
did, so a commit made in the meantime is never lost. If anything fails before
that step, the branch is left as it was. Deleting the dropped backups comes
after the branch has moved. If that fails, every kept backup is still
complete, so salt prints a warning and exits 0 so a backup script still
pushes. The next prune that drops something deletes them.

A night with nothing to drop changes nothing.

### Rewriting history and force pushing

Dropping backups rewrites the branch's history, so a plain `git push` is
refused afterwards. Push with:

```bash
git push --force-with-lease
```

`--force-with-lease` only overwrites the remote if it still holds what this
machine last saw there, so a backup pushed from another machine in the
meantime is not lost. `salt prune` never pushes by itself. `salt backup`
pushes this way after it prunes (see "One-command backup").

- **Other clones.** Any other clone of the backup repo, such as on another
  laptop, still has the old history. Before backing up from it, run
  `git fetch` then `git reset --hard origin/<branch>`, or clone it afresh.
- **GitHub may keep dropped backups for a while.** After the force push,
  GitHub can still serve dropped commits to anyone who knows their hash, and
  can keep them in caches, forks and pull request references until it cleans
  up on its own schedule. GitHub support can remove them sooner. The dropped
  backups are encrypted like every other backup.

### Only the backup repo salt was set up in

`salt prune` refuses, and changes nothing, unless:

- the folder is a salt repo (it has `.salt/format.json`) and is the top of its
  git repository, so a folder inside another repository never has that
  repository rewritten;
- this machine has approved the repo's keys and settings (see "Security"). If
  someone else changed them, the history that shows the change must not be
  rewritten away;
- a branch is checked out (not a detached HEAD), and only that branch is
  rewritten;
- the clone is not shallow, so salt can see the date of every backup it
  might drop. Run `git fetch --unshallow` first.

Other branches and tags are left alone. Any that still point at old backups
keep them in the repo.

## Security

Salt protects backups from anyone who can read the repo. It also guards
against someone who can push to it:

- **Added keys.** The repo's key list and settings are public files. `salt
  init` saves an approved copy on the user's machine, and `salt seal` refuses
  if the repo's copy differs. `salt trust` approves a genuine change, or a
  repo cloned onto a new machine. It warns before asking about any key not
  stored on the machine, and about visible file names.
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
  public files. Someone who can push can add a rule that makes git skip new
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
- **Planted files.** Anyone can encrypt a file to the public key, so someone
  who can push could replace `index.age` and add objects of their own, which
  would decrypt like real ones. Seal signs the index with the signing key
  (see "Signing key"). Restore and verify refuse an index not signed by the
  key derived from one of the keys that decrypts it. The signature is inside
  the encrypted index, so it reveals nothing and adds no public file. It
  covers the index's version and each entry in order, hashed with SHA-512 one
  entry at a time as the index is read, so checking it never holds a second
  copy of the index. Ed25519 signatures are deterministic, so an unchanged
  backup still makes no commit. An object the index does not list is never
  restored. `salt verify` lists it and the next `salt seal` removes it.

The signature does not stop someone who can push from putting back an older
backup from the repo's history, since that backup is still genuinely signed
with your key.

Each key in the repo signs with its own signing key, and a backup is accepted
only if it was signed with a key the restoring machine holds. With several
keys, each machine can therefore check that a backup came from its own key.

## What the repo reveals

The backup repo can be private or public. A private repo is recommended, and
salt works the same either way. In this document a "public" file means one
salt leaves unencrypted, such as `.salt/recipients.txt`, not one anyone on
the internet can see. In a private repo, only you and the people you give
access to can see what is listed below.

Anyone who can read the repo cannot read your files, but can learn some
things about them:

- **How many files there are.** Each file is one encrypted object, except a
  file over 16 MiB, which is one object per chunk of about 4 MiB (see "Large
  files"). So the number of objects shows roughly how many files there are
  and how big the large ones are. Symlinks are kept only in the index.
- **Roughly how big each file is.** Files are compressed, then encrypted, and
  encryption adds a small overhead (a short header and 16 bytes per 64 KiB).
  So an object's size is close to the file's compressed size, which also shows
  how well it compresses. The size of `index.age` roughly shows how many files
  there are and how long their names are.
- **What changed, and when.** An unchanged file keeps its encrypted object, so
  an unchanged backup makes no commit. A changed file's old object is removed
  and a new one added in the same commit. A commit that changes only
  `index.age` shows that a file's last-modified date or permissions changed,
  but no file's contents. From the history, a reader can see when backups
  ran, how many files changed each time, and, by matching sizes, how one file
  such as a growing database changes over time. For a file in chunks, a
  reader sees which chunks were replaced, which shows when the file changed,
  roughly where in it and roughly how much, but not what it holds. `salt
  prune` limits this to the backups it keeps.
- **How many keys can decrypt the backups.** `.salt/recipients.txt` is public,
  so a reader can see how many keys the backups are encrypted to.
- **File names, with `--plain-paths`.** Objects are stored under their real
  names, so file names, folders and each named file's size are visible.
  `salt trust` warns about this.

Salt does not hide these. That would mean padding every file and
re-encrypting unchanged ones, making the repo larger with a commit every
night.

## Process and memory safety

If salt starts git, and git runs the hook that starts salt again, each run can
start another until the machine runs out of memory. These rules prevent that:

1. The hook runs `salt` by name, never a file path. `os.Executable()` is
   banned.
2. Salt refuses to start inside another salt (`SALT_ACTIVE`).
3. Every git command salt runs has git hooks switched off (`internal/gitx`).
   Compression and the delta search are off too (see "Large files").
4. Only `internal/gitx`, `internal/source` and `internal/proc` may start other
   programs. `internal/proc` runs the ones salt may stop part way (`git
   clone`, `git ls-remote`, `git rev-list`, `git push`, `git commit`,
   `sqlite3` and `pg_dump`). It keeps at most 4 KiB of their error output, and waits at
   most 5 seconds for the output of one that was stopped, since a program it
   started can hold that output open. Unit tests never start any.
   `internal/rules` enforces rules 1 and 4.
5. End-to-end tests run only through `make e2e`. It builds salt once, caps
   the number of processes, and keeps the tests away from the real keychain.
6. At most 4 files are worked on at once, with a 512 MB soft memory limit.
   A file sealed in chunks holds one chunk, at most 16 MiB, in memory at a
   time.
7. Only one salt seals, backs up or prunes a repo at a time. Each takes a
   lock (`flock`) on `.git/salt/lock` in the repo, and a second one stops at
   once instead of waiting. The lock is never committed. It is the same file
   whatever the environment, so a cron job and a shell, which can have
   different cache folders, still share it. A repo with no `.git` folder,
   which `salt seal` accepts, is locked through a file in salt's cache folder
   named after the repo instead. The system drops the lock when salt ends,
   however it ends. Without it, a backup still pushing when the next one
   starts would delete the objects the other had just written.

## Databases

Salt encrypts database files like any other file, such as Mnemosyne's SQLite
file `mnemosyne.db`.

A database in use must be copied safely before it is encrypted. A plain `cp`
can give a broken copy with no warning. Salt would encrypt it exactly as it
is, and you would only find out when you restore it.

For SQLite, `salt seal --sqlite DB SRC REPO` makes the copy itself
(`internal/source`). It runs `sqlite3 -init /dev/null -bail "file:DB?mode=rw"`
inside a new private temporary folder, and sends it `.timeout 30000` and
`.backup N` on stdin, one per line. With `-bail`, a command that fails stops
`sqlite3` with an error. The commands are not given with `-cmd`, because
`sqlite3` 3.53 with `-bail` stops after the first SQL command given that way
and exits 0 without a copy. If `sqlite3` exits 0 without making a copy
anyway, salt stops and says so. Salt seals the copy at the top of the backup
under DB's file name. It always removes the folder, also when the copy or the
seal fails or salt is stopped with Ctrl-C or SIGTERM.

SQLite's backup gives a consistent copy while other programs write, and
copies page by page, so nothing is held in memory. It starts again whenever
another program saves to the database, so a large, busy database might never
finish. For a database in WAL mode (header bytes 18 and 19 are 2), salt sends
`BEGIN;` and `SELECT count(*) FROM sqlite_master;` before `.backup`. The read
transaction makes the backup copy the database as it was at that moment, so
it never starts again, and other programs keep saving to the `-wal` file. In
rollback-journal mode a read transaction would make other programs wait to
save until the backup ends, so salt leaves it out. The agent never waits for
salt, and a save during the copy only makes the copy start again. Two copies
of an unchanged database are identical, so an unchanged database makes no
commit.

Before starting `sqlite3`, salt refuses a path that is missing or is not a
SQLite database, because `sqlite3` would create an empty database at a
missing path. It also opens DB as a URI with `mode=rw`, escaping characters
such as `?` and `%`. So a database deleted after that check, as a tool may do
while salt backs it up, is an error, and is never made again, empty, in the
tool's folder.

For Postgres, `salt seal --postgres CONN SRC REPO` dumps the database with
`pg_dump --no-password --format=plain --lock-wait-timeout=30000
--file=N --dbname=CONN` into the same kind of private temporary folder. It
seals the dump at the top of the backup as the database's name with `.sql`.
`pg_dump` reads the whole database in one transaction, so the dump is
consistent while other programs write. It streams the dump to the file, so
nothing is held in memory. Plain SQL restores with `psql` alone, and zstd
compresses it well.

CONN is a libpq URL or a string of libpq settings. A driver in a URL's scheme
(`postgresql+psycopg://`), as SQLAlchemy writes it, is dropped. `--postgres-env
VAR` reads CONN from an environment variable, which is where agents usually
keep it, so the password is never on salt's command line. `pg_dump` comes
with Postgres, and its version must be the same as the server's or newer.
`sqlite3`, for `--sqlite`, comes with macOS, and on Linux is installed with
the package manager. CI's macOS e2e job tests Homebrew's `sqlite3`, not
Apple's `/usr/bin/sqlite3`.

Salt takes the password out of CONN and gives it to `pg_dump` in
`PGPASSWORD`, so it never shows in a process list or a message. Without one,
`pg_dump` looks in `~/.pgpass`, `PGPASSFILE` and `PGPASSWORD` as usual.
`--no-password` stops `pg_dump` from waiting for someone to type a password.
Unless the person set `PGCONNECT_TIMEOUT`, salt sets it to 30 seconds, so a
server that cannot be reached fails the backup instead of stalling it. A
table another program has locked fails the dump after 30 seconds. A dropped
connection or any other `pg_dump` error stops salt before sealing, with
`pg_dump`'s reason. A connection with no database name uses `PGDATABASE`, as
libpq does. Without that it is refused, rather than letting `pg_dump` guess
one.

A dump has no last-modified date of its own, so none is recorded, and it is
owner-only (0600). Recent `pg_dump` releases (18, and 17.6, 16.10, 15.14,
14.19 and 13.22) write a new random key into every plain dump (`\restrict`),
which would make every dump differ. So salt gives `pg_dump --restrict-key`
when `pg_dump --help` lists it. The key is random, made once per backup repo
and kept in salt's cache folder (`copykey-<hash>`, 0600, never committed). It
stays secret, so the protection it gives on restore still holds. If the cache
is lost, a new key only means one more commit. An unchanged database then
gives an identical dump and makes no commit.

Every kind of database is a `source.Database`, which says what the copy is
called, how to show the database in messages without a password, and how to
make the copy. `source.Kinds` maps each `salt seal` option to one, so adding
another database means adding one type and one entry there. Salt's commands
handle every kind the same way: the temporary folder, cleaning up, name
clashes, Ctrl-C, and the per-repo key.

### Database names

Each kind picks the name its copy is backed up under. A SQLite file keeps its
file name, and a Postgres dump is the database's name with `.sql`. Two
databases can have the same name, such as two agents that each keep a
`state.db`, or two servers that both use Postgres's default database,
`postgres`. Salt never asks anyone to rename their own data, so `--name NAME`
backs up the database from the next database option as NAME instead. Other
options and SRC or REPO may come between them, but it reads best just before:

```bash
salt seal --sqlite ~/agent1/state.db --name agent2/state.db --sqlite ~/agent2/state.db SRC REPO
salt seal --name honcho.sql --postgres-env HONCHO_DB SRC REPO
```

NAME is a slash path in the backup, so it can put the copy in a folder, beside
that agent's other files. It works the same for every kind, through
`source.Named`, which changes only the name. Without `--name`, nothing
changes. These are usage errors, refused before anything is copied:

- a NAME that could lead outside the backup (empty, absolute, with `..` above
  the top, or with a backslash);
- a NAME ending in `/`, which names a folder rather than the file the copy is
  backed up as;
- a `--name` not followed by a database option.

Two names clash when they are the same, or when one is a folder above the
other, since a file cannot also be a folder. Names are compared exactly, case
included, as salt keeps every name as it was given. `state.db` and `State.db`
are two names, and both are backed up. macOS and Windows usually ignore case,
so such a backup restores in full only where case counts, such as Linux.
Elsewhere restore refuses rather than write one file over the other. With
`--plain-paths` each name is also a file name in the repo, which macOS and
Windows would keep as one file, silently losing one of them, and the repo may
be cloned onto either. So there, names that differ only by case clash too,
and the message says so. Two files in folders whose names differ only by case
keep their own names and do not clash.

Two databases whose names clash are refused before any copy is made, since a
dump can take a long time. The message names both and points to `--name`. A
name that clashes with a file or folder in SRC is only known once salt reads
SRC, so that clash is refused after the copies are made, which are then
removed, with the same advice. `seal.FirstClash` finds the clash for both
checks, by looking each name up rather than comparing every pair.

The backup gets the live database file's permissions and last-modified date.
They are read before the copy, because the copy can change them. After a
crash, `sqlite3` moves a leftover `-wal` file's changes into the database
file when it closes. Salt takes that date as it is, in either SQLite journal
mode:

- Rollback journal (`delete`, `truncate`, the default): every change is
  written into the database file, so its date is the date of the last change.
- WAL (`wal`, used by Hermes and Mnemosyne): changes go to `live.db-wal`
  first and reach `live.db` only at a checkpoint, so `live.db` can be older
  than the last change. This is a property of the database, and Salt does
  not try to work around it.

### Restoring a Postgres database

`salt restore` gives back the dump. `psql` loads it into a new, empty
database.

```bash
salt restore ~/my-backup-repo --to ~/restored-files memory.sql
createdb memory_restored
psql -X -v ON_ERROR_STOP=1 --single-transaction -d memory_restored -f ~/restored-files/memory.sql
```

The dump recreates every table, row and extension, but the new server must
already have the extensions, such as `pgvector`, and every database
user the dump names. With `ON_ERROR_STOP` and `--single-transaction`, a
failed restore stops at the first error and leaves the new database empty.

## One-command backup

`salt backup --preset NAME REPO` does a whole nightly backup with no script.
It is meant for a cron line, so it prints nothing when it works. In order, it:

1. opens REPO and refuses, as `salt seal` does, if this machine has not
   approved its keys or has no signing key. It also refuses, before any work
   is done, if REPO is not a git repo or has no remote named `origin`;
2. gathers what each preset names (see "Presets"), and refuses if a preset
   finds nothing, naming where it looked. A place the last backup held that
   is not found this time is named in one line each, such as a folder on a
   drive that is not mounted, or one pointed to by a variable set in the
   person's shell but not in cron. The backup goes on without it, and its
   earlier copies stay in history until prune drops them. The last backup's
   paths come from salt's change cache;
3. makes a safe copy of each SQLite database found, as `--sqlite` does, and
   seals the copies and every other file found into REPO, which then holds
   only them. Anything else in REPO is removed, as with `salt seal --prune`;
4. stages everything with `git add --all`, runs the pre-commit hook's check
   (`salt check`) inside salt, and commits as `salt backup` if anything
   changed. The commit runs with git hooks off, like every git command salt
   runs (see "Process and memory safety"), so salt runs the check itself. It
   is never signed, whatever `commit.gpgsign` says. It holds only ciphertext,
   its index is signed with salt's own key, and a signing key that asks for a
   passphrase would stop every scheduled backup;
5. reads where origin's branch is (`git ls-remote`), and goes on only if that
   is nothing, a commit the local branch holds, or a commit this machine
   pushed or tried to push (kept in `.git/salt/pushed.json`, never
   committed). This is checked before prune rewrites the branch, while the
   branch still holds the commit this machine last pushed and any commit
   pushed by hand. The remote-tracking branch never counts, since a fetch
   moves it to whatever another machine pushed;
6. warns when the push would send more than 1 GiB, or when a file added
   more than 500 MiB of new encrypted data, naming the file (see "Large
   files"). The push still goes ahead. Ctrl-C or SIGTERM while the push is
   measured stops the backup before old backups are dropped;
7. drops old backups as `salt prune` does, keeping `--keep-days N` days with
   a change (5 by default), one a day before the latest;
8. pushes the branch to `origin`, leased to the commit step 5 found there
   (`--force-with-lease=ref:commit`), so a backup pushed from another
   machine is never overwritten, even if something has fetched into the repo
   since. A push whose answer was lost when the connection dropped still
   counts as this machine's on the next run. git is not asked to prompt for
   a password (`GIT_TERMINAL_PROMPT=0`; ssh can still ask on a terminal, but
   cron has none). Errors never show credentials from origin's URL. An HTTP
   transfer slower than 1 KiB/s for a minute is stopped, and `git ls-remote`
   and `git push` are each stopped after 2 hours.

A failure stops the steps that follow. A backup committed but not pushed is
pushed by the next run. Ctrl-C or SIGTERM stops a database copy, the commit
or the push, and the backup stops before its next step, so old backups are
never dropped after one.

### Presets

A preset says where one tool keeps the files needed to restore its memory.
Each is a JSON file in `internal/preset/presets`, built into salt. The same
code reads them all, so adding a tool means adding one file:

```json
{
  "name": "example",
  "about": "one line shown to people",
  "paths": [
    {"from": "${TOOL_HOME:-~/.tool}/data", "to": "tool/data"},
    {"from": "${TOOL_HOME:-~/.tool}/profiles/*/data", "to": "tool/profiles/*/data"}
  ],
  "skip": ["*.log", "cache"],
  "secrets": [{"files": ["config.yaml"], "keys": ["*sync_key"]}]
}
```

- `name` is the file's name without `.json`. A field salt does not know is
  refused, so a misspelt one, such as `secret`, never drops a rule.
- `from` is where a file or folder is. `${VAR}` is an environment variable,
  and the path is skipped when it is unset or empty. `${VAR:-DEFAULT}` uses
  DEFAULT then. A leading `~` is the home folder. A part that is only `*`
  matches every folder there, such as each profile, leaving out hidden ones.
  A `*` held by a variable is part of a name, never matched.
- `to` is where it goes in the backup. It has a `*` for each `*` in `from`,
  which takes the name that `*` matched. Paths that do not exist are skipped.
- `skip` lists name patterns of files and folders never backed up, such as
  caches, logs and downloaded models. `.DS_Store` and `.git` are always
  skipped, as in `salt seal`.
- `secrets` lists files that may hold secrets, and the settings in them that
  do. Such a file is read as YAML (which includes JSON), every document in
  it. Every setting at any depth is checked by its name in lower case, so
  `keys` are written in lower case too. Salt always checks the settings most
  tools keep secrets in (`*api_key`, `*apikey`, `*api-key`, `*secret`,
  `*secret_key`, `*access_key`, `*private_key`, `*password`, `*passphrase`,
  `token`, `*_token`, `*-token` and `authorization`). `keys` adds the tool's
  own, so it can be left out. If one of these holds text (a non-empty string
  at any depth below the setting, or a YAML alias, which salt does not
  follow), the file is left out. Salt prints one line naming the file and the
  setting, never its value. Numbers, true or false, and null never count, so
  `max_token: 512` is not taken for a secret. Nor does a string that is only
  `${NAME}` or `${env:NAME}`, which names the environment variable a secret
  is kept in, as in `api_key: ${MODEL_API_KEY}`. A string with anything more,
  such as `sk-${NAME}`, a default in `${NAME:-x}` or another source in
  `${vault:x}`, still counts. Secrets almost always mix letters and digits,
  so one that is only a number is unusual. When a checked setting holds a
  number and no text, the file is backed up, and salt prints a warning naming
  the file and the setting, never its value. It says to keep it in an
  environment variable if it is a secret. A file that
  cannot be read, cannot be read as YAML, or is over 1 MiB is left out too.
  Salt never changes the file to remove the secret.

Inside a folder, a file that starts with SQLite's header is a database and
gets a safe copy. Its `-wal`, `-shm` and `-journal` files are not backed up,
since the copy already holds what is in them. Every other file is sealed as
it is, read in place without a copy. Its permissions and last-modified date
are read just before its contents, not when it was found, since the database
copies made in between can take a while. A symlink inside a folder is not
followed or backed up, and salt prints one line about it. A `from` that is
itself a symlink is followed. A file or database the tool deletes while salt
backs it up, as tools do with temporary files, or replaces with something
that is not a file, is left out of that backup instead of stopping it. A
database named with `--sqlite`, or a file in `salt seal`'s source folder,
still stops the seal when it is missing, since the person named it.

A place found twice, such as a folder named both by a variable and by its
default, or by two presets, is backed up once, under the first path, taking
presets in name order. A place inside another, such as one preset's data
folder inside another preset's folder, is backed up under its own path, and
the folder around it leaves it out. Where presets overlap, a file is backed
up if any preset that reaches it would back it up, so adding a preset never
drops a file another one backs up. A preset reaches a place inside its own
unless it skips a folder on the way. Every preset's secrets rules apply to
every file. So nothing is backed up twice, and the backup is the same
whatever order the presets are given in.

Two places backed up at the same path are refused before anything is copied.
A place that contains the backup repo, or is inside it, is refused, comparing
real paths with symlinks followed. Files are then read from those real paths,
so a symlink changed while salt backs up cannot lead the reads anywhere else.
Messages show the path the preset names.

The `mnemosyne` preset covers Mnemosyne's data folder (its main database,
memory banks and shared database), its `config.yaml`, and its attached files
(`blobs`). It looks in the Hermes folder (`$HERMES_HOME` or `~/.hermes`), in
each Hermes profile, and where `MNEMOSYNE_DATA_DIR`, `MNEMOSYNE_BLOB_DIR`,
`MNEMOSYNE_SHARED_DB_PATH` and `MNEMOSYNE_HOME` point. Mnemosyne keeps its
downloaded models, logs and `backups` folder beside these folders, not in
them, so they are not backed up. Inside them, the preset leaves out `.env`
files, the copies Mnemosyne makes of a database before migrating it
(`*.pre_*_backup`) and its unfinished repairs (`.mnemosyne-repair-*`). A
folder that `MNEMOSYNE_MODEL_CACHE_DIR` or `MNEMOSYNE_BACKUP_DIR` points to
inside one of them is backed up with it. Mnemosyne's default blob folder,
`~/.hermes/mnemosyne/blobs`, does not follow `HERMES_HOME`, so the preset's
does not either. Its `config.yaml` can hold API keys (`mnemosyne config set`,
or `mnemosyne config migrate`, which copies every `MNEMOSYNE_*` variable into
it), so it is checked for them.

| On the machine | In the backup |
|---|---|
| `~/.hermes/mnemosyne/data` and `config.yaml` (or under `$HERMES_HOME`) | `hermes/mnemosyne/` |
| `~/.hermes/profiles/<name>/mnemosyne/data` and `config.yaml` | `hermes/profiles/<name>/mnemosyne/` |
| `~/.hermes/mnemosyne/blobs` (or `$MNEMOSYNE_BLOB_DIR`) | `mnemosyne-blobs/` |
| `$MNEMOSYNE_DATA_DIR` | `mnemosyne-data/` |
| `$MNEMOSYNE_SHARED_DB_PATH` | `mnemosyne-shared.db` |
| `~/.mnemosyne/data` (or `$MNEMOSYNE_HOME/data`) | `mnemosyne-home/data/` |

The `hermes` preset covers Hermes's own memory (`memories/MEMORY.md` and
`memories/USER.md`), its persona (`SOUL.md`), its settings (`config.yaml`
and `profile.yaml`), the names chosen for its chat channels
(`channel_aliases.json`), the chat users it has approved
(`pairing` and `platforms/pairing`), its scheduled jobs (`cron/jobs.json`,
`cron/notepad.db` and `cron/output`), its databases (`state.db`, which holds
its sessions and their messages, `kanban.db`, each board's
`kanban/boards/<board>/kanban.db` and `projects.db`), and its `skills`,
`reference`, `skins` and `plans` folders.
It looks in the Hermes folder (`$HERMES_HOME` or `~/.hermes`) and in each
Hermes profile.

The preset names each file and folder it backs up, so nothing else in the
Hermes folder is backed up. That leaves out its credential files (`.env`,
`auth.json` and the credential vault, `vault.key` and `vault.json.enc`), the
channel list (`channel_directory.json`, a cache Hermes builds again when it
starts), logs, the `sessions` folder of JSON transcripts, caches, browser
profiles, downloaded models, the `hermes-agent` source folder, Hermes's own
backups and snapshots, and each board's workspaces and attachments. Inside
the folders it backs up, it leaves out `.env` files, `auth.json`, the vault
files, and the Python and Node caches and packages a skill can hold
(`__pycache__`, `node_modules`, `.venv` and the like), which are installed
again when needed. Memory providers with their own database, such as
Mnemosyne or Hermes's Holographic provider (`memory_store.db`), are not part
of this preset. Mnemosyne has its own, and the two can be given together.

Hermes's `config.yaml` can hold API keys and tokens, such as
`model.api_key`, a `sudo_password` or a token in an MCP server's `env`. So it
is checked for them, with `key` checked as well as salt's usual settings.
The usual `api_key: ${MODEL_API_KEY}` or `${env:MODEL_API_KEY}` names a
variable, not a key, so a file holding only that is backed up. Other files
are not checked, so a key a skill keeps in its own file is backed up,
encrypted like everything else.

| On the machine | In the backup |
|---|---|
| `~/.hermes/<file or folder>` (or under `$HERMES_HOME`) | `hermes/<file or folder>` |
| `~/.hermes/profiles/<name>/<file or folder>` | `hermes/profiles/<name>/<file or folder>` |

Hermes's credential files are not backed up, so after restoring Hermes, set
up its API keys and logins again, as on a new machine. The pairing folders
can also hold unused pairing codes, which expire after an hour. They are
encrypted like every other file and restored with the owner-only
permissions they had.

`state.db` holds every session, so it can grow to several GB and changes
whenever Hermes is used. See "Large files" for what that costs the backup
repo, and back up once a day.

Hermes keeps the newest 50 outputs of each scheduled job
(`cron.output_retention`) and deletes older ones, so `cron/output` stays
small. A deleted output drops out of the next backup and stays in history
until prune removes it. With `cron.output_retention` set to 0 or less,
Hermes keeps every output and so does the backup.

Hermes sets `HERMES_HOME` to a profile's folder while it runs that profile.
A `salt backup` started from inside Hermes, such as from one of its
scheduled jobs, then sees only that profile. It backs that profile up as the
Hermes folder and drops the rest from the backup, naming each place that
went missing. Run `salt backup` from cron or another scheduler outside Hermes,
with `HERMES_HOME` unset or set to the main Hermes folder.

Cron does not see variables set in the person's shell, so any of these the
agent uses must be set in the cron line too, such as
`HERMES_HOME=/srv/hermes /opt/homebrew/bin/salt backup --preset mnemosyne REPO`.
To get the memory back, restore into a new folder, stop the agent, and copy
each folder back to where it came from, such as `hermes/` to `~/.hermes/`.

## Out of scope

Touch ID, and switching recovery method.

## Still to build

- OpenViking support. Its data format has not been checked yet.
- Presets for more tools, such as OpenClaw, Honcho, Hindsight and Hermes's
  Holographic memory provider.
- Reusing the unchanged chunks of a large file that changes often, so a
  backup uploads only what changed
  ([#44](https://github.com/spicy-lemonade/salt/issues/44)).
