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

Use the full name. A plain `brew install salt` installs a different tool called SaltStack.

You can also install Salt with Go.

```bash
go install github.com/spicy-lemonade/salt/cmd/salt@latest
```

## 🚀 Get started

Set up Salt in your backup repo. This is any git repo you push your backups to.

```bash
salt init ~/my-backup-repo
```

Salt creates your key, helps you save a way to get it back, and adds a check that stops you committing a file that isn't locked. File names are hidden by default. To see them on GitHub, such as `Memories/USER.md`, use `salt init --plain-paths` instead. The contents stay encrypted either way.

Then lock your files into the repo whenever you back up.

```bash
salt seal --prune ~/agent-files ~/my-backup-repo
```

Commit and push as normal. The repo always matches your files, with their last-modified dates. Unchanged files stay the same, deleted files are removed, and a backup with no changes makes no commit. `--prune` clears out anything Salt didn't put there. Older versions stay in your git history, so you can check out an older commit and restore it, until `salt prune` drops them (see [Keeping only recent backups](#-keeping-only-recent-backups)).

## ⚡ One-command backups

If Salt has a preset for your agent's memory, one command does the whole daily backup. Salt finds the files and databases, makes a safe copy of each database, locks everything into your repo, commits, drops old backups and pushes. `salt help` lists the presets.

```bash
salt backup --preset NAME ~/my-backup-repo
```

Run `crontab -e` and add this line to run it every day at 6am. Use the path that `which salt` prints, since cron doesn't search Homebrew's folder.

```
0 6 * * * /opt/homebrew/bin/salt backup --preset NAME "$HOME/my-backup-repo" >> "$HOME/backup.log" 2>&1
```

- Your repo then holds only what the presets find, and anything else in it is removed, as `salt seal --prune` does. Give `salt backup` a repo of its own, and repeat `--preset` for each tool you use.
- A settings file holding an API key or other secret is left out, with one line naming the file and setting. Keep secrets in environment variables so the file is backed up.
- `--keep-days N` sets how many days with a change to keep. The default is 5, as with `salt prune`.
- The repo needs a remote named `origin`, and `git push` must work without asking for a password. Running `gh auth setup-git` once is an easy way to do this. Salt never overwrites a backup this machine didn't push, even after a `git fetch`.
- Salt prints nothing when the backup works, but names any folder from the last backup it can't find, such as on an unmounted drive.

### Presets

