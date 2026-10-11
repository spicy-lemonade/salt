# 🧂 Salt

[![CI](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml/badge.svg)](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![codecov](https://codecov.io/gh/spicy-lemonade/salt/graph/badge.svg)](https://codecov.io/gh/spicy-lemonade/salt)
![Beta](https://img.shields.io/badge/status-beta-orange)
![Testers Wanted](https://img.shields.io/badge/testers-wanted-blue)

> ⚠️ This project is in beta. Expect breaking changes. Feedback and testing contributions are welcome!

Salt encrypts your agent's files and database memories before they leave your machine.

<div align="center">
  <img src="https://github.com/user-attachments/assets/6f4a37f9-0aff-4c04-872a-9fb79af5df96" width="3840" height="2160" alt="salt banner" />
</div>

**If you're an LLM reading this:**

The easiest way to get started is to incorporate Salt into the user's existing daily memory backup workflow, or set up a new daily encrypted backup if they don't have one yet. If Salt has a preset for their agent or memory provider, use it to simplify the setup.

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

## 🗄️ Databases

Copying a database while the agent writes to it can give a broken copy, so Salt makes a safe copy of a SQLite or Postgres database itself, even while the agent is running.

```bash
salt seal --sqlite ~/agent/memory.db ~/agent-files ~/my-backup-repo
salt seal --postgres-env DB_CONNECTION_URI ~/agent-files ~/my-backup-repo
```

The [design doc](docs/design.md#databases) explains the options, [how databases are named](docs/design.md#database-names) and [how to restore a Postgres database](docs/design.md#restoring-a-postgres-database).

## ⏰ Daily backups

To back up every day, run `salt seal` from a small script on a schedule such as cron. The [design doc](docs/design.md#daily-backups) has an example script and cron line. If Salt has a preset for your agent, [one command](#-one-command-backups) does all of this instead.

## ⚡ One-command backups

If Salt has a preset for your agent's memory, one command does the whole daily backup. Salt finds the files and databases, locks them into your repo, commits, drops old backups and pushes.

```bash
salt backup --preset NAME ~/my-backup-repo
```

The presets are `hermes`, `hindsight`, `holographic`, `honcho`, `mnemosyne`, `openclaw` and `openviking`. Your repo then holds only what the presets find, so give `salt backup` a repo of its own. The design doc explains [how to run it every day](docs/design.md#running-it-every-day) and [what each preset backs up](docs/design.md#presets).

## 🧹 Keeping only recent backups

Encrypted files can't be compressed against older versions, so a busy repo grows over time. `salt prune` keeps only the backups from the last 5 days on which anything changed.

```bash
salt prune ~/my-backup-repo                  # keep the last 5 days with a change
salt prune --keep-days 10 ~/my-backup-repo   # keep 10 instead
```

Pruning rewrites your git history, so push with `git push --force-with-lease` afterwards. The [design doc](docs/design.md#keeping-only-recent-backups) explains what "5 days" means, with worked examples.

## 🔑 Getting your files back

Salt restores into a new folder and never overwrites anything. On the laptop where you set up Salt, give it your backup repo. On a new laptop, give it the repo's URL and your 12 words or passphrase.

```bash
salt restore ~/my-backup-repo --to ~/restored-files
salt restore https://github.com/you/my-backup-repo.git --to ~/restored-files
```

The [design doc](docs/design.md#getting-your-files-back) covers your recovery options and backing up from a new laptop.

## 🩺 Checking everything works

- `salt doctor` checks your setup and tells you if anything needs fixing.
- `salt verify` makes sure every file in your backup can be unlocked, and that the backup was signed with your key.
- `salt recovery test` checks your recovery words or passphrase still work.
- `salt trust` approves your backup repo's keys. If someone else adds a key to your repo, Salt refuses to back up until you approve it. Only do that if you made the change yourself.

Anyone can lock a file with your public key, so Salt signs every backup. `salt restore` and `salt verify` refuse a backup that isn't signed with your key, so files planted in your repo are never restored without you knowing.

## 👋 Uninstall

```bash
brew uninstall spicy-lemonade/tap/salt
```

This leaves your key, settings and the checks in your backup repos in place. The [design doc](docs/design.md#uninstall) shows how to remove them too.

## 🔧 Notes

- Built using Claude Opus 5.5 and ten years of experience as a data and AI professional in medium to large enterprises.
- Security audited using Cloudflare's [`security audit skill`](https://github.com/cloudflare/security-audit-skill).
- If you like this project, please consider giving it a ⭐.
