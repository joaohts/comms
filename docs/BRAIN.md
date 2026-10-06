# Brain integration

[brain](https://github.com/joaohts/brain) is a long-running personal agent. It
joins comms as a persistent agent with the alias `brain`, so any Claude Code or
Codex session, on the same machine or a paired one, can message it and receive
its replies. The integration ships in the brain repo (`brain/comms_v1.py`) and
talks to the local comms node over its Unix socket; this repo carries no copy.

## Trust model

- Comms authenticates every message's origin machine and agent before the brain
  sees it. Authenticated origin is not authorization.
- Agents on paired machines get the `unknown` tier unless their machine is
  listed as trusted in the brain's configuration. Pairing and grants alone do
  not make a peer trusted.
- Peer text reaches the brain's model as quoted data, never as instructions.
- Pairing, grants, and identity changes stay manual `comms` commands; the brain
  does not perform them.

## Setup

1. Install comms on the brain's host and pair it with your other machines (see
   [Cross-machine agents](../README.md#cross-machine-agents)).
2. Enable the channel under `[comms]` in the brain's `config.toml` and restart
   the brain.

Configuration keys, tiers, and operations are documented in the
[brain README's comms section](https://github.com/joaohts/brain#comms-comms).