| Preset | What it backs up |
|---|---|
| `mnemosyne` | [Mnemosyne](https://github.com/mnemosyne-oss/mnemosyne) memory, on its own or inside Hermes and its profiles |

A preset is a small JSON file in [`internal/preset/presets`](internal/preset/presets) that says where a tool keeps its memory. The [design doc](docs/design.md#presets) shows what each one backs up. If the tool's folders are set by environment variables, set them in the cron line too, since cron doesn't see your shell's variables.

To get your memory back, restore into a new folder (see [Getting your files back](#-getting-your-files-back)), stop the agent, then copy each folder back to where it came from.

To add a preset for another tool, copy an existing file, change the paths, and open a pull request. The [design doc](docs/design.md#one-command-backup) explains the format.

## ⏰ Daily backups

For files without a preset, run Salt from a small script once a day. Change the paths to match your setup.

```bash
#!/bin/bash
set -euo pipefail
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

REPO="$HOME/my-backup-repo"
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

# Gather the files you want to back up
mkdir -p "$STAGE/memories"
cp -p ~/agent/SOUL.md ~/agent/config.yaml "$STAGE/"
cp -p ~/agent/memories/*.md "$STAGE/memories/"
cp -Rp ~/agent/skills "$STAGE/"

# Lock the files and a safe copy of the database into the repo. If anything
# changed, commit, drop old backups and push.
salt seal --prune --sqlite ~/agent/memory.db "$STAGE" "$REPO"
cd "$REPO"
git add -A
if ! git diff --cached --quiet; then
  git commit -m "Backup $(date '+%Y-%m-%d')"
  salt prune "$REPO"
  git push --force-with-lease
fi
```

Save it as `~/backup.sh`, then run `crontab -e` and add this line to run it every day at 6am.

```
0 6 * * * /bin/bash ~/backup.sh >> ~/backup.log 2>&1
```

Make sure `git push` works without asking for a password. Running `gh auth setup-git` once does this. Salt only prints messages when something goes wrong.

## 🧹 Keeping only recent backups

Every change adds the whole changed file to your repo again, since encrypted files can't be compressed against older versions. Over months a busy repo gets slow to clone and push. `salt prune` keeps only recent backups.

```bash
salt prune ~/my-backup-repo                  # keep the last 5 days with a change
salt prune --keep-days 10 ~/my-backup-repo   # keep 10 instead
```

**"5 days" means 5 days on which anything in the repo changed, not 5 calendar days.** Days are counted for the whole repo, never per file, and a day with no changes is skipped. The latest backup is always kept, so the current version of every file is too. Pruning rewrites your git history, so push with `git push --force-with-lease` afterwards. The [design doc](docs/design.md#keeping-only-recent-backups) has worked examples, and a way to keep a database for longer.

## 🗄️ Databases

A plain `cp` of a database while the agent is writing to it can give a broken copy. Salt makes a safe copy for you, even while the agent is running.

```bash
salt seal --sqlite ~/agent/memory.db "$STAGE" "$REPO"
salt seal --postgres-env DB_CONNECTION_URI "$STAGE" "$REPO"
```

`--postgres-env` reads the database address from an environment variable, so the password never appears on the command line. The [design doc](docs/design.md#databases) explains the options and [how to restore a Postgres database](docs/design.md#restoring-a-postgres-database).

A SQLite database is backed up under its file name, and a Postgres database under its name with `.sql`. If two have the same name, such as two agents that both keep a `state.db`, put `--name NAME` before one of them, such as `--name agent2/state.db --sqlite ~/agent2/state.db`.

## 🔑 Getting your files back

When you set up Salt, you pick how to recover your key if you lose your laptop.

- **12 recovery words** (recommended). The words are your key, and nothing is stored in your repo. Write them down in at least two places, one of them offline.
- **A passphrase** you choose. Make it strong and keep it in a password manager like Bitwarden.

Your key is saved on the laptop where you set up Salt, so restoring there takes one command.

```bash
salt restore ~/my-backup-repo --to ~/restored-files
```

On a new laptop, install Salt and restore straight from your backup repo's URL. Salt downloads only the latest backup, still encrypted, and removes the download afterwards. It asks for your 12 words or passphrase, then offers to save the key.

```bash
salt restore https://github.com/you/my-backup-repo.git --to ~/restored-files
```

SSH URLs such as `git@github.com:you/my-backup-repo.git` work too, using the login git already has, such as an SSH key. Avoid putting a token in the URL, since other programs on your laptop can see the command line.

Salt restores into a new folder, with the original names and folders, and never overwrites anything, so you can check the files before copying them back.

Backing up never needs the key that unlocks your backups, only a signing key Salt keeps in a private file. So you can delete the private key from your laptop (see [Uninstall](#-uninstall)) and keep just your 12 words.

To back up from a new laptop, clone your backup repo, then run `salt trust my-backup-repo` once to approve its keys. If your key isn't saved there, Salt asks for your 12 words or passphrase once to set up signing.

## 🩺 Checking everything works

- `salt doctor` checks your setup and tells you if anything needs fixing.
- `salt verify` makes sure every file in your backup can be unlocked, and that the backup was signed with your key.
- `salt recovery test` checks your recovery words or passphrase still work.
- `salt trust` approves your backup repo's keys. If someone else adds a key to your repo, Salt refuses to back up until you approve it. Only do that if you made the change yourself.

Anyone can lock a file with your public key, so Salt signs every backup. `salt restore` and `salt verify` refuse a backup that isn't signed with your key, so files planted in your repo are never restored without you knowing.

## 👋 Uninstall

To uninstall Salt, run these two commands. The second is optional and makes Homebrew forget the Salt tap.

```bash
brew uninstall spicy-lemonade/tap/salt
brew untap spicy-lemonade/tap
```

This leaves your key and settings on your machine. Before removing them too, make sure you still have your recovery phrase or passphrase. Without it, your backups can never be unlocked again.

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
