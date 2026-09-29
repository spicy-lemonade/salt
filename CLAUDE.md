# CLAUDE.md

## Important Rules

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

## Project Overview

Salt is an open-source Go CLI that encrypts AI-agent memory backups before they are pushed to Git. Examples are Hermes with Mnemosyne SQLite databases and Markdown files such as USER.md, MEMORY.md, SOUL.md and SKILL.md. It will later cover Honcho and Hindsight (Postgres + pgvector) and OpenViking. An example usage is a nightly backup script which saves a snapshot of the agent files, but runs `salt seal` to encrypt them before they reach the git repo. A pre-commit hook (`salt check`) then refuses any commit containing a file that is not encrypted. Salt is distributed through a Homebrew tap. The full design is in `docs/design.md`.

**Key Technologies:**
- Go 1.26+
- age (`filippo.io/age`) for encryption: X25519 keys, and scrypt for passphrase-wrapped keys
- zstd (`github.com/klauspost/compress/zstd`) for compression before encryption
- BIP39 12-word recovery phrases; the age key is derived from the phrase with HKDF-SHA256
- OS keychain via `github.com/zalando/go-keyring`, with a 0600 file fallback (`SALT_KEYSTORE=file`)

**Commands:** `init`, `seal`, `check`, `restore`, `verify`, `doctor`, `recovery test|show`, `hook install`, `version`.

**Package layout:**
- `cmd/salt`: CLI entry point and flag parsing
- `internal/app`: commands; talks to the person only through `UI` and to git only through `GitOps`
- `internal/seal`: streaming seal, restore and verify; encrypted index; change-detection cache
- `internal/keys`: recovery phrase, key derivation, passphrase wrapping, key stores
- `internal/repo`: backup repo layout, format file, recipients, public-file allowlist
- `internal/check`: pre-commit plaintext detection
- `internal/hook`: pre-commit hook script and installation
- `internal/gitx`: the only way salt runs git (hooks always disabled)
- `internal/guard`: refuses nested salt processes; sets a soft memory limit
- `internal/rules`: source-scan test enforcing the process-safety rules

### Data Architecture

**Backup repository layout** (a git repo owned by salt):
- `.salt/format.json`: public; layout version, `encrypt_paths`, recovery method
- `.salt/recipients.txt`: public; the age public keys every file is encrypted to
- `.salt/key.age`: passphrase-wrapped private key (passphrase recovery only)
- `index.age`: encrypted JSON index holding real paths, SHA-256 of the plaintext, sizes, modes and symlinks
- `objects/xx/<random>.age`: file contents when paths are encrypted (the default)
- `files/<path>.age`: file contents with `--plain-paths`
- `README.md`, `LICENSE`, `.gitignore`, `.gitattributes`: the only other files allowed unencrypted

**Pipeline:** file -> zstd -> age -> object, fully streamed with fixed buffers and at most 4 workers. No data file is ever read whole into memory.

**Keys:**
- Sealing needs only the public key, so encrypting a backup never needs anything secret.
- The private key lives in the OS keychain.
- Recovery is either a 12-word phrase (the recommended option, where the words are the key, and nothing secret is stored in the repo) or a user-chosen passphrase that wraps `.salt/key.age`.

**Change detection:** age output is randomised, so a local cache (the OS cache dir, `seal-<hash>.json`, 0600, never committed) maps plaintext hashes to existing ciphertext. Unchanged files keep their ciphertext, and an unchanged snapshot produces no commit.

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

# Run a specific test
go test ./internal/seal -run TestRoundTrip
```

### Code Quality


## Project notes
- The project uses Go 1.26+ (module `github.com/spicy-lemonade/salt`).
- **Memory and process safety come first.** An earlier attempt crashed the machine when tests kept restarting themselves. Follow the rules in `docs/design.md` under "Process and memory safety", and don't weaken them.
- Never touch the real keychain in tests, use `keys.MemStore` in unit tests and `SALT_KEYSTORE=file` in e2e.
- `internal/keys` pins the phrase-to-key derivation (`TestIdentityFromEntropyPinned`). Changing `deriveSalt` or `deriveInfo` would make every existing recovery phrase useless.
- User-facing onboarding and warning copy is agreed wording (see `docs/design.md`, "Onboarding copy"). Do not reword it without asking.
- Salt prints to stderr only. A Hermes `--no-agent` cron job forwards any stdout as a message, so success must be silent on stdout.
- Decided out of scope is Touch ID gating, and switching recovery method from the 12 word passphrase to the user chosen passphrase or vice versa.
- Not yet built are the source adapters (SQLite, Postgres/pgvector, OpenViking), a `salt backup` preset, splitting files over 100 MB, the signed index, and the Homebrew tap and release.