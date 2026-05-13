---
name: full-skill
title: Full Featured Skill
trigger: when the user wants the kitchen sink
task_type: domain
tags:
  - example
  - full
required_tools:
  - shell_run
required_namespaces:
  - tools.shell
required_tags:
  - allow-net
scope: project
---
This skill exercises every Skills.md section the importer normalises.

It carries a multi-paragraph description so the parser keeps the gap between paragraphs in the description.

## Steps

- Prepare the environment.
- Run the primary action.
- Verify the result.

## Preconditions

- The user is authenticated.
- The session has the `allow-net` tag.

## Failure modes

- Network unreachable: surface a typed error.
- Tool returns non-zero exit: bubble up.
