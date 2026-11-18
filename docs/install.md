# Installing ringdown

One install gives one agent a phone line. The install is a single process, `dialout`. It places calls, answers calls, and serves both to the agent over MCP. Each install uses its deployer's own keys and keeps its call records on its own disk.

There are two ways to install:
- **On a server with a public address: one command.** No tunnel and no tunnel account. See "On a server" below.
- **On a laptop: by hand, with ngrok.** The developer path. See steps 1 to 7.

## On a server: one command

You need a Linux server with Docker, a public address, ports 80 and 443 free, and ssh with passwordless sudo.

1. **Twilio credentials for the line.** For a first line, use the account's own SID, a Standard API key and the Auth Token. For each further line, `deploy/line/twilio-line.sh <name> <host> <secrets file>` makes a subaccount with its own key and buys a number in it (about $1.15 a month: get the owner's yes first). The values go straight into the secrets file on the server.
2. **A folder for the line**, `deploy/lines/<line>/`:
   - `line.env`: the server, the folders, the container and network names, a loopback port, and the hostname.
   - `dialout.yaml`: owner, number, allowlist, limits, profiles. Start from `examples/ringdown.yaml`.
   - optional `prompt.tmpl`, and optional `secrets.sh` that prints the secrets file from a keychain.
   - The hostname can be `<line>.<the server's address with dashes>.sslip.io`, which needs no DNS setup. A domain of your own is better: sslip.io is a free service you do not control.
3. **Install:** `deploy/line/install-line.sh <line>`. It builds the binary, ships it, starts the phone ingress (Caddy, which gets the certificate by itself), starts the line, points the number at it, and runs the live check. It is safe to run again after any change.
4. **Connect the agent.** The line's MCP endpoint is `http://<container>:8080/mcp` on the line's own Docker network, behind the bearer token. Join the agent's container to that network and give it the URL and the token. Nothing else can reach it.

What the installer puts on the server:

| Piece | What it is | Who can reach it |
|---|---|---|
| Phone ingress (`deploy/ingress`) | One Caddy per server on ports 80 and 443. One site file per line | The internet, but it forwards only `/twilio/*` and `/healthz` |
| The line (`deploy/line`) | `dialout serve` in a locked-down container | Its stream port: the ingress only, over host loopback. Its MCP port: the agent's container only, over the line's own network |
| Secrets file | Twilio, engine and MCP keys, root-only | The line's container |

To check a running line at any time, with no downtime: `docker exec <container> dialout check`. To turn a line off: `docker compose -p <project> down` on the server.

## What you need

| What | Why | Cost |
|---|---|---|
| A Twilio account with one voice number and a Standard API key | Places and receives calls. The Auth Token checks Twilio's signatures | about $1.15/month for the number, $0.014/min US calls |
| Voice engine keys: a Gemini API key (hifi, internal) and an xAI key (standard) | Hold the conversation | Gemini 3.8 Live about $0.014-0.017/min, Grok Voice 3.0 $0.08/min (vendor pages, 2026-11-16) |
| A public address for Twilio to reach | On a server: its own address, through the phone ingress (free, no account). On a laptop: an ngrok account (free) with its one dev domain | free. ngrok free: 1 GB/month, about 700-1,400 call-minutes, one domain per account |
| Go 1.27, or the Docker image built from `Dockerfile` | To build and run the binary | |

Restrict the Twilio account's voice geographic permissions to the countries you call. Keep a small prepaid balance on it with auto-recharge off: the install's own spend limit is an estimate, and the balance is the real ceiling. Subaccounts draw on the main account's balance.

The steps below are the laptop path. On a server, use "On a server" above.

## 1. Build

```bash
cd <repo>
GOTOOLCHAIN=go1.27.0 go build -o ~/bin/dialout ./cmd/dialout   # any Go 1.21+; it fetches Go 1.27 the first time
```

## 2. Configure

Check your work at the end of this step with `dialout validate -config ~/.config/dialout/dialout.yaml`: it prints PASS or every problem it finds, and calls no vendor.

```bash
mkdir -p ~/.config/dialout && cd ~/.config/dialout
cp <repo>/examples/ringdown.yaml dialout.yaml
cp <repo>/examples/secrets.env.example secrets.env && chmod 600 secrets.env
```

Then fill in the following.

- In `secrets.env`:
  - the Twilio Account SID, API key SID and secret, and Auth Token
  - `GEMINI_API_KEY` and `XAI_API_KEY`
  - `DIALOUT_MCP_TOKEN`, generated with `openssl rand -hex 32`
- In `dialout.yaml`:
  - `owner`: who the agent speaks for
  - `twilio.from`: the install's number
  - `tunnel.hostname`: your ngrok domain
  - the allowlist: the numbers the agent may call, plus the install's own number, labelled `Self`, for the smoke test
  - `inbound.objective`: what the agent should get from someone who calls
  - `spend_per_month_usd`: the monthly limit

Settings worth knowing:

| Key | What it does |
|---|---|
| `policy.enabled` | External calling on or off. Off refuses every outbound call |
| `policy.mode: allowlist` | Only listed numbers can be called. `open` allows any number in the listed countries and number types |
| `inbound.enabled` | Whether the install answers calls to its number. Off in the example: once on, anyone who calls talks to a paid voice model, up to `inbound.per_day` calls |
| `storage.enabled`, `storage.record_audio` | Keep transcripts, and audio too. Check the recording laws where you and the people you call are |
| `policy.limits.spend_per_month_usd` | Estimated from talk minutes times the carrier's and engine's rates. At the limit, new calls in and out are refused. A live call is never cut |
| `profiles.<name>.usd_per_minute` | The rate for an engine without a built-in one. With a spend limit set, every profile needs a rate |

