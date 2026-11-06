---
tags: [new-registry-source]
max_turns: 150
timeout_seconds: 3600
allowed_tools: [Read, Glob, Grep, Skill, Write, Edit, Bash, WebFetch]
---

add an open vsx source to dug (the vscode extension registry at open-vsx.org). i want `dug list openvsx` to walk the public catalog with download counts and ratings as metadata so i can order by popularity, and version history for --version-depth. make sure it's wired into the workspace like the other sources are
