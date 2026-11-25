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
| `noc-bot.example.yaml` | configuration: only where things are (listeners, laya, LLM, Slack, store) |

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

## Slack: a colleague that learns the site by talking

noc-bot starts knowing nothing about the site. The config file only says where things are
(`noc-bot.example.yaml`); teams, who to mention, what devices are, known issues and what is normal
here are learned in Slack and kept in the store.

* **Alerts are written by the LLM**: a headline and a note as long as the situation needs, using what
  operators taught it. Every number, address and time in that text must appear in the evidence it was
  given (or found with its tools); otherwise the text is dropped and the plain fact list is posted.
  Mentions, team and severity are added by code. Raw logs go to the thread as a folded snippet
  (`files:write`), or a code block if the app lacks that scope.
* **Conversation**: every `@noc-bot` and every reply in a thread noc-bot posted in goes to one agent,
  with the thread's history. It looks things up with read-only tools (logs, incidents, its own settings
  and memory, Slack user groups) and answers.
* **Learning by proposal**: to remember something, or to change how it alerts (who owns what, known
  issues, verdicts on an alert), the agent posts a card; nothing takes effect until a person presses 登録.
* **Buttons and emoji** on alerts (✅ ❌ 🔼 🔽, 担当違う) are human labels for laya.
* laya is asked about **network areas** (physical, l2l3, server, auth, wireless), which do not change
  between events. Which team owns an area is site knowledge, set up in conversation.

Create the app from `deploy/slack-app-manifest.yaml`, install it, invite `@noc-bot` to the alert
channel, and set `SLACK_BOT_TOKEN`, `SLACK_APP_TOKEN` and `slack.channel`.

## Not done yet

* noc-query on the syslog server (trace a user, grep the archive) as LLM tools.
* Active probes (DNS, DHCP, RADIUS) as outcome labels.
* Kubernetes manifests in iwnoc2026-cluster (`template/config/kubernetes/apps/noc-ai/`).
