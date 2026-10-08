# CLAUDE.md

## Important Rules

- **Source data is always the truth.** Salt must never modify, repair or convert the files it backs up; it encrypts them and restores them exactly as they were given (contents, dates and permissions).
- **Build for any agent memory, not one tool.** Hermes, Mnemosyne, Honcho, Hindsight and the others are examples. Salt's code, messages and defaults must work for any files, SQLite or Postgres database, whatever tool made them, with no tool-specific paths, names or special cases.
- **Presets are the one place tool-specific paths live**, one JSON file per tool in `internal/preset/presets`. A memory tool that works with any agent, such as Mnemosyne, Honcho, Hindsight or OpenViking, gets a preset that looks where the tool keeps its data on its own, and also inside an agent's folder where that agent embeds it, so it works alone or beside any agent's preset. The exceptions are an agent's own built-in memory (`hermes`) and a memory tool built for one agent (`holographic`, a Hermes plugin), whose presets look only in that agent's folders. No preset depends on another, and any can be given with any other.
- *Never* run git commands without asking for user permission, even if 'auto-accept' is selected during a Claude Command session.
- *Never* run files which use an LLM API without asking for user permission,  even if 'auto-accept' is selected during a Claude Command session.
- *Never* attempt to re-engineer the code or alter data without asking for user permission.
- *Never* make assumptions. Ask for more information and wait for the user response.
- Do *not* use numerical prefixes when writing comments.
- Do *not* use newline characters in print statements.
- Use double quotation marks instead of single quotation marks when possible.
- Use type hinting.
- Favor modular, resuable code.
- Favor vectorised code.
- Use concurrency when processing data with an LLM API.
- Read existing files before writing any output.
- Do not re-read files unless they have been changed.
- Use gitmoji when commiting, by running `gitmoji -c` and selecting an appropriate emoji.

## Project Overview

Salt is an open-source Go CLI that encrypts AI-agent memory backups before they are pushed to Git. Examples are Hermes with Mnemosyne SQLite databases and Markdown files such as USER.md, MEMORY.md, SOUL.md and SKILL.md. It also covers Postgres databases such as Honcho and Hindsight (Postgres + pgvector), and will later cover OpenViking. An example usage is a nightly backup script which saves a snapshot of the agent files, but runs `salt seal` to encrypt them before they reach the git repo. A pre-commit hook (`salt check`) then refuses any commit containing a file that is not encrypted. Salt is distributed through a Homebrew tap. The full design is in `docs/design.md`.

Salt is a general-purpose open-source tool for public release. Write code, defaults, messages and docs for any user and any setup.

**Key Technologies:**
- Go 1.26+
- age (`filippo.io/age`) for encryption: X25519 keys, and scrypt for passphrase-wrapped keys
- zstd (`github.com/klauspost/compress/zstd`) for compression before encryption
- BIP39 12-word recovery phrases; the age key is derived from the phrase with HKDF-SHA256
- OS keychain via `github.com/zalando/go-keyring`, with a 0600 file fallback (`SALT_KEYSTORE=file`)

**Commands:** `init`, `seal [--sqlite DB] [--postgres CONN] [--postgres-env VAR] [--name NAME]`, `backup --preset NAME [--keep-days N]`, `prune`, `check`, `restore [--allow-unsigned]`, `verify [--allow-unsigned]`, `doctor`, `trust`, `recovery test|show`, `hook install`, `version`.

**Package layout:**
- `cmd/salt`: CLI entry point and flag parsing
- `internal/app`: commands; talks to the person only through `UI` and to git only through `GitOps`
- `internal/seal`: streaming seal, restore and verify; encrypted index; change-detection cache
- `internal/keys`: recovery phrase, key derivation, passphrase wrapping, key stores
- `internal/repo`: backup repo layout, format file, recipients, public-file allowlist
- `internal/check`: pre-commit plaintext detection
- `internal/hook`: pre-commit hook script and installation
- `internal/gitx`: the only way salt runs git (hooks, compression and delta search always off)
- `internal/source`: safe copies of live databases (runs `sqlite3` and `pg_dump`); each kind is a `Database` listed in `Kinds`; with `gitx` and `proc`, the only packages that start programs
- `internal/proc`: runs the programs `gitx` and `source` may stop part way (`git clone`, `git ls-remote`, `git rev-list`, `git push`, `git commit`, `sqlite3`, `pg_dump`); caps their error output and how long a stopped one is waited for
- `internal/preset`: presets for `salt backup`, one embedded JSON file per tool in `presets/`, read by the same tool-agnostic code; finds each preset's files, SQLite databases and the databases it names by connection (such as Postgres), and leaves out files holding secrets
- `internal/guard`: refuses nested salt processes; sets a soft memory limit; locks a backup repo so two salts never seal, back up or prune it at once
- `internal/prune`: keeps only the backups from the last N days with a change (counted for the whole repo, not per file), all of the latest day's and the last of each earlier day's, by rewriting the branch's history
- `internal/trust`: this machine's approved copy of each repo's keys and settings; seal refuses if the repo differs
- `internal/rules`: source-scan test enforcing the process-safety rules

