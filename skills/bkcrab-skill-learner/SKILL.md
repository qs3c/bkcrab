---
name: bkcrab-skill-learner
description: Analyze conversations to extract reusable skill patterns. Used internally by BkCrab to auto-generate skills from complex multi-step tasks.
metadata:
  bkcrab:
    always: true
---

# Skill Learner

Analyze a conversation and determine if it demonstrates a reusable multi-step workflow that should be saved as a skill.

## Input

You receive the full working context of one session: the recent span is verbatim messages (including tool calls and results); if the session was long, the older span appears as a `[Conversation Summary]` block. Treat the summary as reliable background narrative and mine the verbatim span for concrete steps.

## When to Extract

Extract a skill when the conversation shows at least one of:

- A repeatable multi-step workflow — multiple tool calls in a clear sequence that forms a general procedure useful beyond this specific conversation (the runtime already enforces a configurable minimum before you are consulted)
- A hard-won approach — the task required trial and error, or the course changed because of findings along the way; capture the path that finally worked AND the dead ends to avoid
- An expectation correction — the user expected a different method or outcome than the first attempt; capture what they actually wanted so the next run starts there

Do NOT extract when:

- The task is one-off or highly specific to current context
- The steps are standard and don't need specialized instructions
- The workflow is trivially simple (not just "read a file and summarize it")

If a skill with the same slug already exists, still output the extraction. The runtime will compare both versions and decide whether to merge them.

## How to Analyze

Given a conversation transcript, identify:

1. **The core workflow** — What sequence of actions was performed?
2. **The turning points** — Where did an attempt fail, and what change made it work? Pitfalls are often more valuable than the happy path.
3. **The pattern** — Is this generalizable to other inputs/contexts?
4. **The value** — Would having this as a skill save significant effort next time?

## Output Format

If the conversation demonstrates a reusable skill, output JSON:

```json
{
  "extract": true,
  "skill": {
    "name": "Human Readable Name",
    "slug": "kebab-case-slug",
    "description": "One-line description of what this skill does and when to trigger it",
    "content": "Full SKILL.md content with YAML frontmatter and markdown instructions"
  }
}
```

If not reusable, output:

```json
{
  "extract": false
}
```

## Skill Content Guidelines

When generating the SKILL.md content:

- Include proper YAML frontmatter with `name` and `description`
- Write clear step-by-step instructions in markdown
- Generalize from the specific conversation — replace specific values with placeholders
- Explain the reasoning behind each step, not just the commands
- Include example inputs/outputs where helpful
- Keep under 500 lines
- Use `{baseDir}` for any bundled resource references

## Example Extraction

A conversation where the user asks to set up a new Go project with CI, and the agent creates go.mod, writes a Makefile, sets up GitHub Actions, and adds a Dockerfile — this is a good extraction candidate because:

- Multiple coordinated steps (4+ tool calls)
- Generalizable to any new Go project
- Saves significant setup time

The extracted skill would capture the project structure, file templates, and the sequence of steps, parameterized for project name and Go version.
