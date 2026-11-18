# Example line

Copy this folder to `deploy/lines/<name>/`, then:
1. Edit `line.env` (where the line runs).
2. Put a `dialout.yaml` next to it, starting from `examples/ringdown.yaml`. Its `tunnel.hostname` must equal `LINE_HOSTNAME`, with `tunnel.standing: true` and `tunnel.listen: 0.0.0.0:8090`, and `server.mcp_listen: 0.0.0.0:8080`.
3. Optional: `prompt.tmpl` (for example `examples/prompt-disclose.tmpl`), and an executable `secrets.sh` that prints the secrets file on stdout. Without `secrets.sh`, put the secrets file on the server yourself at `LINE_SECRETS` (root, mode 600).
4. Run `deploy/line/install-line.sh <name>`. It ends with `check: PASS`.
