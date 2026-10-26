# Frontend demo polish implementation plan

**Goal:** Make a new visitor's registration, navigation, chat and settings flows reliable enough for a portfolio review.

**Architecture:** Keep the existing Next static export and Go server. Repair state and feedback at their existing owners; use durable chat events for restored failure feedback and runtime state for running indicators.

**Scope approved:** Existing visual style, login consistency, mobile navigation/settings, Unicode titles, chat failures, save/copy feedback, regression checks, Docker rollout.

- [x] Authentication: one LoginScreen at every entry, explicit unavailable/retry state, fresh role check after login, awaited logout. Verify registration → home → refresh and protected routes.
- [x] Navigation/layout: close mobile drawer after navigation; responsive settings navigation with minmax(0,1fr), accessible title; retain desktop layout. Verify at the actual narrow browser viewport.
- [x] Chat: Unicode-safe session titles/previews, restore latest turn failure from events, distinguish actual running state from stale extraction anchors, readable Chinese error messages and retained history on failed reload. Add Go/API regressions.
- [x] Operations: check failed HTTP/JSON responses before success, keep drafts/dialogs on save failure, restore pending state in finally, clipboard fallback with visible failures. Cover account, agents, projects and chat titles.
- [ ] Validation: frontend unit tests, TypeScript, ESLint and production export; affected Go packages; browser registration/chat/project/settings/history checks. Commit and push the isolated branch, update server checkout using fast-forward only, build a revision-labelled Docker image, replace only bkcrab, and verify health and browser flows. Preserve private server configuration and existing data.

**Baseline:** Server and origin/main at adbe84c; 77 frontend tests pass, TypeScript/build pass, ESLint has 20 warnings and no errors. Registration was closed; permanently enabled at user request. DeepSeek Flash was configured as the working system default at user request; no credentials belong in this document or source control.

**Pre-rollout validation:** 84 frontend tests pass; production Next export passes with a local pnpm 10.15.0 installation; session package and targeted worker-status regressions pass. Full setup suite and Linux confirmation are pending before rollout.
