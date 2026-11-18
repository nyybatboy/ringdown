# Codes the server returns, and what to do

Every value below comes from the code (`internal/policy/policy.go`, `internal/server/server.go`, `internal/call/`, `internal/voice/tools.go`). If you see a code that is not here, treat it as a failure and report it with the call record.

## `place_call` refusals

A refused call never rings anyone. The error text is `refused (<code>): <detail>`.

| Code | Meaning | What to do |
|---|---|---|
| `disabled` | The line's external switch is off (`policy.enabled: false`) | Report it. Only the owner turns it on |
| `maintenance` | The policy source marked the line as under maintenance | Report it; try later |
| `unsupported_mode` | `policy.mode` is not `allowlist` or `open` | Configuration error: fix the config, then `dialout validate` |
| `not_allowlisted` | `to` is not on the allowlist | Use a label from `list_numbers`, or ask the owner to add the number |
| `invalid_number` | `to` is not a label and not a dialable number | Use a label, or international form (`+1 212 555 0123`) |
| `country_not_allowed` | The number is in a country not in `policy.countries` | Report it. Do not reformat the number to get around it |
| `number_type_not_allowed` | The number's type (e.g. premium, toll-free) is not allowed | Report it |
| `blocked` | The number is on the blocklist | Report it |
| `do_not_call` | That person asked, on an earlier call, not to be called again | Do not call. Only the owner can clear it |
| `spend_limit` | The month's estimated spend reached `policy.limits.spend_per_month_usd` | Report it with the amount. A live call is never cut by this |
| `call_in_progress` | The line is already on an outbound call | Wait for that call (`get_call` with `wait_seconds`) then retry |
| `hourly_limit`, `daily_limit` | Rate limit reached | Report it; retry after the window |
| `number_daily_limit` | Open mode: this number was already called the allowed times today | Retry tomorrow, or ask the owner |
| `outside_hours` | Open mode: outside `policy.hours` | Retry inside the calling hours shown by `list_numbers` |
| `invalid_objective`, `invalid_context`, `invalid_limits`, `invalid_opening`, `invalid_callee` | A brief field is empty when required, or longer than the line allows | Shorten it; the limits are in the config's `policy.limits.*_chars` |
| `invalid_kind` | `kind` is not `business` or `personal` | Use one of those, or leave it out |
| `invalid_duration` | `max_minutes` is negative or more than 120 | Leave it out, or pass 1 to 120 |

Retrying with the same `request_id` within an hour returns the call already placed instead of placing another. Use it whenever a retry might duplicate a call.

## Call status (`get_call`)

`queued` → `ringing` → `in-progress` → one of these final states:

| Status | Meaning |
|---|---|
| `completed` | The call connected and ended. Read `outcome` and the transcript |
| `no-answer`, `busy` | Nobody picked up, or the line was busy. Ask before retrying |
| `failed` | It could not be placed or broke before the conversation; `error` says why |
| `canceled` | `hang_up` was called before or during the call |
| `interrupted` | The server stopped mid-call (restart, crash). The transcript up to that point is kept |

## End reasons (`end_reason` on a completed call)

| Reason | Meaning | Trust the outcome? |
|---|---|---|
| `end_call` | The agent ended it normally after the goodbye | Yes, after reading the transcript |
| `end_call_without_goodbye`, `end_call_goodbye_too_long` | The agent ended it, but the close was abrupt or ran long | Read the transcript's last turns |
| `outcome_without_end_call` | The agent reported an outcome and the line went quiet, so the server closed it | Usually yes |
| `agent_closed` | The engine closed the conversation itself | Read the transcript |
| `callee_hung_up` | The other person hung up | Check whether the objective was met before they did |
| `hung_up_on_request` | Your `hang_up` | n/a |
| `time_limit` | The call reached its maximum length | Probably unfinished: read the transcript |
| `watchdog_hang_up` | The optional watchdog judged the call should end | Read the transcript and the watchdog reason |
| `engine_error` | The voice engine dropped the session (e.g. Gemini's ~10-minute limit, or a transient 1011 error) | Treat a call under about 20 seconds, or any `engine_error`, as not connected; the person may have been cut off mid-sentence |
| `stream_closed: …` | Twilio's media stream broke | Treat as not connected |
| `no_stream` | Twilio never opened the media stream (often the public address is wrong) | Run `dialout check`; check `tunnel.hostname` |
| `canceled` | Canceled before it connected | n/a |

## Outcomes (`outcome.result`)

The voice agent reports one of these. It is a hint: judge from the transcript.

| Result | Meaning |
|---|---|
| `success` | The agent believes the objective was met |
| `partial` | Part of it was met, or a yes never came. `outcome.checked: true` means the server downgraded a reported success because the agent's last question was never answered |
| `failed` | The objective was not met |
| `voicemail` | Reached voicemail or an automated menu; the agent hangs up by default |
| `wrong_number` | The person said it was the wrong number |
| `do_not_call` | The person asked not to be called again. Further calls to that number are refused |
