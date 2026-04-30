---
name: future-agent
description: Agent file con campos que che todavía no entiende.
model: sonnet
color: purple
tools: ["Read"]
capabilities:
  - planning
  - review
hooks:
  pre_run: ./scripts/setup.sh
mcpServers:
  - filesystem
permissionMode: ask
hypothetical_v2_field: arbitrary
---
Body irrelevante para el parser.
