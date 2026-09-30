# OpenIM channel

Approved scope: ordinary OpenIM bot accounts, single chat and explicitly mentioned group chat, complete text replies. Reuse OpenIM Server webhooks and REST APIs; no OpenIM fork, SDK change, or streaming UI.

## Boundaries

- Each binding identifies a normalized OpenIM API URL plus bot user ID. Separate instances and bots must have separate conversation namespaces.
- The existing configuration schema permits one OpenIM binding per user/agent pair. Multiple agents can bind different bots on the same instance. Replacing a different bot requires disconnecting the old binding first.
- An instance has one OpenIM webhook URL. bkcrab dispatches its callbacks across registered bots of that instance; a common webhook secret is derived with HMAC from the instance administrator secret. All bots for an instance use the same administrator credentials.
- Credentials remain server-side. The connection form validates administrator credentials and the existing ordinary bot user; it does not create users or alter friend/group relationships.
- Group responses require both an explicit bot mention and an operator-configured group allowlist. Ignore self messages and non-chat events. Initial supported inputs are text and at-text; unsupported content is acknowledged without invoking the agent.
- Register a durable SQL ingress queue before acknowledging accepted callbacks. A worker claims pending records atomically, sends them to the existing message bus, and marks them dispatched. Retain dispatched IDs for seven days. This guarantees durable ingress, not exactly-once agent execution: the existing bus/task queue has no durable completion acknowledgement. OpenIM's own callback failure compensation remains a separate deployment concern.
- Pending ingress is scoped to the binding owner and agent as well as the bot, so reassignment does not replay another binding's pending work. User/group IDs are limited to 64 bytes to fit existing session columns; external chatter IDs are hashed within the OpenIM instance namespace.
- The adapter sends complete plain-text replies using `/msg/send_msg`, caches administrator tokens, refreshes on expiration, and retries once only on a definite token rejection. It must not automatically retry ambiguous send failures.
- Existing agent behavior and other channels remain unchanged. OpenIM group dedup uses stable message IDs instead of content hashes.

## Deployment

Document Docker networking, callback secret handling, bot registration, group membership, API token scope, and failure limitations. Build/test locally; live OpenIM validation requires the actual API address and credentials. Do not interrupt the running server for this implementation.
