---
tags: [new-registry-source]
max_turns: 150
timeout_seconds: 3600
allowed_tools: [Read, Glob, Grep, Skill, Write, Edit, Bash, WebFetch]
---

Let's do the Jenkins plugins source from the todo list in sources/README.md. The update center JSON has every plugin with a download URL and a SHA-256, and plugin-versions.json has the history. Get it to the point where `just check` and `just build` pass and I can actually list the catalog against the real update center.