### Data Architecture

**Backup repository layout** (a git repo owned by salt):
- `.salt/format.json`: public; layout version, `encrypt_paths`, recovery method
- `.salt/recipients.txt`: public; the age public keys every file is encrypted to
- `.salt/key.age`: passphrase-wrapped private key (passphrase recovery only)
- `index.age`: encrypted JSON index holding real paths, SHA-256 of the plaintext, sizes, modes, last-modified times and symlinks, signed with the signing key
- `objects/xx/<random>.age`: file contents when paths are encrypted (the default), and every chunk of a file over 16 MiB
- `files/<path>.age`: file contents with `--plain-paths`
- `README.md`, `LICENSE`, `.gitignore`, `.gitattributes`: the only other files allowed unencrypted

**Pipeline:** file -> zstd -> age -> object, fully streamed with fixed buffers and at most 4 workers. No data file is ever read whole into memory. A file over 16 MiB is cut into content-defined chunks of plaintext (FastCDC, 1 to 16 MiB, about 4 MiB on average, boundaries keyed from the signing key), each compressed and encrypted as its own object and reused by its plaintext hash, so a change uploads only the chunks around it. An index with parts or chunks is version 2. Older salts' 45 MiB parts are still read (see `docs/design.md`, "Large files").

**Keys:**
- Sealing needs only the public key and the signing key, so encrypting a backup never needs the decryption key.
- The signing key is Ed25519, derived from the age private key with HKDF-SHA256 (`keys.SigningKey`), and kept in a 0600 file in the OS config dir (`salt/signing/`). `salt init` and `salt trust` save it. Restore and verify refuse an index not signed by the key derived from one of the keys that decrypts it, unless `--allow-unsigned` is passed. Changing `signSalt` or `signInfo` would fail every existing backup's signature check.
- The private key lives in the OS keychain.
- Recovery is either a 12-word phrase (the recommended option, where the words are the key, and nothing secret is stored in the repo) or a user-chosen passphrase that wraps `.salt/key.age`.

**Approved keys:** `salt init` saves the repo's keys and file-name setting to the OS config dir (`salt/trusted/<hash>.json`, 0600). `salt seal` refuses if the repo's `.salt/recipients.txt` or `format.json` differ, because anyone who can push could otherwise add their own key. `salt trust` approves a change. All repo reads and writes go through `os.Root` so symlinks can't lead outside the repo.

**Change detection:** age output is randomised, so a local cache (the OS cache dir, `seal-<hash>.json`, 0600, never committed) maps plaintext hashes, of whole files and of chunks, to existing ciphertext. Unchanged files and chunks keep their ciphertext, a seal that fails still saves the entries of the files it finished, and an unchanged snapshot (same contents, permissions and last-modified dates) produces no commit. The same folder holds `copykey-<hash>` (0600), a random key per repo that database copies use in place of a random one of their own (for `pg_dump --restrict-key`), so an unchanged database also makes no commit.

**Restore:** decrypts into a temp directory, verifies every file against the index, then moves the result into place with owner-only permissions. An existing destination is moved aside, never overwritten.

### Testing & Quality

The standard `go test` framework is used for writing tests. Unit tests must never start processes: no `git`, no `go build`, no `salt`. `internal/rules` enforces this, and bans `os.Executable`. Anything that runs the real binary lives in `test/e2e` behind the `e2e` build tag.

```bash
# Unit tests (capped: -p 2, 120s timeout, GOMEMLIMIT=1GiB)
make test

# Vet, including the e2e package
make vet

# End-to-end tests against real git and the real hook. Builds salt once, caps the
# process count with ulimit, and uses SALT_KEYSTORE=file with a temp HOME, so the
# real keychain is never touched. Never run `go test -tags e2e` directly.
make e2e

# End-to-end tests with another sqlite3 first on salt's PATH, such as Homebrew's
make e2e E2E_PATH="$(brew --prefix sqlite)/bin"

# Run a specific test
go test ./internal/seal -run TestRoundTrip
```

