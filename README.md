# 🧂 Salt

[![CI](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml/badge.svg)](https://github.com/spicy-lemonade/salt/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![codecov](https://codecov.io/gh/spicy-lemonade/salt/graph/badge.svg)](https://codecov.io/gh/spicy-lemonade/salt)

Salt encrypts your agent's files and database memories before they leave your machine.

<div align="center">
  <img src="https://github.com/user-attachments/assets/6f4a37f9-0aff-4c04-872a-9fb79af5df96" width="3840" height="2160" alt="salt banner" />
</div>

## 🤔 Why Salt

AI agents remember things about you. They keep notes like `USER.md`, `MEMORY.md` and `SOUL.md`, plus memory databases. If you back these up to GitHub, anyone who gets into that repo can read them.

Salt locks your files before they reach GitHub. Anyone who looks inside only sees scrambled data. Even the file names are hidden.

## 📦 Install

Homebrew support is coming soon. For now you can install Salt with Go.

```bash
go install github.com/spicy-lemonade/salt/cmd/salt@latest
```

## 🚀 Get started

Set up Salt in your backup repo.

```bash
salt init ~/my-backup-repo
```

Salt creates your key and helps you save a way to get it back. It also adds a check that stops you from ever committing a file that isn't locked.

Then lock your files into the repo whenever you back up.

```bash
salt seal ~/agent-files ~/my-backup-repo
```

Commit and push as normal. Files that haven't changed stay the same, so a backup with no changes makes no commit.

## 🔑 Getting your files back

When you set up Salt you pick one of two ways to recover your key if you lose your laptop.

- **12 recovery words** (recommended). Write them down and keep them safe. The words are your key, so nothing secret is stored in your repo.
- **A passphrase** you choose. Make it strong and keep it in a password manager like Bitwarden.

To get your files back, run this and follow the prompts.

```bash
salt restore ~/my-backup-repo --to ~/restored-files
```

## 🩺 Checking everything works

- `salt doctor` checks your setup and tells you if anything needs fixing.
- `salt verify` makes sure every file in your backup can be unlocked.
- `salt recovery test` checks your recovery words or passphrase still work.
