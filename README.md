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

Salt locks your files before they reach GitHub. Anyone who looks inside only sees scrambled data. Even the file names are hidden.

## 📦 Install

```bash
brew install spicy-lemonade/tap/salt
```

Use the full name above. (A plain `brew install salt` installs a different tool called SaltStack, so don't do that!).

You can also install Salt with Go.

```bash
go install github.com/spicy-lemonade/salt/cmd/salt@latest
```

To uninstall Salt, run these two commands. The second one is optional and tells Homebrew to forget the Salt tap.

```bash
brew uninstall spicy-lemonade/tap/salt
brew untap spicy-lemonade/tap
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

Commit and push as normal. Files that haven't changed stay the same, so a backup with no changes makes no commit. The `--prune` option clears out anything in the repo that Salt didn't put there.

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

Salt encrypts database files too, such as Mnemosyne's memory database. It treats them like any other file.

A database must be backed up before Salt encrypts it. For now you need to do this yourself. The script above uses `sqlite3 ... ".backup ..."` for SQLite. For Postgres databases, such as Honcho or Hindsight, use `pg_dump`.

Don't copy a database with plain `cp` while the agent is using it. The copy can be broken with no warning. This is a problem because Salt encrypts the broken copy exactly as it is. Decrypting it later gives you back the same broken database, and you only find out when you try to restore it.

For safety, Salt will soon make these backups for you.

## 🔑 Getting your files back

When you set up Salt you pick one of two ways to recover your key if you lose your laptop.

- **12 recovery words** (recommended). Write them down on paper and keep them safe. The words are your key, and nothing is stored in your repo. Write them down in at least two places, with one preferably being offline.
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

To start backing up from the new laptop, run `salt trust my-backup-repo` once. Salt shows which keys your backups are locked with and asks you to approve them. It won't back up until you do.

## 🩺 Checking everything works

- `salt doctor` checks your setup and tells you if anything needs fixing.
- `salt verify` makes sure every file in your backup can be unlocked.
- `salt recovery test` checks your recovery words or passphrase still work.
- `salt trust` approves your backup repo's keys on this laptop. If someone else adds a key to your repo, Salt refuses to back up and tells you. Only run `salt trust` if you made the change yourself.

## 🔧 Notes

- Built using Claude Opus 5.5 and ten years of experience as a data and AI professional in medium to large enterprises.
- Security audited using Cloudflare's [`security audit skill`](https://github.com/cloudflare/security-audit-skill).
- If you love this project, please consider giving it a ⭐.