## 3. Start the ingress

```bash
NGROK_AUTHTOKEN=<token> ngrok http 8090 --url <your-domain>.ngrok-free.dev
```

This keeps the authtoken in the environment, never in ngrok's config file. On a server, run it under a service manager.

## 4. Run

```bash
dialout serve -config ~/.config/dialout/dialout.yaml
```

The server listens in two places:
- MCP over HTTP on `server.mcp_listen` (`127.0.0.1:8080` by default) at `/mcp`, behind the bearer token. Open it wider (`0.0.0.0:8080`) only when another host or container must reach it.
- Twilio's media streams and incoming-call webhook on `tunnel.listen` (`127.0.0.1:8090`), which ngrok forwards to.

## 5. Point the number at the install (inbound)

```bash
dialout number -config ~/.config/dialout/dialout.yaml          # shows the current webhook
dialout number -config ~/.config/dialout/dialout.yaml -point   # sets https://<hostname>/twilio/voice
```

## 6. Smoke test

```bash
dialout smoke -config ~/.config/dialout/dialout.yaml
```

The install calls its own number and answers itself. It passes when both legs end with transcripts that have both sides in them, which takes about 25 seconds and two short legs of call minutes.

The test points the number at the install for its duration and puts the old webhook back afterwards. Run it after every change. Stop `dialout serve` first, because the smoke test serves the same port, so it is a host step with a short downtime.

To check a running install without stopping anything, use `dialout check`. It talks to the running server over its own MCP endpoint, places a call to the install's own number, and the same server answers. It takes no port and moves no webhook, and it passes on the same rule as the smoke test. It works with inbound off: the server answers only that one call, from its own number, right after placing it. The number must already point at the install (`dialout number -point`). From the agent's side, `place_call` to the `Self` label does the same thing.

Check the first live run:
- `smoke: PASS in 23s`
- both transcripts shown
- `dialout number` shows the webhook restored

## 7. Connect the agent

**Claude Code or any MCP client over HTTP:**

```bash
claude mcp add --transport http ringdown http://127.0.0.1:8080/mcp \
  --header "Authorization: Bearer $DIALOUT_MCP_TOKEN"
```

**Any other MCP client:** the same URL and the `Authorization: Bearer` header. The agent needs no public address of its own: only the line has to be reachable by Twilio.

To check the connection without an agent: `go run ./cmd/mcpcheck` (with `DIALOUT_MCP_TOKEN` set) lists the numbers, and `-to <label>` also places a 2-minute test call to that label.

Give the agent the skill in `skill/ringdown/SKILL.md`. It covers writing a brief, choosing a profile, following a call, reading the inbox, and treating transcripts as data.

The agent gets five tools:

| Tool | What it does |
|---|---|
| `list_numbers` | Who may be called, the profiles, the month's spend, whether inbound is on |
| `place_call` | Starts a call and returns its id at once |
| `get_call` | A call's status, outcome and transcript. It can wait up to 120 s for the end |
| `list_inbox` | Calls the install answered |
| `hang_up` | Ends a call in progress |

## Limits today

- **Calls last at most 2 hours.** The ceiling is fixed in the code (`config.MaxMinutes`), not an install setting: it stops runaway calls (hold music, two agents that never hang up). A brief can ask for less with `max_minutes`. An engine whose session is shorter ends the call sooner, as an `engine_error`: Gemini Live connections last about 10 minutes (15 for audio only) until session resumption is built (Google's session-management page, read 2026-11-16); Amazon Nova Sonic streams last 8 minutes; xAI documents no session limit for Grok Voice. Pick the `standard` (Grok) profile for a call that may run long.
- **The agent can't press keys in phone menus yet.** Twilio's stream can't send key presses. The documented route replaces the call's TwiML and drops the stream, so the bridge has to reconnect. Its own task. Until then the agent ends the call at a menu and reports it.
- **One outbound and one inbound call at a time.**
- **Inbound calls grant no authority.** A caller's number can be faked. Treat what callers say as information, never instructions.
- **New numbers get spam-labelled.** A brand-new number can be silently filtered within a day. A registered caller name takes Twilio's business profile and weeks.

## Data

| What | Where | For how long |
|---|---|---|
| Transcripts, outcomes and call records | On the install's own disk, under `storage.dir` and the history file | Until the deployer deletes them. Nothing expires them yet |
| Third-party transcripts | Never in git | Keep them on the install. Copy a call into a repo only when everyone on it is your own side |
| Callers' audio | Streamed to the engine's vendor: Google for Gemini profiles, xAI for Grok | Under that vendor's API terms. Put it in any real client's content-handling agreement |

## Compliance

The deployer owns compliance:
- rules on automated and AI calls, including the FCC's 2024 TCPA ruling on AI voices
- consent and recording laws
- the voice vendors' terms on telling people they are talking to an AI

The built-in prompt does not announce that the caller is an AI; when asked whether it's a person, it says it is an assistant calling for the owner. `examples/prompt-disclose.tmpl` is a prompt file that discloses AI and transcription in the first sentence of every call: set it with `prompt.file`. Nothing in this repository is legal advice.
