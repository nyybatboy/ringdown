# agent-phone

**Give any AI agent a phone line.** Your agent writes a short brief: what it wants to come away with, its private limits, and the facts it may share. A realtime voice model places the call (or answers one) through Twilio, holds a natural conversation, and hands back the transcript and an outcome.

- **Self-hosted, bring your own keys.** One Go binary per line. Nothing is hosted for you.
- **Engine per call.** Named profiles (`hifi`, `standard`, `internal`) over Gemini Live, Grok Voice, OpenAI Realtime, ElevenLabs Agents or Amazon Nova Sonic.
- **MCP native.** Tools: `list_numbers`, `place_call`, `get_call`, `list_inbox`, `hang_up`, over Streamable HTTP or stdio.
- **A policy the agent can't override.** Allowlist or open dialling, countries, number types, hours, rate limits, a monthly spend limit, and a do-not-call memory, all enforced inside the server.
- **Inbound too.** The line can answer its own number with a standing brief ("take a message") and keep an inbox.
- **Checks you can run.** `dialout validate` checks a config without calling anything. `dialout check` has a running line call its own number and answer itself, then prints PASS or FAIL.

**If you are an AI agent, start with [AGENTS.md](AGENTS.md).** If you are placing calls through a line someone else installed, read [skill/agent-phone/SKILL.md](skill/agent-phone/SKILL.md).

## Quick start (laptop)

```bash
go build -o ./bin/dialout ./cmd/dialout
cp examples/agent-phone.yaml dialout.yaml
cp examples/secrets.env.example secrets.env && chmod 600 secrets.env   # fill in your keys
./bin/dialout validate -config dialout.yaml                             # PASS before going on
ngrok http 8090 --url <your-domain>.ngrok-free.dev                       # in another terminal
./bin/dialout serve -config dialout.yaml
./bin/dialout check -config dialout.yaml                                 # the line calls itself
```

On a server with a public address, `deploy/line/install-line.sh <line>` does it in one command, with no tunnel: see [docs/install.md](docs/install.md).

## Status

Early. In daily use on two lines since November 2026. Known limits:
- Gemini Live sessions end at about 10 minutes, which ends the call; use a Grok profile for long calls. The hard ceiling for any call is 2 hours.
- It can't press keys in phone menus (DTMF) yet.
- One outbound and one inbound call at a time per line.

## Your responsibility

You own compliance where you and the people you call are: laws on automated and AI calls, consent, recording, and your voice vendors' terms. By default the agent does not announce it is an AI. It says it is an assistant if asked. [`examples/prompt-disclose.tmpl`](examples/prompt-disclose.tmpl) discloses AI and transcription in the first sentence of every call. Nothing here is legal advice.

## Credits

A fork of [dialout](https://github.com/mfbonfigli/dialout) by mfbonfigli (MIT). Its README, with the full configuration, prompt and policy reference, is kept as [docs/reference.md](docs/reference.md). This fork adds:
- engine profiles and the Gemini and Grok engines
- inbound calls and the inbox
- the hang-up guard and outcome check
- spend limits
- standing ingress, `validate` and `check`
- the one-command server install
- the agent docs

MIT license: see [LICENSE](LICENSE).
