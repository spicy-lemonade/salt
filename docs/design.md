# salt design

salt encrypts AI-agent memory backups (Hermes, Mnemosyne, Honcho, Hindsight,
OpenViking, and the Markdown files around them) before they are pushed to Git.

## Principles

- **Plaintext never enters the backup repo.** `salt seal SRC REPO` writes only
  ciphertext into the repo; the pre-commit hook `salt check` refuses any staged
  file that is not age ciphertext (fail-closed), apart from a small allowlist.
- **Nightly backups need no secret.** Sealing uses only the public key
  (recipient). The private key is needed only to restore.
- **File paths are encrypted by default.** Files are stored as
  `objects/<xx>/<id>.age` plus an encrypted `index.age`. Opt out with
  `salt init --plain-paths` or `encrypt_paths = false` in `salt.toml`.
- **Streaming only.** Data flows read → zstd → age → write with fixed buffers;
  data files are never read whole into memory.
- **Unchanged files are not re-encrypted.** A local cache (never committed)
  maps plaintext hashes to existing ciphertext, so an unchanged snapshot makes
  no commit.

## Keys and recovery

One age X25519 key pair per user.

- Public key: in `salt.toml`, used by the nightly seal.
- Private key: kept in the OS keychain for everyday decrypts (Touch ID on
  macOS). Optional extra recipients: YubiKey (`age-plugin-yubikey`), password
  manager, second machine.

At `salt init` the user chooses a recovery method.

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

Choose [1]:
```

Phrase display:

```
Write these down, in order.
Anybody with these words can decrypt and read your backups.
If you lose them and this laptop, your backups cannot be recovered.
```

### Recovery phrase (recommended)

- 12 BIP39 words (128 bits of entropy); the age identity is derived from them,
  so nothing key-related is stored in the repo.
- The last word carries a checksum, so typos are caught; words are unique in
  their first 4 letters, so entry can autocomplete.
- **Onboarding runs `salt recovery test` immediately**: the user types all 12
  words back. If they are wrong, onboarding restarts at the phrase display.
  Nothing (keychain entry, config, repo files) is written until the test
  passes, so an aborted init leaves no key behind.
- `salt recovery show` re-displays the phrase after Touch ID, for when the
  laptop still works but the written copy is lost.

### Passphrase

- The private key is encrypted with the passphrase (age scrypt, ~1s per guess)
  and stored as `key.age` in the repo.
- Weak passphrases are rejected (strength estimate plus minimum length).
- Scripted installs read it from stdin or a file, never from argv.

### Changing method

Possible, but the recovery method is tied to the key. Before switching, salt
must warn explicitly:

```
Your recovery method is tied to your encryption key. Switching creates a new
key and re-encrypts your current backup with it. Backups made before today
can still only be decrypted with your OLD phrase or passphrase. Keep it.
```

`salt restore` can hold several identities, so a restore that spans the change
can unlock both.

### Restore

```
brew install spicy-lemonade/tap/salt
git clone <backup repo> && cd <backup repo>
salt restore --to ~/.hermes
```

Identity lookup order: keychain → configured plugin/password manager → prompt
for recovery phrase or passphrase. The unlocked key stays in memory only.
Restores decrypt into a temp dir, verify every file against the encrypted
index, then move into place with 0600 permissions. A live directory is never
overwritten without `--force`. age is authenticated: tampered files fail to
decrypt rather than yielding wrong data. Phase 2 adds a signed index to detect
files planted by someone who can push to the repo.

## Sources

| Source | Method |
|---|---|
| Files/dirs (md, yaml, json, skills/) | glob include/exclude |
| SQLite (Mnemosyne, state/kanban/notepad DBs) | `sqlite3 .backup` to temp, then stream |
| Postgres + pgvector (Honcho, Hindsight) | `pg_dump -Fc` streamed, local or `docker exec` |
| OpenViking (AGFS + vector index) | directory source with optional pre/post commands |

Large files are split below GitHub's 100 MB limit. `salt prune` (opt-in,
history rewrite) bounds repo growth.

## Process and memory safety

The first attempt crashed the host: the hook installed during tests pointed at
`os.Executable()`, which was the test binary, so every test commit re-ran the
whole suite recursively. Rules:

1. **salt never runs inside salt.** `internal/guard` sets `SALT_ACTIVE` on
   start and refuses to run if it is already set, so a hook fired by any child
   of salt cannot start salt again.
2. **Git calls made by salt disable hooks** (`-c core.hooksPath=/dev/null`)
   and salt never runs `git commit` from hook mode.
3. **No `os.Executable()`.** Hooks call `salt` by name. A source-scan test
   enforces this.
4. **Unit tests never start processes.** Only `internal/gitx` and source
   adapters may call `exec.Command`, and no `_test.go` outside `test/e2e`
   may. A source-scan test enforces this.
5. **e2e tests** use the `e2e` build tag and a prebuilt `SALT_BIN`. They never
   build salt themselves and run only in Docker with `--memory` and
   `--pids-limit` (`make e2e`).
6. **Runtime limits:** a soft memory limit (512 MiB unless `GOMEMLIMIT` is
   set), at most 4 workers.

## Phases

0. Guardrails: guard, gitx, source-scan test, Makefile, Docker e2e, CI.
1. Core: keys and recovery, seal, cache, check, file and SQLite sources,
   Hermes preset.
2. Restore: restore, cat, verify, doctor, textconv diffs, signed index,
   round-trip CI test.
3. Postgres (Honcho, Hindsight), OpenViking.
4. Size: splitting, prune, maybe content-defined chunking.
5. Release: GoReleaser, `spicy-lemonade/homebrew-tap`, public launch.
