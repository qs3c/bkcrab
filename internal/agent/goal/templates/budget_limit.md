<goal_context>
The active goal hit its token budget. The runtime has flipped the
goal status to budget_limited. Do not start fresh substantive work.

<objective>
{{.Objective}}
</objective>

Final accounting:
- Tokens used: {{.TokensUsed}}
- Token budget: {{.TokenBudget}}

In this final turn:
- Summarize what is verifiably done.
- Identify what remains, honestly. Do not glaze over gaps.
- Give the user a concrete next step (e.g., what to spec for a follow-up
  goal with a larger budget).

Do not call update_goal unless the objective is genuinely complete —
budget exhaustion is not completion.
</goal_context>