## Project notes
- The project uses Go 1.26+ (module `github.com/spicy-lemonade/salt`).
- **Memory and process safety come first.** Follow the rules in `docs/design.md` under "Process and memory safety", and don't weaken them.
- Never touch the real keychain in tests, use `keys.MemStore` in unit tests and `SALT_KEYSTORE=file` in e2e.
- `internal/keys` pins the phrase-to-key derivation (`TestIdentityFromEntropyPinned`). Changing `deriveSalt` or `deriveInfo` would make every existing recovery phrase useless.
- User-facing onboarding and warning copy is agreed wording (see `docs/design.md`, "Onboarding copy"). Do not reword it without asking.
- Salt prints to stderr only. Scheduled jobs (cron, agent schedulers) often send any stdout on as an email or message, so success must be silent on stdout.
- Decided out of scope is Touch ID gating, and switching recovery method from the 12 word passphrase to the user chosen passphrase or vice versa.
- `salt seal --sqlite DB` makes a safe copy of a live SQLite database itself (`internal/source` runs `sqlite3 .backup`). `salt seal --postgres CONN` (or `--postgres-env VAR`) dumps a Postgres database as plain SQL with `pg_dump`, with the password passed in `PGPASSWORD` and never shown. See `docs/design.md`, "Databases".
- `salt backup --preset NAME REPO` gathers what the presets name, seals it (the repo then holds only that), commits with hooks off after running the `salt check` logic in-process, prunes and pushes with `--force-with-lease`. It prints nothing on success. See `docs/design.md`, "One-command backup".
- Not yet built: OpenViking support, and presets other than `hermes`, `hindsight`, `holographic`, `honcho` and `mnemosyne` (OpenClaw).

### Agent contributing rules

These rules are for AI agents working on Salt for external contributors. Follow them for every change.

**Tests are required.**
- Every change must come with tests. A pull request without tests will not be accepted.
- New behaviour needs tests for both the normal case and the failure cases, such as bad input, missing files, a wrong key or a closed terminal.
- A bug fix needs a test that fails without the fix and passes with it.
- Combined coverage (unit and end-to-end together) must stay at 90% or above. Never lower it.

**Where tests go.**
- Unit tests sit next to the code they test, in the same package. They must never start another program. No `git`, no `go build`, no running `salt`. `internal/rules` fails the build if they do.
- Anything that needs real git or the real `salt` binary goes in `test/e2e`, in a file starting with `//go:build e2e`. Use the `newEnv` helper there so every run gets a temporary home folder.
- Commands in `internal/app` are tested through the fake `UI` (`scriptUI`) and fake git (`fakeGit`) in `app_test.go`. Reuse them rather than calling real git.

**Keep tests safe.**
- Never touch the real keychain. Use `keys.MemStore` in unit tests. Any test package that could reach `keys.KeyringStore` must call `keyring.MockInit()` first. End-to-end tests already use `SALT_KEYSTORE=file`.
- Only write inside `t.TempDir()`. Never read or write the real `~/.hermes`, a real backup repo or the real home folder.
- Set `keys.WrapWorkFactor = 10` in any test package that locks keys with a passphrase. At full strength each lock uses 256 MB of memory.
- Don't weaken `internal/rules`, `internal/guard` or the limits in the `Makefile`. They stop tests from crashing the machine.

**How to test locally.** Run these from the repo root, in this order. All of them must pass before you open a pull request.

```bash
# Formatting. Must print nothing.
gofmt -l .

# Look for common mistakes
make vet

# Unit tests. Writes coverage.out
make test

# End-to-end tests. Writes coverage-e2e.out
make e2e

# Combined coverage. Must be 90% or above
(cat coverage.out; tail -n +2 coverage-e2e.out) > coverage-all.out
go tool cover -func=coverage-all.out | tail -1
```

To see which lines are not covered, open `go tool cover -html=coverage-all.out`.

**Never:**
- run `go test -tags e2e` directly. Always use `make e2e`, which sets the safety limits.
- run `salt init`, `salt restore` or `salt recovery` on your own machine outside a temporary folder. They write to the real keychain.
- change the recovery-phrase derivation (`deriveSalt`, `deriveInfo`), the signing-key derivation (`signSalt`, `signInfo`) or the agreed onboarding wording in `docs/design.md`.
