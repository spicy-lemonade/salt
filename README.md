# 🧂 Salt

[![CI](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml/badge.svg)](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![codecov](https://codecov.io/gh/spicy-lemonade/salt/graph/badge.svg)](https://codecov.io/gh/spicy-lemonade/salt)

Salt encrypts your agent's files and database memories before they leave your machine.

<div align="center">
  <img src="https://github.com/user-attachments/assets/6f4a37f9-0aff-4c04-872a-9fb79af5df96" width="3840" height="2160" alt="salt banner" />
</div>

## 🤔 Why Salt

AI agents remember things about you. They keep notes like `USER.md`, `MEMORY.md` and `SOUL.md`, plus memory databases like SQLite or Postgres. If you back these up to GitHub, anyone who gets into that repo can read them.

Salt encrypts your files before they reach GitHub. Anyone who looks inside only sees scrambled data. Even the file names are hidden, because a name like `job_search_new_york.md` can say a lot on its own.

Your agent's files on your laptop stay as they are. Only the backup copy is encrypted, since that's the part that could leak. We still recommend a private repo, with Salt as an extra layer on top of GitHub's access controls.

## 📦 Install

```bash
brew install spicy-lemonade/tap/salt
```

Use the full name above. (A plain `brew install salt` installs a different tool called SaltStack, so don't do that!).

You can also install Salt with Go.

```bash
go install github.com/spicy-lemonade/salt/cmd/salt@latest
```

## 🚀 Get started

Set up Salt in your backup repo. This is any git repo you push your backups to.

```bash
salt init ~/my-backup-repo
```

Salt creates your key and helps you save a way to get it back. It also adds a check that stops you from ever committing a file that isn't locked.

Salt hides your file names by default. If you'd rather see your folders and file names, e.g. `Memories/USER.md` on GitHub, set up with `salt init --plain-paths ~/my-backup-repo` instead. The file contents stay encrypted either way.

Then lock your files into the repo whenever you back up.

```bash
salt seal --prune ~/agent-files ~/my-backup-repo
```

Commit and push as normal. Your backup always matches your files: unchanged files stay the same, deleted files are removed, and a backup with no changes makes no commit. Each file's last-modified date is saved too, and `salt restore` puts it back. `--prune` also clears out anything Salt didn't put there. Older versions stay in your git history, so you can check out an older commit and run `salt restore` on it.

## ⏰ Daily backups

Most people run Salt from a small script once a day. Here is an example of a daily backup for Hermes. Just change the paths to match your setup.

```bash
#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

REPO="$HOME/my-backup-repo"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

# Gather the files you want to back up
mkdir -p "$STAGE/memories"
cp -p ~/.hermes/SOUL.md ~/.hermes/config.yaml "$STAGE/"
cp -p ~/.hermes/memories/*.md "$STAGE/memories/"
cp -Rp ~/.hermes/skills "$STAGE/"

# Copy databases safely, even while the agent is running
sqlite3 ~/.hermes/state.db ".backup '$STAGE/state.db'"
sqlite3 ~/.hermes/mnemosyne/data/mnemosyne.db ".backup '$STAGE/mnemosyne.db'"

# Keep each database's real last-modified date on its copy
touch -r ~/.hermes/state.db "$STAGE/state.db"
touch -r ~/.hermes/mnemosyne/data/mnemosyne.db "$STAGE/mnemosyne.db"

# Lock everything into the repo, then commit and push if anything changed
salt seal --prune "$STAGE" "$REPO"
cd "$REPO"
git add -A
if ! git diff --cached --quiet; then
  git commit -m "Backup $(date '+%Y-%m-%d')"
  git push
fi
```

Save it as `~/backup.sh`, then run `crontab -e` and add this line to run it every day at 6am.

```
0 6 * * * /bin/bash ~/backup.sh >> ~/backup.log 2>&1
```

Make sure `git push` works without asking for a password. Running `gh auth setup-git` once is an easy way to do this.

Salt only prints messages when something goes wrong, so a successful backup is silent.

## 🗄️ Databases

Salt encrypts database files like any other file, exactly as they are. So copy a database safely first. A plain `cp` while the agent is running can give a broken copy, and you'd only find out when you restore it. Use `sqlite3 ... ".backup ..."` as in the script above, or `pg_dump` for Postgres databases such as Honcho or Hindsight. Salt will soon do this for you.

To keep several snapshots, give each copy a dated name, such as `memory-2026-09-30.db`, and keep them in the folder you back up.

## 🔑 Getting your files back

When you set up Salt you pick one of two ways to recover your key if you lose your laptop.

- **12 recovery words** (recommended). The words are your key, and nothing is stored in your repo. Write them down in at least two places, one of them offline.
- **A passphrase** you choose. Make it strong and keep it in a password manager like Bitwarden.

Your key is saved on the laptop where you set up Salt, so restoring there takes one command.

```bash
salt restore ~/my-backup-repo --to ~/restored-files
```

On a new laptop, install Salt, download your backup, and restore it. Salt asks for your 12 words or passphrase, then offers to save the key so you aren't asked again.

```bash
git clone --depth 1 https://github.com/you/my-backup-repo.git
salt restore my-backup-repo --to ~/restored-files
```

Salt puts your files in a new folder with their original names and folders. It never overwrites anything, so you can check them before copying them back.

Backing up only needs the public key in your repo. The private key is only needed to restore, so you can delete it from your laptop (see [Uninstall](#-uninstall)) and keep just your 12 words.

To start backing up from the new laptop, run `salt trust my-backup-repo` once. Salt shows which keys your backups are locked with and asks you to approve them. It won't back up until you do.

## 🩺 Checking everything works

- `salt doctor` checks your setup and tells you if anything needs fixing.
- `salt verify` makes sure every file in your backup can be unlocked.
- `salt recovery test` checks your recovery words or passphrase still work.
- `salt trust` approves your backup repo's keys on your laptop. If someone else adds a key to your repo, Salt refuses to back up and tells you. Only run `salt trust` if you made the change yourself.

## 👋 Uninstall

To uninstall Salt, run these two commands. The second one is optional and tells Homebrew to forget the Salt tap.

```bash
brew uninstall spicy-lemonade/tap/salt
brew untap spicy-lemonade/tap
```

Uninstalling leaves your key and settings on your machine. To remove them too, first make sure you still have your recovery phrase or passphrase. Without it, your backups can never be unlocked again.

```bash
# macOS: remove the key from the Keychain (run it again for each extra key if you have more than one)
security delete-generic-password -s salt

# Linux: remove the key from the system keyring
secret-tool clear service salt

# Remove approved keys, the key file (if you used SALT_KEYSTORE=file) and the cache
rm -rf ~/Library/Application\ Support/salt ~/Library/Caches/salt   # macOS
rm -rf ~/.config/salt ~/.cache/salt                                # Linux

# In each backup repo, remove the check. Otherwise every commit will be refused
rm ~/my-backup-repo/.git/hooks/pre-commit
```

## 🔧 Notes

- Built using Claude Opus 5.5 and ten years of experience as a data and AI professional in medium to large enterprises.
- Security audited using Cloudflare's [`security audit skill`](https://github.com/cloudflare/security-audit-skill).
- If you like this project, please consider giving it a ⭐.
