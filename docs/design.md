# Salt design

Salt encrypts AI-agent memory, such as Hermes, Mnemosyne, Honcho, Hindsight
and OpenViking, before it is backed up to Git.

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
  If this machine approved the repo's keys, only an approved key may have
  signed it, and they warn if the repo's keys or settings changed since.
  `--allow-unsigned` goes ahead with a warning, for example to look at a
  backup someone else replaced. Every file is still checked against the
  index.
- Restore and verify read each file only one byte past its size in the
  signed index, so an object swapped for one that expands hugely is refused
  as soon as it runs past that size. With `--allow-unsigned` the sizes come
  from whoever wrote the index, so they limit nothing.
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
  stored on the machine, and about visible file names. `salt restore` and
  `salt verify` accept a backup only if an approved key signed it (see
  "Planted files"). An approved copy that can't be read stops seal, restore
  and verify, unless restore or verify is given `--allow-unsigned`. `salt
  trust` replaces it.
- **Hiding plaintext from the hook.** `salt check` reads staged files as
  `:0:<path>`, so a file named like `0:x` can't hide behind `x`.
- **Symlinks.** Salt never creates symlinks where it keeps data (`.salt/`,
  `index.age`, `objects/`, `files/`). Seal, restore and verify refuse to run
  if one is there, whether it points outside the repo or back inside it. As a
  second guard, every read and write goes through `os.Root`, which refuses
  paths that lead outside the repo. Every command reads salt's own files in
  `.salt/` only if neither they nor the folder are symlinks, and reads at most
  64 KiB of each. So a pushed link to a device or to another file can't make
  salt read without end or show what the file holds. A bad key in
  `.salt/recipients.txt` is named by its line number, not quoted.
- **Tampered index.** The index is read one entry at a time, capped at 100,000
  entries and 32 MB, so a crafted index can't use much memory. `salt verify`
  lists at most 50 problems, with long paths shortened.
- **Terminal codes.** Someone who can push chooses file names, attribute
  values and the index's paths, and a remote or database server chooses what
  git and `pg_dump` print. A control character among them, such as ESC, could
  change what the terminal shows. Everything salt prints goes through one
  writer (`internal/escape`) that shows every character that does not show as
  itself, other than a newline or a tab, as a Go escape such as `\x1b`, and
  every byte that is not UTF-8 as `\xNN`. Spaces such as the narrow no-break
  space in macOS screenshot names are shown as they are. A name from the repo
  that holds one of those characters, a newline included, is also quoted, so
  it can't look like a line of salt's own. That covers paths in the index,
  files `salt check` and `salt doctor` find, and files `salt verify` finds
  outside the index.
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

Nor does the signature say which repo a backup belongs to. If one machine
holds the keys for two repos, someone who can push to one and read the other
could add the other's key to the first and copy its backup in. It would be
genuinely signed with your key. Restore and verify therefore accept only a
signature from a key this machine approved for the repo. They name the added
key, and if it signed the backup they say so, rather than calling it unsigned.
A local repo this machine never approved, for example one opened by another
path such as a symlink, has no approved keys to check against, so any key that
opens the backup may have signed it, and they warn. A restore from a URL is
the same, without the warning. A repo ID inside the signed data would close
that, but needs a new index version.

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
8. Salt never waits on something that is not a file. Opening a named pipe
   waits until something writes to it, which could be for ever, and a file
   can become one after salt has looked at it. So every file salt reads from
   a source folder or the backup repo, its pre-commit hook included, is
   opened without waiting, checked once it is open, and refused at once if it
   is not a regular file (`internal/regular`).

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
libpq has no environment variable for its other secrets, an SSL key's
passphrase (`sslpassword`), `oauth_client_secret`, and the SCRAM keys
(`scram_client_key`, `scram_server_key`), so salt refuses a CONN that gives
one, naming the setting but never its value. It would otherwise show in
`pg_dump`'s command line and in salt's messages. A libpq service file
(`~/.pg_service.conf`, named with `service=NAME`) keeps it out of both. Salt
then adds no keepalives (see below), so the service should set its own.
`--no-password` stops `pg_dump` from waiting for someone to type a password.
Unless the person set `PGCONNECT_TIMEOUT`, salt sets it to 30 seconds, so a
server that cannot be reached fails the backup instead of stalling it. A
table another program has locked fails the dump after 30 seconds. Salt adds
libpq's keepalives to the connection `pg_dump` gets (`keepalives_idle=30`,
`keepalives_interval=10`, `keepalives_count=3`, and `tcp_user_timeout=60000`
when `pg_dump --version` is 12 or later, as older libpq refuses it). A
connection that dies part way through a dump, such as when the server's
container restarts or the laptop sleeps, then fails it after about a minute
without a reply, instead of the two hours the system would wait, all the
while holding the repo's lock. A busy server still answers, so a long dump
is never stopped. A setting the connection gives itself is kept. A
connection that turns keepalives off with `keepalives=0` gets none, and so
does one that names a libpq service, or runs with `PGSERVICE` set, so the
service file's own are kept. Messages show the connection without them.
A dropped connection or any other `pg_dump` error stops salt before sealing,
with `pg_dump`'s reason. A connection with no database name uses
`PGDATABASE`, as libpq does. Without that it is refused, rather than letting
`pg_dump` guess one.

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
   finds nothing, naming where it looked and each variable it needs that is
   not set. When another preset found something, it says to leave that one
   out if the person doesn't use it;
