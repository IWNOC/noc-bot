# noc-bot

An operator that watches the event network's logs, alerts in Slack, and learns from the people who
answer it. It does what operators did by staring at logs and thinking "huh, that's strange".

```
rsyslog ──▶ noc-bot (Go)
             ├─ templates (Drain) + counts against normal ── rules ──▶ incidents ──▶ Slack
             ├─ new patterns ─────────▶ laya-svc "line" desk
             ├─ every 5 min summary ──▶ laya-svc "window" desk
             ├─ opened incidents ─────▶ local LLM note (kept apart from facts) + "incident" desk
             └─ /v1/teacher ◀── laya-svc's Escalator asks when laya is unsure
                                 (exact answers from config, else the LLM with tools and evidence)
```

* **Rules first.** Silence, bursts, clock skew and known issues are fixed rules. They keep working when
  laya or the LLM is down.
* **laya reads words.** `internal/statebuilder` writes plain English with rates as words
  ("FAR ABOVE NORMAL"). On the event's own logs this turned laya from confidently wrong to right or
  honestly unsure. Changing the wording means re-running the evaluation; bump `statebuilder.Version`.
* **Facts and opinions stay apart.** Slack messages print exact values from code; the LLM's note is a
  separate, marked section.
* **Every decision is kept.** `store` records each text laya read, its answers, and every label from
  people, outcomes and the teacher. That is the training and evaluation data the event produces.

## Licence note (important)

laya itself is Apache-2.0. laya-svc's learning (`laya.Learner`, `laya.Escalator`) comes from
**vegaml-learn, PolyForm Noncommercial 1.0.0**. noc-bot is a noncommercial project, so it uses it.
If noc-bot is ever used commercially (sold, run as a paid service, put in a company product, delivered
to a sponsor), get a commercial licence from https://embedding.in/licence or run laya-svc with
`LAYA_SVC_LEARNING=off`.

## Parts

| Path | What |
| --- | --- |
| `cmd/noc-bot` | the Go service |
| `internal/` | ingest, drain, stats, rules, incident, statebuilder, laya, llm, teacher, notify, store, bot |
| `laya-svc/` | Python: one laya Learner + Escalator per desk, HTTP API (`laya-svc/laya_svc/app.py`) |
| `laya-svc/desks/` | the six desks' questions (line, window, incident, comment, action, helpdesk) |
| `deploy/rsyslog-forward.conf` | rsyslog snippet that forwards event lines to noc-bot without ever blocking |
| `noc-bot.example.yaml` | configuration: senders, teams, expected senders, known issues |

## Run

```sh
go build ./cmd/noc-bot
LLM_TOKEN=... LAYA_SVC_TOKEN=... SLACK_WEBHOOK_URL=... NOC_BOT_TEACHER_TOKEN=... ./noc-bot -config noc-bot.yaml
```

laya-svc (on the machine with the GPU, next to the local LLM):

```sh
cd laya-svc
uv venv && uv pip install -e '.[learn]'
LAYA_SVC_TOKEN=... LAYA_SVC_TEACHER_URL=http://<noc-bot>:8080/v1/teacher LAYA_SVC_TEACHER_TOKEN=... \
LAYA_SVC_DESKS=./desks LAYA_SVC_WORKDIR=./state LAYA_SVC_HOST=0.0.0.0 python -m laya_svc
```

Desks start in **shadow mode**: the teacher answers every question and laya is measured against it,
so `GET /v1/desks/<desk>/report` shows how often laya would have been right before it answers alone.

## Not done yet

* Slack App (Socket Mode): buttons, thread comments, the comment desk, and chat-based team setup.
  Today noc-bot posts with a webhook or a bot token only.
* noc-query on the syslog server (trace a user, grep the archive) as LLM tools.
* Active probes (DNS, DHCP, RADIUS) as outcome labels.
* Kubernetes manifests in iwnoc2026-cluster (`template/config/kubernetes/apps/noc-ai/`).