3. makes a safe copy of each SQLite database found, as `--sqlite` does, and
   of each database a preset names, such as a Postgres one, as `--postgres`
   does. It seals the copies and every other file found into REPO, which then holds
   only them. Anything else in REPO is removed, as with `salt seal --prune`.
   A place the last backup held that is not found this time, and that
   holds nothing of its own in this backup (a place inside it that is
   found does not count), is then named in one line each, such as a
   folder on a drive that is not mounted, one pointed to by a variable set
   in the person's shell but not in cron, or a file deleted before it was
   sealed. A missing place inside another missing one is not named again.
   The backup goes on without it, and its earlier copies stay in history
   until prune drops them. The last backup's paths come from salt's change
   cache. A preset whose every file was deleted before it was sealed stops
   the backup here, before anything is committed, as one that found
   nothing does;
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
  "databases": [
    {"kind": "postgres", "from": "${TOOL_DB_URL:-postgresql://localhost/tool}", "to": "tool/tool.sql"}
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
  A part with text around its `*`, such as `workspace-*` or `.tool-*`,
  matches each folder whose name has that text, with at least one character
  for the `*`. It matches hidden folders only when it starts with a dot, as
  in a shell. A part holds at most one `*` and no variable, and the first
  part holds none. A `*` held by a variable is part of a name, never
  matched.
- `to` is where it goes in the backup. It has a `*` for each `*` in `from`,
  at most one in a part, which takes the text that `*` matched, so
  `{"from": "~/.tool-*/data", "to": "tool-profiles/*/data"}` backs up
  `~/.tool-work/data` as `tool-profiles/work/data`. A `*` that would match a
  whole `.` or `..` matches nothing. Paths that do not exist are skipped.
- `databases` lists live databases that are not files, such as one on a
  Postgres server. `kind` is the `salt seal` option for that kind of
  database without its dashes, such as `postgres`. Only a kind given by a
  connection can be named, since `paths` already finds a SQLite file and
  gives it a safe copy. `from` is the connection, with variables and
  defaults as in a path's `from` and no `*`. The database is skipped
  when a variable without a default is unset or empty. `to` is the path
  the copy is backed up under. The copy is made as `salt seal` makes it, so
  its password is never shown and an unchanged database makes no commit.
  An error reading the connection names the variable it came from, never
  the connection. A server that cannot be reached stops the backup before
  anything is committed, so the last backup stays the latest. That error
  says which variable the preset read the connection from, or that it is
  the preset's default, used when the variable is not set, which is then
  to be set in the cron line too. A connection
  two presets name, compared without its password, is backed up once under
  the first preset's path, taking presets in name order.
- `skip` lists name patterns of files and folders never backed up, such as
  caches, logs and downloaded models. `.DS_Store` and `.git` are always
  skipped, as in `salt seal`.
- `secrets` lists files that may hold secrets, and the settings in them that
  do. A file is named by a name pattern, such as `config.yaml`, or by the
  folders it is in and its name, joined by `/`, such as
  `_system/users.json`. Such a pattern is matched against as many of the
  last parts of the file's path, at any depth, so a file with the same name
  in any other folder is not checked. Such a file is read as YAML (which
  includes JSON), every document in it. Every setting at any depth is checked by its name in lower case, so
  `keys` are written in lower case too. Salt always checks the settings most
  tools keep secrets in (`*api_key`, `*apikey`, `*api-key`, `*secret`,
  `*secret_key`, `*secretkey`, `*access_key`, `*accesskey`, `*private_key`,
  `*privatekey`, `*password`, `*passphrase`, `token`, `*_token`, `*-token`
  and `authorization`), in snake_case and camelCase. `keys` adds the tool's
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
  cannot be read, cannot be read as YAML or JSON, or is over 1 MiB is left
  out too. A comment, as JSON5 allows, is not YAML. One that stops salt
  reading the file, or that salt would read as part of a setting's name, as
  before a setting or a closing brace, or between a name and its colon,
  keeps the file out, since it would hide that setting from the check. Salt
  takes a name that is not quoted for a comment when it starts with `//` or
  holds `/*`, so a YAML name such as `src/*` keeps the file out unless it is
  quoted. A quoted name, such as the `"//"` some JSON files use for a
  note, `"src/*"` or a URL, is a name, not a comment, and is checked like any
  other. A comment before the opening brace keeps the file out too, either
  because YAML cannot read what follows or, when no `: ` follows it, because
  YAML reads the whole file as one piece of text. A comment after a value
  becomes part of the value, so the setting holds text. JSON5 also allows a
  name and its value with no space between them, as in `{apiKey:"x"}`, which
  YAML reads as one name with no value. So a name that is not quoted and
  holds `:`, inside braces, keeps the file out, and salt says a name has no
  space after its colon. This includes YAML that means such a name, as in
  `{llama3:8b: 1}`, which salt cannot tell apart. A quoted name such as
  `"a:b"`, and a YAML name outside braces such as `llama3:8b`, are names.
  Remove comments from such a file, and put a space after each colon, to be
  sure it is checked as written and backed up. Salt never changes the file
  to remove the secret.
- `refs` in a secrets rule lists the ways the tool names where a secret is
  kept in an object, each as the exact names of that object's settings in
  lower case, never patterns, such as
  `[["source", "id"], ["source", "provider", "id"]]`. An
  object holding exactly one of these sets of settings, with one value in
  each, names where the secret is, as `${NAME}` does, so it never counts as
  a secret. An object with any other setting, or with more than one value in
  a setting, still counts.

A preset is for one tool. A memory tool that works with any agent, such as
Mnemosyne, Honcho, Hindsight or OpenViking, gets a preset that looks where
the tool keeps its data on its own, and also inside an agent's folder where
that agent embeds it, so it works alone or beside any agent's preset. The
exceptions are an agent's own built-in memory, such as `hermes`, and a
memory tool built for one agent, such as `holographic`, a Hermes plugin.
Their presets look only in that agent's folders. No preset depends on
another, and any can be given with any other.

Inside a folder, a file that starts with SQLite's header is a database and
gets a safe copy. Its `-wal`, `-shm` and `-journal` files are not backed up,
since the copy already holds what is in them. Every other file is sealed as
it is, read in place without a copy. Its permissions and last-modified date
are read just before its contents, not when it was found, since the database
copies made in between can take a while. A symlink inside a folder is not
followed or backed up, and salt prints one line about it. A `from` that is
itself a symlink is followed. A file or database the tool deletes while salt
backs it up, as tools do with temporary files, or replaces with something
that is not a file, is left out of that backup instead of stopping it. So
is a place a preset found and the tool deletes before salt reads it, such
as a profile deleted during the backup. It then counts as not found, so
salt names it if the last backup held it, and a preset left with nothing
still stops the backup. A database named with `--sqlite`, or a file in
`salt seal`'s source folder, still stops the seal when it is missing, since
the person named it.

A place found twice, such as a folder named both by a variable and by its
default, or by two presets, is backed up once, under the first path, taking
presets in name order. A place inside another, such as one preset's data
folder inside another preset's folder, is backed up under its own path, and
the folder around it leaves it out. Where presets overlap, a file is backed
up if any preset that reaches it would back it up, so adding a preset never
drops a file another one backs up. A preset reaches a place inside its own
unless it skips a folder on the way. Every preset's secrets rules apply to
every file, so a rule that names only a file's name reaches the same name
in every preset's places. A rule that also names its folders reaches only
files in such folders. So nothing is backed up twice, and the backup is the same
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
of this preset. Each has its own preset, which can be given with this one.

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

The `holographic` preset adds the database of Hermes's Holographic memory
provider (`memory_store.db`) to the `hermes` preset. It looks where the
`hermes` preset does, in the Hermes folder (`$HERMES_HOME` or `~/.hermes`)
and in each Hermes profile, and backs the database up beside Hermes's own
files. It backs up nothing else, so give it with `--preset hermes` to back
up the rest of Hermes. Hermes keeps the database in WAL mode, so it gets a
safe copy and its `-wal` and `-shm` files are not backed up. A Hermes folder
or profile without the database is skipped, but finding none at all stops
the backup, as for every preset.

The preset covers the default path, `$HERMES_HOME/memory_store.db`, which
is where Hermes keeps the database unless
`plugins.hermes-memory-store.db_path` in its `config.yaml` names somewhere
else. A `db_path` naming the default file, such as the
`~/.hermes/memory_store.db` that older versions of `hermes memory setup`
write, is covered too. A database kept anywhere else is not. Back it up
with `salt seal --sqlite PATH` into a repo of its own.

| On the machine | In the backup |
|---|---|
| `~/.hermes/memory_store.db` (or under `$HERMES_HOME`) | `hermes/memory_store.db` |
| `~/.hermes/profiles/<name>/memory_store.db` | `hermes/profiles/<name>/memory_store.db` |

The `honcho` preset covers self-hosted
[Honcho](https://github.com/plastic-labs/honcho). Honcho keeps all its
memory in one Postgres database, with its workspaces, peers, sessions,
messages, the conclusions it draws and their pgvector embeddings. The
preset dumps the database `DB_CONNECTION_URI` names, as Honcho does, or
Honcho's own default when it is not set,
`postgresql://postgres:postgres@localhost:5432/postgres`. That default is
also where Honcho's Docker setup and the Honcho CLI's `local` stack can be
reached from the machine. Salt drops the `+psycopg` driver in Honcho's
connection, as for `--postgres`. Honcho usually reads its connection from
its `.env` file, which cron does not read, so if Honcho uses another
database, set `DB_CONNECTION_URI` in the cron line too. Honcho keeps its
vectors in Postgres unless `VECTOR_STORE_TYPE` names another store, such as
Turbopuffer, LanceDB or Qdrant. Such a store is not backed up.

The preset also backs up the settings that point an agent at its memory in
Honcho. These are Honcho's own `config.json`, which the Honcho CLI and
Honcho's plugins share, and Hermes's `honcho.json` in the Hermes folder and
in each Hermes profile, which names the workspace and peers Hermes uses.
Either can hold an API key (`apiKey`) or an OAuth token (`refreshToken`),
so both are checked for them, with `*token` checked as well as salt's usual
settings. A file holding one is left out, and salt names it. After
restoring, log in to Honcho again, as on a new machine.

The Honcho server's `.env` and `config.toml` are not backed up. They hold
its LLM API keys and sit in whatever folder the server was started from.
Nor are its Redis cache and the stack folders the Honcho CLI keeps in
`~/.honcho/profiles`, which `honcho stack` makes again. A Honcho CLI stack
other than `local` listens on a port of its own. Back it up with its
connection in `DB_CONNECTION_URI`, into a repo of its own. Honcho's cloud
service keeps memory on Honcho's servers, so the preset cannot back it up.

| On the machine | In the backup |
|---|---|
| Honcho's database (`$DB_CONNECTION_URI`, or `postgres` on `localhost:5432`) | `honcho/honcho.sql` |
| `~/.honcho/config.json` (or `$HONCHO_CONFIG_DIR/config.json`) | `honcho/config.json` |
| `~/.hermes/honcho.json` (or under `$HERMES_HOME`) | `hermes/honcho.json` |
| `~/.hermes/profiles/<name>/honcho.json` | `hermes/profiles/<name>/honcho.json` |

To get Honcho's memory back, load `honcho/honcho.sql` into a new database
as in "Restoring a Postgres database", on a server with pgvector, and point
`DB_CONNECTION_URI` at it.

The `hindsight` preset covers self-hosted
[Hindsight](https://github.com/vectorize-io/hindsight). Hindsight keeps all
its memory in one Postgres database, with its memory banks, the facts,
entities and observations it draws, their pgvector embeddings, and the
files given to it. Each tenant's memory is in a schema of its own in the
same database. The preset dumps the database `HINDSIGHT_API_DATABASE_URL`
names, as Hindsight does, or Hindsight's own embedded database when it is
not set, `postgresql://hindsight:hindsight@localhost:5432/hindsight`.
Hindsight usually reads its connection from its `.env` file, which cron
does not read, so if Hindsight uses a Postgres server of its own, set
`HINDSIGHT_API_DATABASE_URL` in the cron line too. Hindsight's own `pg0`
form of the variable names its embedded database and is not a Postgres
connection, so salt refuses it. Set a `postgresql://` connection instead.

Hindsight's embedded database is a PostgreSQL 18 server that Hindsight
starts itself, with the user, password and database all `hindsight`, so
`pg_dump` must be version 18 or newer. It runs while Hindsight does, so
back it up while Hindsight is running. It takes port 5432 when that is free,
or else the next free port. Each hindsight-embed profile has a database of
its own, and each agent that runs Hindsight on the machine has a profile of
its own, such as `hermes` or `claude-code`. With more than one, whichever
starts first takes port 5432, so the default could back up a different one
from day to day. Give each a fixed port instead, with
`HINDSIGHT_EMBED_API_DATABASE_URL=pg0://hindsight-embed-NAME:PORT` in the
profile's `~/.hindsight/profiles/NAME.env`, and back each up with its
connection in `HINDSIGHT_API_DATABASE_URL`, into a repo of its own.
Hindsight's Docker image keeps its embedded database inside the container,
where salt cannot reach it, so run it with `HINDSIGHT_API_DATABASE_URL`
naming a Postgres server that salt can reach. Files kept in another store
(`HINDSIGHT_API_FILE_STORAGE_TYPE`, such as S3), a database on Hindsight's
Oracle backend and Hindsight Cloud's memory are not backed up. The preset
always backs up a database, so with none for it to reach, such as when the
memory is in Hindsight Cloud, every backup stops, and the preset does not
suit that setup.

The preset also backs up the settings that point each agent at its memory
in Hindsight, such as the bank it uses. Hindsight's integrations keep these
in `~/.hindsight`, one JSON file each, named `aider.json`,
`claude-code.json`, `cline.json`, `codex.json`, `coding-agent.json`,
`copilot.json`, `copilot-cli.json`, `cursor.json`, `cursor-cli.json`,
`devin-desktop.json`, `omo.json`, `opencode.json`, `openhands.json`,
`zcode.json` and `zed.json`. Hermes keeps its own in `hindsight/config.json`
in the Hermes folder and in each Hermes profile, and older Hermes setups
share `~/.hindsight/config.json`. Any of these can hold an API key or a token
(`hindsightApiToken`, `apiToken`, `api_key` or `llmApiKey`), so each is
checked for them, with `*token` checked as well as salt's usual settings. A
file holding one is left out, and salt names it. After restoring, set the
key or token up again, as on a new machine. An agent that keeps its
Hindsight settings in its own settings file, such as OpenClaw's
`openclaw.json`, is not covered by this preset. Nor is a settings file that
`HINDSIGHT_CONFIG` names somewhere else, or a repository's
`.hindsight/config.toml`.

Nothing else in `~/.hindsight` is backed up. Its `.env` files hold
Hindsight's LLM API keys, `config` and `cli-profiles` hold the Hindsight
CLI's API keys in TOML, which salt cannot check, and `xai_oauth.json` and
`control.token` hold logins. Nor are its logs, or the integrations' state
folders, installed code and downloaded models, which are made again when
needed. A hindsight-embed profile's settings are in its `.env` file, so
after restoring, set Hindsight up again with the same embedding model as
before, since the stored vectors were made with it.

| On the machine | In the backup |
|---|---|
| Hindsight's database (`$HINDSIGHT_API_DATABASE_URL`, or `hindsight` on `localhost:5432`) | `hindsight/hindsight.sql` |
| `~/.hindsight/<integration>.json`, such as `claude-code.json`, and `~/.hindsight/config.json` | `hindsight/<integration>.json` and `hindsight/config.json` |
| `~/.hermes/hindsight/config.json` (or under `$HERMES_HOME`) | `hermes/hindsight/config.json` |
| `~/.hermes/profiles/<name>/hindsight/config.json` | `hermes/profiles/<name>/hindsight/config.json` |

To get Hindsight's memory back, load `hindsight/hindsight.sql` into a new
database as in "Restoring a Postgres database", on a server with pgvector
and any other extension Hindsight was set up with, and point
`HINDSIGHT_API_DATABASE_URL` at it.

The `openclaw` preset covers [OpenClaw](https://github.com/openclaw/openclaw).
OpenClaw keeps each agent's memory as Markdown in the agent's workspace, in
`MEMORY.md`, `USER.md`, `DREAMS.md` and the daily notes in `memory/`, beside
its persona (`SOUL.md` and `IDENTITY.md`), its instructions (`AGENTS.md`)
and its skills. The preset backs up each workspace whole, with whatever else
the agent keeps there. It also backs up the Memory Wiki plugin's vaults
(`wiki`), the LanceDB memory plugin's store (`memory/lancedb`), the skills
every agent shares (`skills`), the skills each agent has learned
(`agents/<agent>/agent/workshop-skills`), and OpenClaw's settings
(`openclaw.json`).

It looks in OpenClaw's folder (`$OPENCLAW_STATE_DIR` or `~/.openclaw`) and
in each OpenClaw profile's folder (`~/.openclaw-<profile>`). In each, the
first agent's workspace is `workspace` and each other agent's is
`workspace-<agent>`. A workspace `OPENCLAW_WORKSPACE_DIR` names is backed up
too. A workspace set somewhere else in `openclaw.json`, in
`agents.defaults.workspace` or an agent's own `workspace`, is not found, so
set `OPENCLAW_WORKSPACE_DIR` to it in the cron line, or back it up with
`salt seal` into a repo of its own. Nor is a LanceDB store that the plugin's
`dbPath` puts somewhere else, or a settings file `OPENCLAW_CONFIG_PATH`
moves somewhere else, since its secrets are checked only in a file named
`openclaw.json`. With `OPENCLAW_HOME` set, set `OPENCLAW_STATE_DIR` in the
cron line too. A workspace holding large files,
such as a repository the agent works on, adds them to the backup, apart from
its `.git` folder.

OpenClaw keeps its sessions, transcripts, scheduled jobs and memory search
index in SQLite databases, `state/openclaw.sqlite` and each agent's
`agents/<agent>/agent/openclaw-agent.sqlite`. The same databases hold its
logins, API keys and the tokens of paired devices, and salt never changes a
file to take them out. So the preset leaves these databases out, and
sessions, transcripts and scheduled jobs are not backed up. OpenClaw builds
the search index again from the workspace's Markdown. OpenClaw's own Git
backup, `openclaw backup git create --exclude-secrets`, can copy the
databases without the logins.

Nothing else in OpenClaw's folder is backed up. That leaves out its
credential files (`.env`, `credentials`, `secrets.json`, `gateway.token`,
`gateway.password`, `identity` and `devices`), each agent's `codex-home`,
the old `sessions` folders, logs, sandboxes, installed plugins and tools,
downloaded models and caches. Inside the folders it backs up, it leaves out
`.env` files, key files (`*.key`, `*.pem`, `*.p12`, `*.pfx`, and SSH keys
whose names start with `id_rsa`, `id_dsa`, `id_ecdsa` or `id_ed25519`), the
credentials a repository the agent works on can hold (`.ssh`, `.aws`,
`.docker`, `.kube`, `.gnupg`, `.netrc`, `.npmrc`, `.pypirc` and
`.git-credentials`), an OpenClaw database kept there, and the Python and
Node caches and packages a skill or project can hold, which are installed
again when needed. Any other SQLite database in a workspace gets a safe
copy.

`openclaw.json` can hold API keys and tokens, such as the gateway's
`gateway.auth.token`, which OpenClaw's setup writes there by default, a
channel's `botToken`, or a key in `env.vars`. So it is checked for them, with
`key`, `encryptkey`, `*signingkey`, `*masterkey`, `*token`, `authtag`,
`serviceaccount`, `value` and `vars` checked as well as salt's usual
settings, which include `*privatekey`, `*secretkey` and `*accesskey`. These
are the settings OpenClaw lists as able to hold a key, and the usual names
of keys, so a setting that only ends in `key`, such as `session.mainKey`, is
not taken for one. A file holding one is left out, and salt names it. A key
written as `${NAME}` names a variable, not a key, and a key kept as an
OpenClaw SecretRef, such as
`{"source": "env", "provider": "default", "id": "NAME"}`, names where the
key is. Neither counts as a secret, so a file holding only such keys is
backed up. OpenClaw reads its settings as JSON5, but writes them as plain
JSON, dropping any comment. Most comments, and a name with no space after
its colon, keep the file out of the backup, as `secrets` above explains, so
remove comments and keep the space to be sure it is backed up.
After restoring, set up OpenClaw's logins and keys again, as on a new
machine.

| On the machine | In the backup |
|---|---|
| `~/.openclaw/<file or folder>` (or under `$OPENCLAW_STATE_DIR`) | `openclaw/<file or folder>` |
| `~/.openclaw-<profile>/<file or folder>` | `openclaw-profiles/<profile>/<file or folder>` |
| `$OPENCLAW_WORKSPACE_DIR` | `openclaw-workspace/` |

`OPENCLAW_STATE_DIR` naming a profile's folder backs that profile up under
`openclaw/`, and the main OpenClaw folder is then not backed up. OpenClaw
sets `OPENCLAW_STATE_DIR` to a profile's folder while it runs that profile,
and the commands it runs, such as its scheduled jobs, see it too. A
`salt backup` started from inside such a profile then backs it up under
`openclaw/`, drops the main folder and the profile's earlier copy from the
backup, and names each place that went missing. Run `salt backup` from cron
or another scheduler outside OpenClaw, with `OPENCLAW_STATE_DIR` unset or
set to the main OpenClaw folder.

To get OpenClaw's memory back, restore into a new folder, stop OpenClaw, and
copy `openclaw/` to `~/.openclaw/` and each `openclaw-profiles/<profile>/` to
`~/.openclaw-<profile>/`.

The `openviking` preset covers self-hosted
[OpenViking](https://github.com/volcengine/OpenViking). OpenViking runs as a
server and keeps its memory as files in its workspace folder, in `viking`.
These are each user's memories, the resources and skills given to it, its
sessions, the summaries it writes beside them (`.abstract.md` and
`.overview.md`), and its accounts and their settings. Its snapshot history
is in `.ovgit` beside them, unless `git.local.base_dir` in `ov.conf` moves it
somewhere else, where it is not found. The preset backs up both, from
`~/.openviking/data`, which is where OpenViking's setup
(`openviking-server init`) and its Docker image keep the workspace. It also
backs up the server's settings (`ov.conf`), the settings that point the `ov`
CLI and each agent's OpenViking plugin at the server (`ovcli.conf`), which
Claude Code, Codex, Cursor, OpenCode and other agents share, and the
settings each repository's agent uses for its memory (`workspaces`).
OpenViking's plugins ignore any key written in a `workspaces` file, so those
files are not checked for secrets.

Hermes's OpenViking plugin can run a server of its own (Quick Local), with
its workspace in `openviking/data` in the Hermes folder and in each Hermes
profile. The preset backs up its `viking` and `.ovgit` folders there too,
and `memory_mirror_registry.json`, which records where in OpenViking each
entry of Hermes's own memory was copied, so Hermes can go on changing them.
That server's `ov.conf` and `ovcli.conf` always hold its key, and Hermes's
setup writes them again, so they are not backed up. OpenClaw's OpenViking
plugin keeps its settings in `openclaw.json`, which the `openclaw` preset
backs up, and its memory in the server's workspace, which this one does.

OpenViking's vector index (`vectordb`) is not backed up. It holds an
embedding of each file in `viking`, which OpenViking can make again from
them. It is also a database the server keeps rewriting, which a copy made
file by file while the server runs can break, and its large files would
change the backup repo every day. Nor are OpenViking's job queue, logs,
temporary uploads or lock files (`.openviking.lock`, `.path.ovlock` and
`.exact.ovlock.*`) backed up, or the `bot` folder of VikingBot, the agent
OpenViking ships. Nothing else in `~/.openviking` is backed up. That leaves
out the encryption key (`master.key`, or a KMS-wrapped `*-root-key.enc`),
Codex logins (`codex_auth.json`), the Context Gateway's `.env` file, saved
CLI profiles (`ovcli.conf.<name>`), old copies of the settings
(`ov.conf.bak`), the installed server and plugins, and the plugins' logs,
state and queues of unsent messages. In the Hermes folder, the Quick Local
server's runtime and downloaded models, and the markers of sessions still to
be committed, are left out.

`ov.conf` can hold API keys, such as each model's `api_key`, the server's
`root_api_key`, or a rerank service's `ak` and `sk`, and `ovcli.conf` can
hold an `api_key`. In the workspace, each account's settings
(`viking/<account>/_system/setting.json` and its `.backup.json` copy) and
the runtime settings (`viking/_system/runtime_config/cluster.json` and its
copy) can hold model keys. So these are checked, with `ak` and `sk` checked
as well as salt's usual settings. Each account's
`viking/<account>/_system/users.json` holds its users' API keys when the
server checks keys (`auth_mode` set to `api_key`, which Docker needs), so it
is checked with `key` as well as salt's usual settings. A file holding one
is left out, and salt names it. The rules name these folders, so a file
with the same name elsewhere, such as in a resource given to OpenViking or
in another preset's folder, is not checked. A server that checks keys
always has them in `users.json`, so salt names it in every backup, and after
restoring, its users are registered again with `ov admin`.

With OpenViking's encryption on, the files in `viking` are encrypted with
the key in `master.key`, or with a wrapped key and the KMS or Vault key that
opens it. Salt does not back these up, and the backup cannot be read
without them, so keep a copy somewhere safe. The settings and users files
are encrypted too, so salt cannot read them to check them. They are left
out, and salt names them in every backup. After restoring, set each
account's model settings up again.

OpenViking saves a snapshot by writing its objects into `.ovgit` first and
then pointing the account's branch (`.ovgit/<account>/refs/heads/main`) at
the newest one. Salt reads files in order of their path, so it reads the
snapshot objects before the branch. A snapshot saved while salt backs up can
then leave the backup's branch pointing at a snapshot whose objects it does
not hold. OpenViking reaches every snapshot through the branch, so after
restoring such a backup it can neither save a new snapshot nor list or go
back to older ones for that account, until its `refs/heads/main` is pointed
back at an earlier snapshot by hand. The memory in `viking` is not affected,
and the next backup holds the whole history again. OpenViking advises
pausing writes while its workspace is copied, so run `salt backup` when
OpenViking is not saving snapshots, or with it stopped.

A workspace set somewhere else in `ov.conf` (`storage.workspace`), such as a
systemd service's `/var/lib/openviking/data` or the `./data` of a
hand-written `ov.conf`, is not found. Nor is a settings file that
`OPENVIKING_CONFIG_FILE` or `OPENVIKING_CLI_CONFIG_FILE` names, or one in
`/etc/openviking`, since secrets are checked only in files named `ov.conf`
and `ovcli.conf`. Back such a workspace's `viking` folder up with
`salt seal` into a repo of its own. Content OpenViking keeps in S3
(`storage.agfs.backend` set to `s3`), a vector database on another server
and the memory in OpenViking's cloud service are not backed up. With no
workspace for the preset to find, and settings that hold a key, the preset
finds nothing and every backup stops, so it does not suit that setup.

| On the machine | In the backup |
|---|---|
| `~/.openviking/ov.conf`, `ovcli.conf` and `workspaces/` | `openviking/` |
| `~/.openviking/data/viking/` and `data/.ovgit/` | `openviking/data/` |
| `~/.hermes/openviking/data/viking/`, `data/.ovgit/` and `memory_mirror_registry.json` (or under `$HERMES_HOME`) | `hermes/openviking/` |
| the same in `~/.hermes/profiles/<name>/openviking/` | `hermes/profiles/<name>/openviking/` |

To get OpenViking's memory back, restore into a new folder, stop OpenViking,
and copy `openviking/` to `~/.openviking/`, and `hermes/openviking/` and each
`hermes/profiles/<name>/openviking/` to the same place in the Hermes folder.
For Hermes's own server, run Hermes's OpenViking setup again to install it.
Set up any settings that were left out again, start OpenViking, and run
`ov reindex viking://`, with an admin key when the server checks keys, to
make the vector index again. Until it finishes, memories can be read and
browsed but not found by meaning. It embeds every file with the model
`ov.conf` names, which with a paid model costs about as much as adding them
all again. Tags set with `set_tags`, the memory type that ranks older events
lower, and how often each memory was used are kept only in the vector index,
so they are lost.

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

- Presets for more tools.
